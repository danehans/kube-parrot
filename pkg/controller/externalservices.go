package controller

import (
	"net"
	"strings"
	"sync"

	"github.com/golang/glog"
	"github.com/sapcc/kube-parrot/pkg/bgp"
	"github.com/sapcc/kube-parrot/pkg/forked/informer"
	"github.com/sapcc/kube-parrot/pkg/forked/util"
	"github.com/sapcc/kube-parrot/pkg/types"
	"github.com/sapcc/kube-parrot/pkg/util"
	"k8s.io/client-go/1.5/pkg/api/v1"
	"k8s.io/client-go/1.5/tools/cache"
)

type ExternalServicesController struct {
	routes     *bgp.ExternalIPRoutesStore
	reconciler reconciler.DirtyReconcilerInterface
	hostIP     net.IP

	services  cache.Store
	endpoints cache.Store
	proxies   cache.Store
}

func NewExternalServicesController(informers informer.SharedInformerFactory,
	hostIP net.IP, routes *bgp.ExternalIPRoutesStore) *ExternalServicesController {

	c := &ExternalServicesController{
		routes:    routes,
		hostIP:    hostIP,
		services:  cache.NewStore(cache.DeletionHandlingMetaNamespaceKeyFunc),
		endpoints: cache.NewStore(cache.DeletionHandlingMetaNamespaceKeyFunc),
		proxies:   cache.NewStore(cache.DeletionHandlingMetaNamespaceKeyFunc),
	}

	c.reconciler = reconciler.NewNamedDirtyReconciler("externalips", c.reconcile)

	informers.Endpoints().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.endpointsAdd,
		UpdateFunc: c.endpointsUpdate,
		DeleteFunc: c.endpointsDelete,
	})

	informers.Pods().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.podAdd,
		UpdateFunc: c.podUpdate,
		DeleteFunc: c.podDelete,
	})

	informers.Services().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.serviceAdd,
		UpdateFunc: c.serviceUpdate,
		DeleteFunc: c.serviceDelete,
	})

	return c
}

func (c *ExternalServicesController) Run(stopCh <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()
	wg.Add(1)

	c.reconciler.Run(stopCh)

	<-stopCh
}

func (c *ExternalServicesController) podDelete(obj interface{}) {
	pod := obj.(*v1.Pod)
	if _, exists, _ := c.proxies.Get(pod); exists {
		glog.V(3).Infof("Deleting Kube-Proxy (%s)", pod.Name)
		c.proxies.Delete(pod)
		c.reconciler.Dirty()
	}
}

func (c *ExternalServicesController) podAdd(obj interface{}) {
	pod := obj.(*v1.Pod)
	if !strings.HasPrefix(pod.Name, types.KubeProxyPrefix) ||
		pod.Namespace != types.KubeProxyNamespace {
		return
	}

	if pod.Status.HostIP != c.hostIP.To4().String() {
		return
	}

	if util.IsPodReady(pod) {
		glog.V(5).Infof("Kube-Proxy is ready (%s)", pod.Name)
		if _, exists, _ := c.proxies.Get(pod); !exists {
			glog.V(3).Infof("Adding Kube-Proxy (%s)", pod.Name)
			c.proxies.Add(pod)
			c.reconciler.Dirty()
		}
	} else {
		glog.V(5).Infof("Kube-Proxy is NOT ready (%s)", pod.Name)
		if _, exists, _ := c.proxies.Get(pod); exists {
			glog.V(3).Infof("Deleting Kube-Proxy (%s)", pod.Name)
			c.proxies.Delete(pod)
			c.reconciler.Dirty()
		}
	}

}

func (c *ExternalServicesController) podUpdate(old, cur interface{}) {
	c.podAdd(cur)
}

func (c *ExternalServicesController) serviceDelete(obj interface{}) {
	service := obj.(*v1.Service)
	glog.V(3).Infof("Deleting Service (%s)", service.Name)
	c.services.Delete(service)
	c.reconciler.Dirty()
}

func (c *ExternalServicesController) serviceAdd(obj interface{}) {
	service := obj.(*v1.Service)
	//if l, ok := service.Annotations[types.AnnotationBGPAnnouncement]; ok {
	//  announcementRequested, err := strconv.ParseBool(l)
	//  if err != nil {
	//    glog.Errorf("Failed to parse annotation %v: %v", types.AnnotationBGPAnnouncement, err)
	//    return
	//  }

	//  if !announcementRequested {
	//    glog.V(3).Infof("Skipping service %v. Annotation is set but not true. Huh?", service.GetName())
	//    return
	//  }
	//} else {
	//  glog.V(5).Infof("Skipping service %v. No announce annotation defined...", service.GetName())
	//  return
	//}

	if len(service.Spec.ExternalIPs) == 0 {
		glog.V(3).Infof("Skipping service %v. No externalIP defined...", service.GetName())
		return
	}

	if _, exists, _ := c.services.Get(service); !exists {
		glog.V(3).Infof("Deleting Service (%s)", service.Name)
		c.services.Add(service)
		c.reconciler.Dirty()
	}
}

func (c *ExternalServicesController) serviceUpdate(old, cur interface{}) {
	c.serviceAdd(cur)
}

func (c *ExternalServicesController) endpointsDelete(obj interface{}) {
	endpoints := obj.(*v1.Endpoints)

	if _, exists, _ := c.endpoints.Get(endpoints); exists {
		glog.V(3).Infof("Deleting Endpoints (%s/%s)", endpoints.Namespace, endpoints.Name)
		c.endpoints.Delete(endpoints)
		c.reconciler.Dirty()
	}
}

func (c *ExternalServicesController) endpointsAdd(obj interface{}) {
	endpoints := obj.(*v1.Endpoints)

	ready := false
	for _, v := range endpoints.Subsets {
		if len(v.Addresses) > 0 {
			ready = true
			break
		}
	}

	if ready {
		glog.V(5).Infof("Endpoint is ready (%s)", endpoints.Name)
		if _, exists, _ := c.endpoints.Get(endpoints); !exists {
			glog.V(3).Infof("Adding Endpoints (%s/%s)", endpoints.Namespace, endpoints.Name)
			c.endpoints.Add(endpoints)
			c.reconciler.Dirty()
		}
	} else {
		if !strings.HasSuffix(endpoints.Name, "kube-scheduler") &&
			!strings.HasSuffix(endpoints.Name, "kube-controller-manager") {
			glog.V(5).Infof("Endpoint is NOT ready (%s)", endpoints.Name)
		}
		if _, exists, _ := c.endpoints.Get(endpoints); exists {
			glog.V(3).Infof("Deleting Endpoints (%s/%s)", endpoints.Namespace, endpoints.Name)
			c.endpoints.Delete(endpoints)
			c.reconciler.Dirty()
		}
	}
}

func (c *ExternalServicesController) endpointsUpdate(old, cur interface{}) {
	c.endpointsAdd(cur)
}

func (c *ExternalServicesController) reconcile() error {
	for _, route := range c.routes.List() {
		if _, ok, _ := c.proxies.Get(route.Proxy); !ok {
			if err := c.routes.Delete(route); err != nil {
				return err
			}
		}

		if _, ok, _ := c.services.Get(route.Service); !ok {
			if err := c.routes.Delete(route); err != nil {
				return err
			}
		}

		if _, ok, _ := c.endpoints.Get(route.Service); !ok {
			if err := c.routes.Delete(route); err != nil {
				return err
			}
		}
	}

	for _, proxy := range c.proxies.List() {
		for _, service := range c.services.List() {
			if _, ok, _ := c.endpoints.Get(service); ok {
				if err := c.routes.Add(service.(*v1.Service), proxy.(*v1.Pod)); err != nil {
					return err
				}
			}
		}
	}

	return nil
}
