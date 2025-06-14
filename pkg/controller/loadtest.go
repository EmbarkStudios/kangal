package controller

import (
	"context"
	"fmt"
	"maps"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"go.uber.org/zap"
	batchV1 "k8s.io/api/batch/v1"
	coreV1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilRuntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedCoreV1 "k8s.io/client-go/kubernetes/typed/core/v1"
	coreListersV1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	"github.com/hellofresh/kangal/pkg/backends"
	loadTestV1 "github.com/hellofresh/kangal/pkg/kubernetes/apis/loadtest/v1"
	clientSetV "github.com/hellofresh/kangal/pkg/kubernetes/generated/clientset/versioned"
	sampleScheme "github.com/hellofresh/kangal/pkg/kubernetes/generated/clientset/versioned/scheme"
	"github.com/hellofresh/kangal/pkg/kubernetes/generated/informers/externalversions"
	listers "github.com/hellofresh/kangal/pkg/kubernetes/generated/listers/loadtest/v1"
)

const (
	controllerAgentName = "kangal"
	falseString         = "false"
	trueString          = "true"
)

// MetricsReporter used to interface with the metrics configurations
type MetricsReporter struct {
	workQueueDepthStat   metric.Int64UpDownCounter
	reconcileCountStat   metric.Int64UpDownCounter
	reconcileLatencyStat metric.Int64Histogram
}

// NewMetricsReporter contains loadtest metrics definition
func NewMetricsReporter(meter metric.Meter) (*MetricsReporter, error) {
	workQueueDepthStat, err := meter.Int64UpDownCounter(
		"kangal_work_queue_depth",
		metric.WithDescription("Depth of the work queue"),
	)
	if err != nil {
		return nil, fmt.Errorf("could not register workQueueDepthStat metric: %w", err)
	}

	reconcileCountStat, err := meter.Int64UpDownCounter(
		"kangal_reconcile_count",
		metric.WithDescription("Number of reconcile operations"),
	)
	if err != nil {
		return nil, fmt.Errorf("could not register reconcileCountStat metric: %w", err)
	}

	reconcileLatencyStat, err := meter.Int64Histogram(
		"kangal_reconcile_latency",
		metric.WithDescription("Latency of reconcile operations"),
		metric.WithUnit("ms"),
	)
	if err != nil {
		return nil, fmt.Errorf("could not register reconcileLatencyStat metric: %w", err)
	}

	return &MetricsReporter{
		workQueueDepthStat:   workQueueDepthStat,
		reconcileCountStat:   reconcileCountStat,
		reconcileLatencyStat: reconcileLatencyStat,
	}, nil
}

// Controller is the controller implementation for LoadTest resources
type Controller struct {
	cfg             Config
	kubeClientSet   kubernetes.Interface
	kangalClientSet clientSetV.Interface

	namespacesLister coreListersV1.NamespaceLister
	namespacesSynced cache.InformerSynced

	podsLister coreListersV1.PodLister
	podsSynced cache.InformerSynced

	loadtestsLister listers.LoadTestLister
	loadtestsSynced cache.InformerSynced

	// workQueue is a rate limited work queue. This is used to queue work to be
	// processed instead of performing it as soon as a change happens. This
	// means we can ensure we only process a fixed amount of resources at a
	// time, and makes it easy to ensure we are never processing the same item
	// simultaneously in two different workers.
	workQueue workqueue.RateLimitingInterface
	// recorder is an event recorder for recording Event resources to the
	// Kubernetes API.
	recorder record.EventRecorder

	statsClient MetricsReporter

	registry backends.Registry
	logger   *zap.Logger
}

// NewController returns a new sample controller
func NewController(
	cfg Config,
	kubeClientSet kubernetes.Interface,
	kangalClientSet clientSetV.Interface,
	kubeInformerFactory informers.SharedInformerFactory,
	kangalInformerFactory externalversions.SharedInformerFactory,
	statsClient MetricsReporter,
	registry backends.Registry,
	logger *zap.Logger,
) *Controller {
	namespaceInformer := kubeInformerFactory.Core().V1().Namespaces()
	podInformer := kubeInformerFactory.Core().V1().Pods()
	jobInformer := kubeInformerFactory.Batch().V1().Jobs()

	loadTestInformer := kangalInformerFactory.Kangal().V1().LoadTests()

	// Create event broadcaster
	// Add sample-controller types to the default Kubernetes Scheme so Events can be
	// logged for sample-controller types.
	utilRuntime.Must(sampleScheme.AddToScheme(scheme.Scheme))
	logger.Debug("Creating event broadcaster")
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(func(format string, args ...interface{}) {
		logger.Info(fmt.Sprintf(format, args...))
	})
	eventBroadcaster.StartRecordingToSink(&typedCoreV1.EventSinkImpl{Interface: kubeClientSet.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, coreV1.EventSource{Component: controllerAgentName})

	controller := &Controller{
		cfg: cfg,

		kubeClientSet:   kubeClientSet,
		kangalClientSet: kangalClientSet,

		namespacesLister: namespaceInformer.Lister(),
		namespacesSynced: namespaceInformer.Informer().HasSynced,

		podsLister: podInformer.Lister(),
		podsSynced: podInformer.Informer().HasSynced,

		loadtestsLister: loadTestInformer.Lister(),
		loadtestsSynced: loadTestInformer.Informer().HasSynced,

		workQueue:   workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "LoadTest"),
		recorder:    recorder,
		statsClient: statsClient,

		registry: registry,
		logger:   logger,
	}

	logger.Debug("Setting up event handlers")

	// Set up an event handler for when a LoadTest resources is added
	loadTestInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueueLoadTest,
		UpdateFunc: func(_, new interface{}) {
			controller.enqueueLoadTest(new)
		},
	})

	jobInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.handleObject,
		UpdateFunc: func(old, new interface{}) {
			newJob := new.(*batchV1.Job)
			oldJob := old.(*batchV1.Job)

			if newJob.ResourceVersion == oldJob.ResourceVersion {
				return
			}
			controller.handleObject(new)
		},
	})

	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.handleObject,
		UpdateFunc: func(old, new interface{}) {
			newPod := new.(*coreV1.Pod)
			oldPod := old.(*coreV1.Pod)

			if newPod.ResourceVersion == oldPod.ResourceVersion {
				// Periodic resync will send update events for all known Jobs.
				// Two different versions of the same Job will always have different RVs.
				return
			}
			controller.handleObject(new)
		},
	})

	return controller
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it will shutdown the workQueue and wait for
// workers to finish processing their current work items.
func (c *Controller) Run(numThreads int, stopCh <-chan struct{}) error {
	defer utilRuntime.HandleCrash()
	defer c.workQueue.ShutDown()

	// Start the informer factories to begin populating the informer caches
	c.logger.Info("Starting loadtest controller")

	// Wait for the caches to be synced before starting workers
	c.logger.Debug("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.namespacesSynced, c.podsSynced, c.loadtestsSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	c.logger.Debug("Starting workers")
	// Launch numThreads number of threads to process LoadTest resources
	for i := 0; i < numThreads; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	c.logger.Debug("Started workers")
	<-stopCh
	c.logger.Debug("Shutting down workers")

	return nil
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workQueue.
func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workQueue and
// attempt to process it, by calling the syncHandler.
func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workQueue.Get()

	if shutdown {
		return false
	}

	// Send the metrics for the current queue depth
	c.statsClient.workQueueDepthStat.Add(context.Background(), int64(c.workQueue.Len()))

	// We wrap this block in a func, so we can defer c.workQueue.Done.
	err := func(obj interface{}) error {
		startTime := time.Now()

		// We call Done here so the workQueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the workQueue and attempted again after a back-off
		// period.
		defer c.workQueue.Done(obj)
		var key string
		var ok bool

		var err error
		defer func() {
			status := trueString
			if err != nil {
				status = falseString
			}

			c.statsClient.reconcileCountStat.Add(context.Background(), 1, metric.WithAttributes(attribute.String("key", key), attribute.String("success", status)))
			c.statsClient.reconcileLatencyStat.Record(context.Background(), int64(time.Since(startTime)/time.Millisecond), metric.WithAttributes(attribute.String("key", key), attribute.String("success", status)))
		}()

		// We expect strings to come off the workQueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workQueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put onto the
		// workQueue.
		if key, ok = obj.(string); !ok {
			// As the item in the workQueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workQueue.Forget(obj)
			utilRuntime.HandleError(fmt.Errorf("expected string in workQueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// LoadTest resource to be synced.
		if err := c.syncHandler(key); err != nil {
			// Put the item back on the workQueue to handle any transient errors.
			c.workQueue.AddRateLimited(key)
			c.logger.Error("error syncing loadtest, re-queuing", zap.String("loadtest", key), zap.Error(err))
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workQueue.Forget(obj)
		c.logger.Debug("Successfully synced", zap.String("loadtest", key))
		return nil
	}(obj)
	if err != nil {
		utilRuntime.HandleError(err)
		return true
	}

	return true
}

// syncHandler compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the LoadTest resource
// with the current status of the resource.
func (c *Controller) syncHandler(key string) error {
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.SyncHandlerTimeout)
	defer cancel()

	logger := c.logger.With(
		zap.String("loadtest", key),
	)

	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		utilRuntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	loadTestFromCache, err := c.loadtestsLister.Get(name)
	if err != nil {
		// The LoadTest resource may no longer exist, in which case we stop
		// processing.
		if errors.IsNotFound(err) {
			utilRuntime.HandleError(fmt.Errorf("loadtest '%s' in work queue no longer exists", key))
			return nil
		}

		// The LoadTest resource may be conflicted, in which case we stop
		// processing.
		if errors.IsConflict(err) {
			utilRuntime.HandleError(fmt.Errorf("there is a conflict with loadtest '%s' between datastore and cache. it might be because object has been removed or modified in the datastore", key))
			return nil
		}
		return err
	}
	// copy object before mutate it
	loadTest := loadTestFromCache.DeepCopy()

	// get report url
	var reportURL string
	if c.cfg.KangalProxyURL != "" {
		reportURL = fmt.Sprintf("%s/load-test/%s/report", c.cfg.KangalProxyURL, loadTest.GetName())
	}

	// get backend
	backend, err := c.registry.GetBackend(loadTest.Spec.Type)
	if err != nil {
		return fmt.Errorf("failed to resolve backend: %w", err)
	}

	// ensure that status is updated if any of the following fails
	defer c.updateLoadTestStatus(ctx, key, loadTest, loadTestFromCache)

	// check or create namespace
	err = c.checkOrCreateNamespace(ctx, loadTest)
	if err != nil {
		return err
	}

	// sync backend resources
	err = backend.Sync(ctx, *loadTest, reportURL)
	if err != nil {
		return err
	}

	// sync backend status
	err = backend.SyncStatus(ctx, *loadTest, &loadTest.Status)
	if err != nil {
		return err
	}

	// check and delete stale finished/errored loadtests
	if c.cfg.CleanUpThreshold != 0 && checkLoadTestLifeTimeExceeded(loadTest, c.cfg.CleanUpThreshold) {
		logger.Info("Deleting loadtest due to exceeded lifetime",
			zap.String("phase", loadTest.Status.Phase.String()),
		)
		c.deleteLoadTest(ctx, key, loadTest)
	}

	return nil
}

// handleObject will take any resource implementing metaV1.Object and attempt
// to find the LoadTest resource that 'owns' it. It does this by looking at the
// objects metadata.ownerReferences field for an appropriate OwnerReference.
// It then enqueues that LoadTest resource to be processed. If the object does not
// have an appropriate OwnerReference, it will simply be skipped.
func (c *Controller) handleObject(obj interface{}) {
	var object metaV1.Object
	var ok bool
	if object, ok = obj.(metaV1.Object); !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilRuntime.HandleError(fmt.Errorf("error decoding object, invalid type"))
			return
		}

		object, ok = tombstone.Obj.(metaV1.Object)
		if !ok {
			utilRuntime.HandleError(fmt.Errorf("error decoding object tombstone, invalid type"))
			return
		}

		c.logger.Info("Recovered deleted object from tombstone", zap.String("loadtest", object.GetName()))
	}

	if ownerRef := metaV1.GetControllerOf(object); ownerRef != nil {
		// If this object is not owned by a LoadTest, we should not do anything more
		// with it.
		if ownerRef.Kind != "LoadTest" {
			return
		}

		c.logger.Debug("Processing object", zap.String("object-name", object.GetName()))

		foo, err := c.loadtestsLister.Get(ownerRef.Name)
		if err != nil {
			c.logger.Debug("ignoring orphaned object", zap.String("loadtest", object.GetSelfLink()),
				zap.String("object_owner", ownerRef.Name))
			return
		}

		c.enqueueLoadTest(foo)
		return
	}
}

// enqueueLoadTest takes a LoadTest resource and converts it into a namespace/name
// string which is then put onto the work queue. This method should *not* be
// passed resources of any type other than LoadTest.
func (c *Controller) enqueueLoadTest(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilRuntime.HandleError(err)
		return
	}
	c.workQueue.Add(key)
}

func (c *Controller) updateLoadTestStatus(ctx context.Context, key string, loadTest *loadTestV1.LoadTest, loadTestFromCache *loadTestV1.LoadTest) {
	logger := c.logger.With(
		zap.String("loadtest", loadTest.GetName()),
	)

	if loadTest.Status.Phase != loadTestFromCache.Status.Phase {
		logger.Debug("Updating loadtest status",
			zap.String("new phase", loadTest.Status.Phase.String()),
			zap.String("previous phase", loadTestFromCache.Status.Phase.String()),
		)

		// UpdateStatus will not allow changes to the Spec of the resource
		_, err := c.kangalClientSet.KangalV1().LoadTests().UpdateStatus(ctx, loadTest, metaV1.UpdateOptions{})
		if err != nil {
			// The LoadTest resource may be conflicted, in which case we stop
			// processing.
			if errors.IsConflict(err) {
				utilRuntime.HandleError(fmt.Errorf("there is a conflict with loadtest '%s' between datastore and cache. it might be because object has been removed or modified in the datastore", key))
				return
			}
			logger.Error("Failed updating loadtest status", zap.Error(err))
			return
		}

		logger.Debug("Status updated", zap.Any("status", loadTest.Status))
	}
}

// checkOrCreateNamespace checks if a namespace has been created and if not deletes it
func (c *Controller) checkOrCreateNamespace(ctx context.Context, loadtest *loadTestV1.LoadTest) error {
	if loadtest.Status.Namespace != "" {
		return nil
	}

	logger := c.logger.With(zap.String("loadtest", loadtest.GetName()))
	for k, v := range loadtest.Spec.Tags {
		logger = logger.With(zap.String(k, v))
	}

	namespaces, err := c.kubeClientSet.CoreV1().Namespaces().List(ctx, metaV1.ListOptions{LabelSelector: "controller=" + loadtest.Name})
	if err != nil {
		return err
	}

	namespaceName := ""
	if len(namespaces.Items) == 0 {
		newNamespace, err := newNamespace(loadtest, c.cfg.NamespaceLabels, c.cfg.NamespaceAnnotations)
		if err != nil {
			return err
		}
		namespaceObj, err := c.kubeClientSet.CoreV1().Namespaces().Create(ctx, newNamespace, metaV1.CreateOptions{})
		if err != nil {
			return err
		}
		namespaceName = namespaceObj.GetName()
		logger.Info("Created new namespace", zap.String("namespace", namespaceName))
	} else {
		namespaceName = namespaces.Items[0].Name
	}

	loadtest.Status.Namespace = namespaceName
	return nil
}

// newNamespace creates a new namespaces object with a random name
func newNamespace(loadtest *loadTestV1.LoadTest, namespacelabels map[string]string, namespaceAnnotations map[string]string) (*coreV1.Namespace, error) {
	labels := maps.Clone(namespacelabels)
	labels["controller"] = loadtest.Name
	labels["app"] = "kangal"

	return &coreV1.Namespace{
		ObjectMeta: metaV1.ObjectMeta{
			Name:        loadtest.Name,
			Labels:      labels,
			Annotations: namespaceAnnotations,
			OwnerReferences: []metaV1.OwnerReference{
				*metaV1.NewControllerRef(loadtest, loadTestV1.SchemeGroupVersion.WithKind("LoadTest")),
			},
		},
	}, nil
}

// checkLoadTestLifeTimeExceeded returns true if the input loadtest has
// existed for longer than certain threshold, and its status is Finished or Errored
func checkLoadTestLifeTimeExceeded(loadTest *loadTestV1.LoadTest, deleteThreshold time.Duration) bool {
	if loadTest.Status.JobStatus.CompletionTime != nil {
		if time.Since(loadTest.Status.JobStatus.CompletionTime.Time) > deleteThreshold &&
			(loadTest.Status.Phase == loadTestV1.LoadTestFinished || loadTest.Status.Phase == loadTestV1.LoadTestErrored) {
			return true
		}
	}

	if loadTest.Status.Phase == loadTestV1.LoadTestErrored &&
		time.Since(loadTest.ObjectMeta.CreationTimestamp.Time) > deleteThreshold {
		return true
	}

	return false
}

func (c *Controller) deleteLoadTest(ctx context.Context, key string, loadTest *loadTestV1.LoadTest) {
	err := c.kangalClientSet.KangalV1().LoadTests().Delete(ctx, loadTest.Name, metaV1.DeleteOptions{})
	if err == nil {
		return
	}

	// The LoadTest resource may be conflicted, in which case we stop processing.
	if errors.IsConflict(err) {
		c.logger.Error("There is a conflict while deleting the loadtest", zap.Error(err))
		utilRuntime.HandleError(fmt.Errorf("there is a conflict with loadtest %q between datastore and cache. It might be because object has been removed or modified in the datastore", key))
	}
	c.logger.Error("Failed to delete loadtest:", zap.Error(err))
}
