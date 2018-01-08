package clustercontroller

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	api "github.com/presslabs/titanium/pkg/apis/titanium/v1alpha1"
	controllerpkg "github.com/presslabs/titanium/pkg/controller"
	mcinformers "github.com/presslabs/titanium/pkg/generated/informers/externalversions/titanium/v1alpha1"
	mclisters "github.com/presslabs/titanium/pkg/generated/listers/titanium/v1alpha1"
	"github.com/presslabs/titanium/pkg/util"
	"github.com/presslabs/titanium/pkg/util/k8sutil"
)

const (
	initRetryWaitTime = 30 * time.Second
	workerPeriodTime  = 1 * time.Second

	ControllerName = "mysqlclusterController"
)

type Controller struct {
	logger *logrus.Entry

	Namespace      string
	ServiceAccount string

	KubeCli    kubernetes.Interface
	KubeExtCli apiextensionsclient.Interface

	CreateCRD bool

	clusterInformerSync cache.InformerSynced
	clusterLister       mclisters.MysqlClusterLister

	clusterInformer cache.SharedIndexInformer

	queue    workqueue.RateLimitingInterface
	workerWg sync.WaitGroup
}

func New(mysqlClusterInformer mcinformers.MysqlClusterInformer,
	namespace string, serviceAccount string,
	kubecli kubernetes.Interface,
	kubeExtCli apiextensionsclient.Interface,
	createCRD bool,
) *Controller {
	ctrl := &Controller{
		logger:     logrus.WithField("pkg", "controller"),
		Namespace:  namespace,
		KubeCli:    kubecli,
		KubeExtCli: kubeExtCli,
		CreateCRD:  createCRD,

		//clusters: make(map[string]*cluster.Cluster),
	}

	ctrl.queue = workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "mysqlcluster")

	// add handlers.
	mysqlClusterInformer.Informer().AddEventHandler(&controllerpkg.QueuingEventHandler{Queue: ctrl.queue})
	ctrl.clusterInformerSync = mysqlClusterInformer.Informer().HasSynced
	ctrl.clusterLister = mysqlClusterInformer.Lister()

	ctrl.clusterInformer = mysqlClusterInformer.Informer()

	return ctrl

}

func (c *Controller) Start(workers int, stopCh <-chan struct{}) error {
	c.logger.Info("Starting controller ...")

	if !c.CreateCRD {
		err := c.createCRDIfNotExists()
		if err != nil {
			return err
		}
	}

	c.logger.Info(fmt.Errorf("Before WaitForCacheSync: %t", c.clusterInformerSync()))
	//for {
	//	if !c.clusterInformerSync() {
	//		c.logger.Info("stay!")
	//		time.Sleep(time.Second)
	//	}
	//}
	c.logger.Info(fmt.Errorf("Before WaitForCacheSync: %t", c.clusterInformerSync()))
	if !cache.WaitForCacheSync(stopCh, c.clusterInformerSync) {
		return fmt.Errorf("error waiting for informer cache to sync.")
	}
	c.logger.Info("After WaitForCacheSync")

	for i := 0; i < workers; i++ {
		c.workerWg.Add(1)
		go wait.Until(func() { c.work(stopCh) }, workerPeriodTime, stopCh)
	}
	<-stopCh
	c.logger.Info("Shutting down controller.")
	c.queue.ShutDown()
	c.logger.Debug("Wait for workers to exit...")
	c.workerWg.Wait()
	c.logger.Debug("Workers exited.")
	return nil
}

func (c *Controller) work(stopCh <-chan struct{}) {
	defer c.workerWg.Done()
	c.logger.Info("Starting worker.")
	for {
		obj, shutdown := c.queue.Get()
		if shutdown {
			break
		}

		var key string
		err := func(obj interface{}) error {
			defer c.queue.Done(obj)
			var ok bool
			if key, ok = obj.(string); !ok {
				return nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx = util.ContextWithStopCh(ctx, stopCh)
			c.logger.Info(fmt.Errorf("%s controller: syncing item '%s'", ControllerName, key))
			if err := c.processNextWorkItem(ctx, key); err != nil {
				return err
			}
			c.queue.Forget(obj)
			return nil
		}(obj)

		if err != nil {
			c.logger.Error("%s controller: Re-queuing item %q due to error processing: %s", ControllerName, key, err.Error())
			c.queue.AddRateLimited(obj)
			continue
		}
	}
}

func (c *Controller) processNextWorkItem(ctx context.Context, key string) error {
	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	mysqlCluster, err := c.clusterLister.Get(name)

	if err != nil {
		if k8sutil.IsKubernetesResourceNotFoundError(err) {
			runtime.HandleError(fmt.Errorf("issuer %q in work queue no longer exists", key))
			return nil
		}

		return err
	}

	return c.Sync(ctx, mysqlCluster)
}

func (c *Controller) createCRDIfNotExists() error {
	c.logger.Info("Creating CRD...")

	err := k8sutil.CreateCRD(
		c.KubeExtCli,
		api.MysqlClusterCRDName,
		api.MysqlClusterCRDKind,
		api.MysqlClusterCRDPlural,
		"mysql",
	)
	if err != nil {
		c.logger.Error("Faild to create CRD: %v", err)
		return err
	}
	return k8sutil.WaitCRDReady(c.KubeExtCli, api.MysqlClusterCRDName)
}

func init() {
	controllerpkg.Register(ControllerName, func(ctx *controllerpkg.Context) controllerpkg.Interface {
		return New(
			ctx.SharedInformerFactory.Titanium().V1alpha1().MysqlClusters(),
			ctx.Namespace,
			ctx.ServiceAccount,
			ctx.KubeCli,
			ctx.KubeExtCli,
			ctx.CreateCRD,
		).Start
	})
}