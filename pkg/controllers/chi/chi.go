// Copyright 2019 Altinity Ltd and/or its affiliates. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package chi

import (
	"context"
	"fmt"
	"time"

	chop "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	chopmetrics "github.com/altinity/clickhouse-operator/pkg/apis/metrics"
	chopclientset "github.com/altinity/clickhouse-operator/pkg/client/clientset/versioned"
	chopclientsetscheme "github.com/altinity/clickhouse-operator/pkg/client/clientset/versioned/scheme"
	chopinformers "github.com/altinity/clickhouse-operator/pkg/client/informers/externalversions/clickhouse.altinity.com/v1"
	choplisters "github.com/altinity/clickhouse-operator/pkg/client/listers/clickhouse.altinity.com/v1"
	chopparser "github.com/altinity/clickhouse-operator/pkg/parser"

	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	labels "k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	wait "k8s.io/apimachinery/pkg/util/wait"

	appsinformers "k8s.io/client-go/informers/apps/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	kube "k8s.io/client-go/kubernetes"
	scheme "k8s.io/client-go/kubernetes/scheme"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	cache "k8s.io/client-go/tools/cache"
	record "k8s.io/client-go/tools/record"
	workqueue "k8s.io/client-go/util/workqueue"

	"github.com/golang/glog"
)

// Controller defines CRO controller
type Controller struct {
	kubeClient              kube.Interface
	chopClient              chopclientset.Interface
	chopLister              choplisters.ClickHouseInstallationLister
	chopListerSynced        cache.InformerSynced
	statefulSetLister       appslisters.StatefulSetLister
	statefulSetListerSynced cache.InformerSynced
	configMapLister         corelisters.ConfigMapLister
	configMapListerSynced   cache.InformerSynced
	serviceLister           corelisters.ServiceLister
	serviceListerSynced     cache.InformerSynced
	queue                   workqueue.RateLimitingInterface
	recorder                record.EventRecorder
	metricsExporter         *chopmetrics.Exporter
}

const (
	componentName = "clickhouse-operator"
	runWorkerPeriod = time.Second
)

const (
	successSynced         = "Synced"
	errResourceExists     = "ErrResourceExists"
	messageResourceSynced = "ClickHouseInstallation synced successfully"
	messageResourceExists = "Resource %q already exists and is not managed by ClickHouseInstallation"
	messageUnableToDecode = "Unable to decode object (invalid type)"
	messageUnableToSync   = "Unable to sync caches for %s controller"
)

// CreateController creates instance of Controller
func CreateController(
	chopClient chopclientset.Interface,
	kubeClient kube.Interface,
	chopInformer chopinformers.ClickHouseInstallationInformer,
	ssInformer appsinformers.StatefulSetInformer,
	cmInformer coreinformers.ConfigMapInformer,
	serviceInformer coreinformers.ServiceInformer,
	chopMetricsExporter *chopmetrics.Exporter,
) *Controller {

	// Initializations
	chopclientsetscheme.AddToScheme(scheme.Scheme)
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(
		&typedcore.EventSinkImpl{
			Interface: kubeClient.CoreV1().Events(""),
		},
	)
	recorder := eventBroadcaster.NewRecorder(
		scheme.Scheme,
		core.EventSource{
			Component: componentName,
		},
	)

	// Creating Controller instance
	controller := &Controller{
		kubeClient:              kubeClient,
		chopClient:              chopClient,
		chopLister:              chopInformer.Lister(),
		chopListerSynced:        chopInformer.Informer().HasSynced,
		statefulSetLister:       ssInformer.Lister(),
		statefulSetListerSynced: ssInformer.Informer().HasSynced,
		configMapLister:         cmInformer.Lister(),
		configMapListerSynced:   cmInformer.Informer().HasSynced,
		serviceLister:           serviceInformer.Lister(),
		serviceListerSynced:     serviceInformer.Informer().HasSynced,
		queue:                   workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "chi"),
		recorder:                recorder,
		metricsExporter:         chopMetricsExporter,
	}
	chopInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueObject,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueueObject(new)
		},
	})
	ssInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.handleObject,
		UpdateFunc: func(old, new interface{}) {
			newStatefulSet := new.(*apps.StatefulSet)
			oldStatefulSet := old.(*apps.StatefulSet)
			if newStatefulSet.ResourceVersion == oldStatefulSet.ResourceVersion {
				return
			}
			controller.handleObject(new)
		},
		DeleteFunc: controller.handleObject,
	})

	return controller
}

// Run syncs caches, starts workers
func (c *Controller) Run(ctx context.Context, threadiness int) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	glog.V(1).Info("Starting ClickHouseInstallation controller")
	if !waitForCacheSync(
		"ClickHouseInstallation",
		ctx.Done(),
		c.chopListerSynced,
		c.statefulSetListerSynced,
		c.configMapListerSynced,
		c.serviceListerSynced,
	) {
		// Unable to sync
		return
	}

	glog.V(1).Info("ClickHouseInstallation controller: starting workers")
	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, runWorkerPeriod, ctx.Done())
	}
	glog.V(1).Info("ClickHouseInstallation controller: workers started")
	defer glog.V(1).Info("ClickHouseInstallation controller: shutting down workers")
	<-ctx.Done()
}

// runWorker is a convenience wrap over processNextWorkItem()
func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem processes objects from the workqueue
func (c *Controller) processNextWorkItem() bool {
	item, shutdown := c.queue.Get()
	if shutdown {
		// Stop iteration
		return false
	}
	err := func(item interface{}) error {
		defer c.queue.Done(item)

		stringItem, ok := item.(string)
		if !ok {
			// Item is impossible to process, no more retries
			c.queue.Forget(item)
			utilruntime.HandleError(fmt.Errorf("unexpected item in the queue - %#v", item))
			return nil
		}

		if err := c.syncItem(stringItem); err != nil {
			// Item will be retried later
			return fmt.Errorf("unable to sync an object '%s': %s", stringItem, err.Error())
		}

		// Item is processed, no more retries
		c.queue.Forget(item)

		return nil
	}(item)
	if err != nil {
		utilruntime.HandleError(err)
	}

	// Continue iteration
	return true
}

// syncItem applies reconciliation actions to watched objects
func (c *Controller) syncItem(key string) error {
	// Extract namespace and name from key - action
	// opposite to <key, err := cache.MetaNamespaceKeyFunc(obj)> in enqueueObject function
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("incorrect resource key: %s", key))
		return nil
	}

	// Check CHI object in cache cache
	chi, err := c.chopLister.ClickHouseInstallations(namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			utilruntime.HandleError(fmt.Errorf("ClickHouseInstallation object '%s' no longer exists in the work queue", key))
			// Not found is not an error on higher level - return nil
			return nil
		}
		return err
	}

	// Check CHI object already in sync
	if chi.Status.ObjectPrefixes == nil || len(chi.Status.ObjectPrefixes) == 0 {
		prefixes, err := c.createControlledResources(chi)
		if err != nil {
			glog.V(2).Infof("ClickHouseInstallation (%q): unable to create controlled resources: %q", chi.Name, err)
			return err
		}
		if err := c.updateChiStatus(chi, prefixes); err != nil {
			glog.V(2).Infof("ClickHouseInstallation (%q): unable to update status of CHI resource: %q", chi.Name, err)
			return err
		}
		glog.V(2).Infof("ClickHouseInstallation (%q): controlled resources are synced (created): %v", chi.Name, prefixes)
	} else {
		// Check consistency of existent resources controlled by the CHI object

		// Number of prefixes - -which is number of Stateful Sets and number of Pods
		prefixesNum := len(chi.Status.ObjectPrefixes)
		// Pod hostnames of CH
		chHostnames := make([]string, prefixesNum)

		for i, prefix := range chi.Status.ObjectPrefixes {
			// Verify we have Stateful Set with such a name
			ssName := chopparser.CreateStatefulSetName(prefix)
			_, err := c.statefulSetLister.StatefulSets(chi.Namespace).Get(ssName)
			if err == nil {
				// TODO: check all controlled objects
				glog.V(2).Infof("ClickHouseInstallation (%q) controls StatefulSet: %q", chi.Name, ssName)

				// Prepare hostnames list for the chopmetrics.Exporter state storage
				chHostnames[i] = chopparser.CreatePodHostname(chi.Namespace, prefix)
			}
		}

		// Check hostnames of the Pods from current CHI object included into chopmetrics.Exporter state

		if !c.metricsExporter.ControlledValuesExist(chi.Name, chHostnames) {
			glog.V(2).Infof("ClickHouseInstallation (%q): including hostnames into chopmetrics.Exporter", chi.Name)
			c.metricsExporter.UpdateControlledState(chi.Name, chHostnames)
		}
	}

	return nil
}

// createControlledResources creates k8s resouces based on ClickHouseInstallation object specification
func (c *Controller) createControlledResources(chi *chop.ClickHouseInstallation) ([]string, error) {
	chiCopy := chi.DeepCopy()
	chiObjects, prefixes := chopparser.CreateObjects(chiCopy)
	for _, objList := range chiObjects {
		switch v := objList.(type) {
		case chopparser.ConfigMapList:
			for _, obj := range v {
				if err := c.createConfigMap(chiCopy, obj); err != nil {
					return nil, err
				}
			}
		case chopparser.ServiceList:
			for _, obj := range v {
				if err := c.createService(chiCopy, obj); err != nil {
					return nil, err
				}
			}
		case chopparser.StatefulSetList:
			for _, obj := range v {
				if err := c.createStatefulSet(chiCopy, obj); err != nil {
					return nil, err
				}
			}
		}
	}
	return prefixes, nil
}

// createConfigMap creates core.ConfigMap resource
func (c *Controller) createConfigMap(chi *chop.ClickHouseInstallation, newConfigMap *core.ConfigMap) error {
	res, err := c.configMapLister.ConfigMaps(chi.Namespace).Get(newConfigMap.Name)
	if res != nil {
		// ConfigMap with such name already exists
		return nil
	}

	if apierrors.IsNotFound(err) {
		// ConfigMap with such name not found - create it
		_, err = c.kubeClient.CoreV1().ConfigMaps(chi.Namespace).Create(newConfigMap)
	}
	if err != nil {
		return err
	}

	return nil
}

// createService creates core.Service resource
func (c *Controller) createService(chi *chop.ClickHouseInstallation, newService *core.Service) error {
	res, err := c.serviceLister.Services(chi.Namespace).Get(newService.Name)
	if res != nil {
		// Service with such name already exists
		return nil
	}

	if apierrors.IsNotFound(err) {
		// Service with such name not found - create it
		_, err = c.kubeClient.CoreV1().Services(chi.Namespace).Create(newService)
	}
	if err != nil {
		return err
	}

	return nil
}

// createStatefulSet creates apps.StatefulSet resource
func (c *Controller) createStatefulSet(chi *chop.ClickHouseInstallation, newStatefulSet *apps.StatefulSet) error {
	res, err := c.statefulSetLister.StatefulSets(chi.Namespace).Get(newStatefulSet.Name)
	if res != nil {
		// StatefulSet with such name already exists
		return nil
	}

	if apierrors.IsNotFound(err) {
		// StatefulSet with such name not found - create it
		_, err = c.kubeClient.AppsV1().StatefulSets(chi.Namespace).Create(newStatefulSet)
	}
	if err != nil {
		return err
	}

	return nil
}

// updateChiStatus updates .status section of ClickHouseInstallation resource
func (c *Controller) updateChiStatus(chi *chop.ClickHouseInstallation, objectPrefixes []string) error {
	chiCopy := chi.DeepCopy()
	chiCopy.Status = chop.ChiStatus{
		ObjectPrefixes: objectPrefixes,
	}
	_, err := c.chopClient.ClickhouseV1().ClickHouseInstallations(chi.Namespace).Update(chiCopy)

	return err
}

// enqueueObject adds ClickHouseInstallation object to the workqueue
func (c *Controller) enqueueObject(obj interface{}) {
	// Make a string key for an API object
	// The key uses the format <namespace>/<name>
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}

	c.queue.AddRateLimited(key)
}

// handleObject enqueues CHI which is owner of `obj` into reconcile loop
func (c *Controller) handleObject(obj interface{}) {
	object, ok := obj.(meta.Object)
	if !ok {
		ts, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf(messageUnableToDecode))
			return
		}
		object, ok = ts.Obj.(meta.Object)
		if !ok {
			utilruntime.HandleError(fmt.Errorf(messageUnableToDecode))
			return
		}
	}

	// object is an instance of meta.Object

	// Checking that we control current StatefulSet Object
	ownerRef := meta.GetControllerOf(object)
	if ownerRef == nil {
		// No owner
		return
	}

	// Ensure owner is of a propoer kind
	if ownerRef.Kind != chop.ClickHouseInstallationCRDResourceKind {
		return
	}

	glog.V(2).Infof("Processing object: %s", object.GetName())

	// Get owner - it is expected to be CHI
	chi, err := c.chopLister.ClickHouseInstallations(object.GetNamespace()).Get(ownerRef.Name)

	if err != nil {
		glog.V(2).Infof("ignoring orphaned object '%s' of ClickHouseInstallation '%s'", object.GetSelfLink(), ownerRef.Name)
		return
	}

	// Add CHI object into reconcile loop
	c.enqueueObject(chi)
}

// waitForCacheSync syncs informers cache
func waitForCacheSync(n string, ch <-chan struct{}, syncs ...cache.InformerSynced) bool {
	glog.V(1).Infof("Syncing caches for %s controller", n)
	if !cache.WaitForCacheSync(ch, syncs...) {
		utilruntime.HandleError(fmt.Errorf(messageUnableToSync, n))
		return false
	}
	glog.V(1).Infof("Caches are synced for %s controller", n)
	return true
}

// clusterWideSelector returns labels.Selector object
func clusterWideSelector(name string) labels.Selector {
	return labels.SelectorFromSet(labels.Set{
		chopparser.ClusterwideLabel: name,
	})
	/*
		glog.V(2).Infof("ClickHouseInstallation (%q) listing controlled resources", chi.Name)
		ssList, err := c.statefulSetLister.StatefulSets(chi.Namespace).List(clusterWideSelector(chi.Name))
		if err != nil {
			return err
		}
		// Listing controlled resources
		for i := range ssList {
			glog.V(2).Infof("ClickHouseInstallation (%q) controlls StatefulSet: %q", chi.Name, ssList[i].Name)
		}
	*/
}
