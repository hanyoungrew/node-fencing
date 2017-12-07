package controller

import (
	"os"
	"time"

	"github.com/golang/glog"
	crdv1 "github.com/rootfs/node-fencing/pkg/apis/crd/v1"
	"github.com/rootfs/node-fencing/pkg/fencing"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
)

type Executor struct {
	crdClient           *rest.RESTClient
	crdScheme           *runtime.Scheme
	client              kubernetes.Interface
	nodeFenceController cache.Controller
}

func NewNodeFenceExecutorController(client kubernetes.Interface, crdClient *rest.RESTClient, crdScheme *runtime.Scheme) *Executor {
	c := &Executor{
		client:    client,
		crdClient: crdClient,
		crdScheme: crdScheme,
	}

	// Watch NodeFence objects
	source := cache.NewListWatchFromClient(
		c.crdClient,
		crdv1.NodeFenceResourcePlural,
		apiv1.NamespaceAll,
		fields.Everything())

	_, nodeFenceController := cache.NewInformer(
		source,
		&crdv1.NodeFence{},
		time.Minute*60,
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.onNodeFencingAdd,
			UpdateFunc: c.onNodeFencingUpdate,
			DeleteFunc: c.onNodeFencingDelete,
		},
	)

	c.nodeFenceController = nodeFenceController

	return c
}

func (c *Executor) Run(ctx <-chan struct{}) {
	glog.Infof("Node fence executor starting")
	go c.nodeFenceController.Run(ctx)
	glog.Infof("Waiting for informer initial sync")
	wait.Poll(time.Second, 5*time.Minute, func() (bool, error) {
		return c.nodeFenceController.HasSynced(), nil
	})
	if !c.nodeFenceController.HasSynced() {
		glog.Errorf("node fence informer controller initial sync timeout")
		os.Exit(1)
	}
	glog.Infof("Watching node fence objects")
	c.handleExistingNodeFences()
}

func (c *Executor) handleExistingNodeFences() {
	var nodeFences crdv1.NodeFenceList
	err := c.crdClient.Get().Resource(crdv1.NodeFenceResourcePlural).Do().Into(&nodeFences)
	if err != nil {
		glog.Errorf("something went wrong - could not fetch nodefences - %s", err)
	} else {
		for _, nf := range nodeFences.Items {
			glog.Infof("Read %s ..", nf.Metadata.Name)
			switch nf.Status {
			case crdv1.NodeFenceConditionNew:
				c.startExecution(nf)
			case crdv1.NodeFenceConditionError:
				// Check if current host failed to run - if so, let controller initiate new executor
			case crdv1.NodeFenceConditionRunning:
				glog.Infof("Node fence is already running on: %s", nf.Hostname)
			}
		}
	}
}

func (c *Executor) startExecution(nf crdv1.NodeFence) {
	config, err := fencing.GetNodeFenceConfig(nf.NodeName, c.client)
	if err != nil {
		glog.Errorf("node fencing failed on node %s", nf.NodeName)
	}
	nf.Status = crdv1.NodeFenceConditionRunning
	hostname, _ := os.Hostname()
	nf.Hostname = hostname
	err = c.crdClient.Put().Resource(crdv1.NodeFenceResourcePlural).Name(nf.Metadata.Name).Body(&nf).Do().Into(&nf)
	if err != nil {
		glog.Errorf("Failed to update status to 'running': %s", err)
		return
	}
	err = fencing.ExecuteFenceAgents(config, nf.Step, c.client)
	if err != nil {
		nf.Status = crdv1.NodeFenceConditionError
	} else {
		nf.Status = crdv1.NodeFenceConditionDone
	}
	err = c.crdClient.Put().Resource(crdv1.NodeFenceResourcePlural).Name(nf.Metadata.Name).Body(&nf).Do().Into(&nf)
	if err != nil {
		glog.Errorf("Failed to update status to 'running': %s", err)
		return
	}
}

func (c *Executor) onNodeFencingAdd(obj interface{}) {
	fence := obj.(*crdv1.NodeFence)
	c.startExecution(*fence)
}

func (c *Executor) onNodeFencingUpdate(oldObj, _ interface{}) {
	oldfence := oldObj.(*crdv1.NodeFence)
	glog.Infof("node fence object updated %s", oldfence.Metadata.Name)
}

func (c *Executor) onNodeFencingDelete(obj interface{}) {
	fence := obj.(*crdv1.NodeFence)
	glog.Infof("node fence object removed %s", fence.Metadata.Name)
}