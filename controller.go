/*
Copyright 2015 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

/**
 * Copyright 2016, Z Lab Corporation. All rights reserved.
 *
 * For the full copyright and license information, please view the LICENSE
 * file that was distributed with this source code.
 */

package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/golang/glog"

	"k8s.io/kubernetes/pkg/api"
	podutil "k8s.io/kubernetes/pkg/api/pod"
	"k8s.io/kubernetes/pkg/apis/extensions"
	"k8s.io/kubernetes/pkg/client/cache"
	"k8s.io/kubernetes/pkg/client/record"
	client "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/controller/framework"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/util/intstr"
	"k8s.io/kubernetes/pkg/watch"

	"github.com/zlabjp/nghttpx-ingress-lb/nghttpx"
)

const (
	defUpstreamName          = "upstream-default-backend"
	defServerName            = "_"
	namedPortAnnotation      = "ingress.kubernetes.io/named-ports"
	backendConfigAnnotation  = "ingress.zlab.co.jp/backend-config"
	podStoreSyncedPollPeriod = 1 * time.Second
)

type serviceAnnotation map[string]string

// getPort returns the port defined in a named port
func (npm serviceAnnotation) getPort(name string) (string, bool) {
	val, ok := npm.getPortMappings()[name]
	return val, ok
}

// getPortMappings returns the map containing the
// mapping of named port names and the port number
func (npm serviceAnnotation) getPortMappings() map[string]string {
	data := npm[namedPortAnnotation]
	var mapping map[string]string
	if data == "" {
		return mapping
	}
	if err := json.Unmarshal([]byte(data), &mapping); err != nil {
		glog.Errorf("unexpected error reading annotations: %v", err)
	}

	return mapping
}

type ingressAnnotation map[string]string

// backend configuration obtained from ingress annotation, specified per service port
type PortBackendConfig struct {
	// backend application protocol.  At the moment, this should be either "h2" or "http/1.1".
	Proto string `json:"proto,omitempty"`
	// true if backend connection requires TLS
	TLS bool `json:"tls,omitempty"`
	// SNI hostname for backend TLS connection
	SNI string `json:"sni,omitempty"`
}

func (ia ingressAnnotation) getBackendConfig() map[string]map[string]PortBackendConfig {
	data := ia[backendConfigAnnotation]
	// the first key specifies service name, and secondary key specifies port name.
	var config map[string]map[string]PortBackendConfig
	if data == "" {
		return config
	}
	if err := json.Unmarshal([]byte(data), &config); err != nil {
		glog.Errorf("unexpected error reading %v annotation: %v", backendConfigAnnotation, err)
		return config
	}

	return config
}

// loadBalancerController watches the kubernetes api and adds/removes services
// from the loadbalancer
type loadBalancerController struct {
	client         *client.Client
	ingController  *framework.Controller
	endpController *framework.Controller
	svcController  *framework.Controller
	secrController *framework.Controller
	mapController  *framework.Controller
	ingLister      StoreToIngressLister
	svcLister      cache.StoreToServiceLister
	endpLister     cache.StoreToEndpointsLister
	secrLister     StoreToSecretLister
	mapLister      StoreToMapLister
	nghttpx        *nghttpx.Manager
	podInfo        *podInfo
	defaultSvc     string
	ngxConfigMap   string

	recorder record.EventRecorder

	syncQueue *taskQueue

	// taskQueue used to update the status of the Ingress rules.
	// this avoids a sync execution in the ResourceEventHandlerFuncs
	ingQueue *taskQueue

	// stopLock is used to enforce only a single call to Stop is active.
	// Needed because we allow stopping through an http endpoint and
	// allowing concurrent stoppers leads to stack traces.
	stopLock sync.Mutex
	shutdown bool
	stopCh   chan struct{}
}

// newLoadBalancerController creates a controller for nghttpx loadbalancer
func newLoadBalancerController(kubeClient *client.Client, resyncPeriod time.Duration, defaultSvc,
	namespace, ngxConfigMapName string, runtimeInfo *podInfo) (*loadBalancerController, error) {

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(kubeClient.Events(namespace))

	lbc := loadBalancerController{
		client:       kubeClient,
		stopCh:       make(chan struct{}),
		podInfo:      runtimeInfo,
		nghttpx:      nghttpx.NewManager(kubeClient),
		ngxConfigMap: ngxConfigMapName,
		defaultSvc:   defaultSvc,
		recorder:     eventBroadcaster.NewRecorder(api.EventSource{Component: "nghttpx-ingress-controller"}),
	}

	lbc.syncQueue = NewTaskQueue(lbc.sync)
	lbc.ingQueue = NewTaskQueue(lbc.updateIngressStatus)

	lbc.ingLister.Store, lbc.ingController = framework.NewInformer(
		&cache.ListWatch{
			ListFunc: func(options api.ListOptions) (runtime.Object, error) {
				return lbc.client.Extensions().Ingress(namespace).List(options)
			},
			WatchFunc: func(options api.ListOptions) (watch.Interface, error) {
				return lbc.client.Extensions().Ingress(namespace).Watch(options)
			},
		},
		&extensions.Ingress{},
		resyncPeriod,
		framework.ResourceEventHandlerFuncs{
			AddFunc:    lbc.addIngressNotification,
			UpdateFunc: lbc.updateIngressNotification,
			DeleteFunc: lbc.deleteIngressNotification,
		},
	)

	lbc.endpLister.Store, lbc.endpController = framework.NewInformer(
		&cache.ListWatch{
			ListFunc: func(options api.ListOptions) (runtime.Object, error) {
				return lbc.client.Endpoints(namespace).List(options)
			},
			WatchFunc: func(options api.ListOptions) (watch.Interface, error) {
				return lbc.client.Endpoints(namespace).Watch(options)
			},
		},
		&api.Endpoints{},
		resyncPeriod,
		framework.ResourceEventHandlerFuncs{
			AddFunc:    lbc.addEndpointNotification,
			UpdateFunc: lbc.updateEndpointNotification,
			DeleteFunc: lbc.deleteEndpointNotification,
		},
	)

	lbc.svcLister.Store, lbc.svcController = framework.NewInformer(
		&cache.ListWatch{
			ListFunc:  serviceListFunc(lbc.client, namespace),
			WatchFunc: serviceWatchFunc(lbc.client, namespace),
		},
		&api.Service{},
		resyncPeriod,
		framework.ResourceEventHandlerFuncs{},
	)

	lbc.secrLister.Store, lbc.secrController = framework.NewInformer(
		&cache.ListWatch{
			ListFunc: func(options api.ListOptions) (runtime.Object, error) {
				return lbc.client.Secrets(namespace).List(options)
			},
			WatchFunc: func(options api.ListOptions) (watch.Interface, error) {
				return lbc.client.Secrets(namespace).Watch(options)
			},
		},
		&api.Secret{},
		resyncPeriod,
		framework.ResourceEventHandlerFuncs{
			AddFunc:    lbc.addSecretNotification,
			UpdateFunc: lbc.updateSecretNotification,
			DeleteFunc: lbc.deleteSecretNotification,
		},
	)

	lbc.mapLister.Store, lbc.mapController = framework.NewInformer(
		&cache.ListWatch{
			ListFunc: func(options api.ListOptions) (runtime.Object, error) {
				return lbc.client.ConfigMaps(namespace).List(options)
			},
			WatchFunc: func(options api.ListOptions) (watch.Interface, error) {
				return lbc.client.ConfigMaps(namespace).Watch(options)
			},
		},
		&api.ConfigMap{},
		resyncPeriod,
		framework.ResourceEventHandlerFuncs{
			AddFunc:    lbc.addConfigMapNotification,
			UpdateFunc: lbc.updateConfigMapNotification,
			DeleteFunc: lbc.deleteConfigMapNotification,
		},
	)

	return &lbc, nil
}

func (lbc *loadBalancerController) addIngressNotification(obj interface{}) {
	addIng := obj.(*extensions.Ingress)
	lbc.recorder.Eventf(addIng, api.EventTypeNormal, "CREATE", fmt.Sprintf("%s/%s", addIng.Namespace, addIng.Name))
	lbc.ingQueue.enqueue(obj)
	lbc.syncQueue.enqueue(obj)
}

func (lbc *loadBalancerController) updateIngressNotification(old interface{}, cur interface{}) {
	if reflect.DeepEqual(old, cur) {
		return
	}

	upIng := cur.(*extensions.Ingress)
	lbc.recorder.Eventf(upIng, api.EventTypeNormal, "UPDATE", fmt.Sprintf("%s/%s", upIng.Namespace, upIng.Name))
	lbc.ingQueue.enqueue(cur)
	lbc.syncQueue.enqueue(cur)
}

func (lbc *loadBalancerController) deleteIngressNotification(obj interface{}) {
	upIng := obj.(*extensions.Ingress)
	lbc.recorder.Eventf(upIng, api.EventTypeNormal, "DELETE", fmt.Sprintf("%s/%s", upIng.Namespace, upIng.Name))
	lbc.syncQueue.enqueue(obj)
}

func serviceListFunc(c *client.Client, ns string) func(api.ListOptions) (runtime.Object, error) {
	return func(opts api.ListOptions) (runtime.Object, error) {
		return c.Services(ns).List(opts)
	}
}

func serviceWatchFunc(c *client.Client, ns string) func(options api.ListOptions) (watch.Interface, error) {
	return func(options api.ListOptions) (watch.Interface, error) {
		return c.Services(ns).Watch(options)
	}
}

func (lbc *loadBalancerController) addEndpointNotification(obj interface{}) {
	lbc.syncQueue.enqueue(obj)
}

func (lbc *loadBalancerController) updateEndpointNotification(old, cur interface{}) {
	if reflect.DeepEqual(old, cur) {
		return
	}

	lbc.syncQueue.enqueue(cur)
}

func (lbc *loadBalancerController) deleteEndpointNotification(obj interface{}) {
	lbc.syncQueue.enqueue(obj)
}

func (lbc *loadBalancerController) addSecretNotification(obj interface{}) {
	addSecr := obj.(*api.Secret)
	if !lbc.secrReferenced(addSecr.Namespace, addSecr.Name) {
		return
	}

	secrKey := fmt.Sprintf("%s/%s", addSecr.Namespace, addSecr.Name)
	lbc.recorder.Eventf(addSecr, api.EventTypeNormal, "CREATE", secrKey)
	lbc.syncQueue.enqueue(obj)
}

func (lbc *loadBalancerController) updateSecretNotification(old, cur interface{}) {
	if reflect.DeepEqual(old, cur) {
		return
	}

	upSecr := cur.(*api.Secret)
	if !lbc.secrReferenced(upSecr.Namespace, upSecr.Name) {
		return
	}

	secrKey := fmt.Sprintf("%s/%s", upSecr.Namespace, upSecr.Name)
	lbc.recorder.Eventf(upSecr, api.EventTypeNormal, "UPDATE", secrKey)
	lbc.syncQueue.enqueue(cur)
}

func (lbc *loadBalancerController) deleteSecretNotification(obj interface{}) {
	delSecr := obj.(*api.Secret)
	if !lbc.secrReferenced(delSecr.Namespace, delSecr.Name) {
		return
	}

	secrKey := fmt.Sprintf("%s/%s", delSecr.Namespace, delSecr.Name)
	lbc.recorder.Eventf(delSecr, api.EventTypeNormal, "DELETE", secrKey)
	lbc.syncQueue.enqueue(obj)
}

func (lbc *loadBalancerController) addConfigMapNotification(obj interface{}) {
	addCmap := obj.(*api.ConfigMap)
	mapKey := fmt.Sprintf("%s/%s", addCmap.Namespace, addCmap.Name)
	lbc.recorder.Eventf(addCmap, api.EventTypeNormal, "CREATE", mapKey)
	lbc.syncQueue.enqueue(obj)
}

func (lbc *loadBalancerController) updateConfigMapNotification(old, cur interface{}) {
	if reflect.DeepEqual(old, cur) {
		return
	}

	upCmap := cur.(*api.ConfigMap)
	mapKey := fmt.Sprintf("%s/%s", upCmap.Namespace, upCmap.Name)
	// updates to configuration configmaps can trigger an update
	if mapKey != lbc.ngxConfigMap {
		return
	}

	lbc.recorder.Eventf(upCmap, api.EventTypeNormal, "UPDATE", mapKey)
	lbc.syncQueue.enqueue(cur)
}

func (lbc *loadBalancerController) deleteConfigMapNotification(obj interface{}) {
	upCmap := obj.(*api.ConfigMap)
	mapKey := fmt.Sprintf("%s/%s", upCmap.Namespace, upCmap.Name)
	lbc.recorder.Eventf(upCmap, api.EventTypeNormal, "DELETE", mapKey)
	lbc.syncQueue.enqueue(obj)
}

func (lbc *loadBalancerController) controllersInSync() bool {
	return lbc.ingController.HasSynced() &&
		lbc.svcController.HasSynced() &&
		lbc.endpController.HasSynced() &&
		lbc.secrController.HasSynced() &&
		lbc.mapController.HasSynced()
}

func (lbc *loadBalancerController) getConfigMap(cfgName string) *api.ConfigMap {
	if cfgName == "" {
		return &api.ConfigMap{}
	}

	ns, name, _ := parseNsName(cfgName)
	// TODO: check why lbc.mapLister.Store.GetByKey(mapKey) is not stable (random content)
	if cfg, err := lbc.client.ConfigMaps(ns).Get(name); err != nil {
		glog.V(3).Infof("configmap not found %v : %v", cfgName, err)
		return &api.ConfigMap{}
	} else {
		return cfg
	}
}

// checkSvcForUpdate verifies if one of the running pods for a service contains
// named port. If the annotation in the service does not exists or is not equals
// to the port mapping obtained from the pod the service must be updated to reflect
// the current state
func (lbc *loadBalancerController) checkSvcForUpdate(svc *api.Service) (map[string]string, error) {
	// get the pods associated with the service
	// TODO: switch this to a watch
	pods, err := lbc.client.Pods(svc.Namespace).List(api.ListOptions{
		LabelSelector: labels.Set(svc.Spec.Selector).AsSelector(),
	})

	namedPorts := map[string]string{}
	if err != nil {
		return namedPorts, fmt.Errorf("error searching service pods %v/%v: %v", svc.Namespace, svc.Name, err)
	}

	if len(pods.Items) == 0 {
		return namedPorts, nil
	}

	// we need to check only one pod searching for named ports
	pod := &pods.Items[0]
	glog.V(4).Infof("checking pod %v/%v for named port information", pod.Namespace, pod.Name)
	for i := range svc.Spec.Ports {
		servicePort := &svc.Spec.Ports[i]
		_, err := strconv.Atoi(servicePort.TargetPort.StrVal)
		if err != nil {
			portNum, err := podutil.FindPort(pod, servicePort)
			if err != nil {
				glog.V(4).Infof("failed to find port for service %s/%s: %v", svc.Namespace, svc.Name, err)
				continue
			}

			if servicePort.TargetPort.StrVal == "" {
				continue
			}

			namedPorts[servicePort.TargetPort.StrVal] = fmt.Sprintf("%v", portNum)
		}
	}

	if svc.ObjectMeta.Annotations == nil {
		svc.ObjectMeta.Annotations = map[string]string{}
	}

	curNamedPort := svc.ObjectMeta.Annotations[namedPortAnnotation]
	if len(namedPorts) > 0 && !reflect.DeepEqual(curNamedPort, namedPorts) {
		data, _ := json.Marshal(namedPorts)

		newSvc, err := lbc.client.Services(svc.Namespace).Get(svc.Name)
		if err != nil {
			return namedPorts, fmt.Errorf("error getting service %v/%v: %v", svc.Namespace, svc.Name, err)
		}

		if newSvc.ObjectMeta.Annotations == nil {
			newSvc.ObjectMeta.Annotations = map[string]string{}
		}

		newSvc.ObjectMeta.Annotations[namedPortAnnotation] = string(data)
		glog.Infof("updating service %v with new named port mappings", svc.Name)
		_, err = lbc.client.Services(svc.Namespace).Update(newSvc)
		if err != nil {
			return namedPorts, fmt.Errorf("error syncing service %v/%v: %v", svc.Namespace, svc.Name, err)
		}

		return newSvc.ObjectMeta.Annotations, nil
	}

	return namedPorts, nil
}

func (lbc *loadBalancerController) sync(key string) {
	if !lbc.controllersInSync() {
		time.Sleep(podStoreSyncedPollPeriod)
		lbc.syncQueue.requeue(key, fmt.Errorf("deferring sync till endpoints controller has synced"))
		return
	}

	ings := lbc.ingLister.Store.List()
	upstreams, server := lbc.getUpstreamServers(ings)

	cfg := lbc.getConfigMap(lbc.ngxConfigMap)

	ngxConfig := lbc.nghttpx.ReadConfig(cfg)
	lbc.nghttpx.CheckAndReload(ngxConfig, nghttpx.IngressConfig{
		Upstreams: upstreams,
		Server:    server,
	})
}

func (lbc *loadBalancerController) updateIngressStatus(key string) {
	if !lbc.controllersInSync() {
		time.Sleep(podStoreSyncedPollPeriod)
		lbc.ingQueue.requeue(key, fmt.Errorf("deferring sync till endpoints controller has synced"))
		return
	}

	obj, ingExists, err := lbc.ingLister.Store.GetByKey(key)
	if err != nil {
		lbc.ingQueue.requeue(key, err)
		return
	}

	if !ingExists {
		return
	}

	ing := obj.(*extensions.Ingress)

	ingClient := lbc.client.Extensions().Ingress(ing.Namespace)

	currIng, err := ingClient.Get(ing.Name)
	if err != nil {
		glog.Errorf("unexpected error searching Ingress %v/%v: %v", ing.Namespace, ing.Name, err)
		return
	}

	lbIPs := ing.Status.LoadBalancer.Ingress
	if !lbc.isStatusIPDefined(lbIPs) {
		glog.Infof("Updating loadbalancer %v/%v with IP %v", ing.Namespace, ing.Name, lbc.podInfo.NodeIP)
		currIng.Status.LoadBalancer.Ingress = append(currIng.Status.LoadBalancer.Ingress, api.LoadBalancerIngress{
			IP: lbc.podInfo.NodeIP,
		})
		if _, err := ingClient.UpdateStatus(currIng); err != nil {
			lbc.recorder.Eventf(currIng, api.EventTypeWarning, "UPDATE", "error: %v", err)
			return
		}

		lbc.recorder.Eventf(currIng, api.EventTypeNormal, "CREATE", "ip: %v", lbc.podInfo.NodeIP)
	}
}

func (lbc *loadBalancerController) isStatusIPDefined(lbings []api.LoadBalancerIngress) bool {
	for _, lbing := range lbings {
		if lbing.IP == lbc.podInfo.NodeIP {
			return true
		}
	}

	return false
}

func (lbc *loadBalancerController) getDefaultUpstream() *nghttpx.Upstream {
	upstream := &nghttpx.Upstream{
		Name: defUpstreamName,
	}
	svcKey := lbc.defaultSvc
	svcObj, svcExists, err := lbc.svcLister.Store.GetByKey(svcKey)
	if err != nil {
		glog.Warningf("unexpected error searching the default backend %v: %v", lbc.defaultSvc, err)
		upstream.Backends = append(upstream.Backends, nghttpx.NewDefaultServer())
		return upstream
	}

	if !svcExists {
		glog.Warningf("service %v does no exists", svcKey)
		upstream.Backends = append(upstream.Backends, nghttpx.NewDefaultServer())
		return upstream
	}

	svc := svcObj.(*api.Service)

	portBackendConfig := defaultPortBackendConfig()

	endps := lbc.getEndpoints(svc, svc.Spec.Ports[0].TargetPort, api.ProtocolTCP, &portBackendConfig)
	if len(endps) == 0 {
		glog.Warningf("service %v does no have any active endpoints", svcKey)
		upstream.Backends = append(upstream.Backends, nghttpx.NewDefaultServer())
	} else {
		upstream.Backends = append(upstream.Backends, endps...)
	}

	return upstream
}

// in nghttpx terminology, nghttpx.Upstream is backend, nghttpx.Server is frontend
func (lbc *loadBalancerController) getUpstreamServers(data []interface{}) ([]*nghttpx.Upstream, *nghttpx.Server) {
	pems := lbc.getPemsFromIngress(data)

	server := &nghttpx.Server{}

	if len(pems) > 0 {
		server.SSL = true
		server.DefaultTLSCred = pems[0]
		server.SubTLSCred = pems[1:]
	}

	upstreams := []*nghttpx.Upstream{}

	for _, ingIf := range data {
		ing := ingIf.(*extensions.Ingress)

		backendConfig := ingressAnnotation(ing.ObjectMeta.Annotations).getBackendConfig()

		for _, rule := range ing.Spec.Rules {
			if rule.IngressRuleValue.HTTP == nil {
				continue
			}

			for _, path := range rule.HTTP.Paths {
				upsName := fmt.Sprintf("%v-%v-%v-%v-%v", ing.GetNamespace(), path.Backend.ServiceName, path.Backend.ServicePort.String(), rule.Host, path.Path)
				ups := &nghttpx.Upstream{
					Name: upsName,
					Host: rule.Host,
					Path: path.Path,
				}

				glog.Infof("upstream name=%v", upsName)
				glog.Infof("host=%v, path=%v", ups.Host, ups.Path)

				svcKey := fmt.Sprintf("%v/%v", ing.GetNamespace(), path.Backend.ServiceName)
				svcObj, svcExists, err := lbc.svcLister.Store.GetByKey(svcKey)
				if err != nil {
					glog.Infof("error getting service %v from the cache: %v", svcKey, err)
					continue
				}

				if !svcExists {
					glog.Warningf("service %v does no exists", svcKey)
					continue
				}

				svc := svcObj.(*api.Service)
				glog.V(3).Infof("obtaining port information for service %v", svcKey)
				bp := path.Backend.ServicePort.String()

				svcBackendConfig := backendConfig[path.Backend.ServiceName]

				for _, servicePort := range svc.Spec.Ports {
					// targetPort could be a string, use the name or the port (int)
					if strconv.Itoa(int(servicePort.Port)) == bp || servicePort.TargetPort.String() == bp || servicePort.Name == bp {
						portBackendConfig, ok := svcBackendConfig[bp]
						if ok {
							glog.Infof("use port backend configuration for service %v: %+v", svcKey, svcBackendConfig)
							switch portBackendConfig.Proto {
							case "h2", "http/1.1":
							default:
								glog.Errorf("unrecognized backend protocol %v for service %v, port %v", portBackendConfig.Proto, svcKey, bp)
								portBackendConfig.Proto = "http/1.1"
							}
						} else {
							portBackendConfig = defaultPortBackendConfig()
						}

						endps := lbc.getEndpoints(svc, servicePort.TargetPort, api.ProtocolTCP, &portBackendConfig)
						if len(endps) == 0 {
							glog.Warningf("service %v does no have any active endpoints", svcKey)
							break
						}

						ups.Backends = append(ups.Backends, endps...)
						break
					}
				}

				if len(ups.Backends) == 0 {
					glog.Warningf("no backend service port found for service %v", svcKey)
					continue
				}

				upstreams = append(upstreams, ups)
			}
		}
	}

	// find default backend.  If only it is not found, use default backend.  This is useful to override default backend with ingress.
	defaultUpstreamFound := false

	for _, upstream := range upstreams {
		if upstream.Host == "" && (upstream.Path == "" || upstream.Path == "/") {
			defaultUpstreamFound = true
			break
		}
	}

	if !defaultUpstreamFound {
		upstreams = append(upstreams, lbc.getDefaultUpstream())
	}

	sort.Sort(nghttpx.UpstreamByNameServers(upstreams))

	for _, value := range upstreams {
		sort.Sort(nghttpx.UpstreamServerByAddrPort(value.Backends))

		// remove duplicate UpstreamServer
		uniqBackends := []nghttpx.UpstreamServer{value.Backends[0]}
		for _, sv := range value.Backends[1:] {
			lastBackend := &uniqBackends[len(uniqBackends)-1]

			if lastBackend.Address == sv.Address && lastBackend.Port == sv.Port {
				continue
			}

			uniqBackends = append(uniqBackends, sv)
		}

		value.Backends = uniqBackends
	}

	return upstreams, server
}

func (lbc *loadBalancerController) getPemsFromIngress(data []interface{}) []nghttpx.TLSCred {
	pems := []nghttpx.TLSCred{}

	for _, ingIf := range data {
		ing := ingIf.(*extensions.Ingress)

		for _, tls := range ing.Spec.TLS {
			secretName := tls.SecretName
			secretKey := fmt.Sprintf("%s/%s", ing.Namespace, secretName)
			secretInterface, exists, err := lbc.secrLister.Store.GetByKey(secretKey)
			if err != nil {
				glog.Warningf("Error retriveing secret %v for ing %v: %v", secretKey, ing.Name, err)
				continue
			}
			if !exists {
				glog.Warningf("Secret %v does not exist", secretKey)
				continue
			}
			secret := secretInterface.(*api.Secret)
			cert, ok := secret.Data[api.TLSCertKey]
			if !ok {
				glog.Warningf("Secret %v has no cert", secretKey)
				continue
			}
			key, ok := secret.Data[api.TLSPrivateKeyKey]
			if !ok {
				glog.Warningf("Secret %v has no private key", secretKey)
				continue
			}

			if _, err := nghttpx.CommonNames(cert); err != nil {
				glog.Errorf("No valid SSL certificate found in secret %v: %v", secretKey, err)
				continue
			}

			if err := nghttpx.CheckPrivateKey(key); err != nil {
				glog.Errorf("No valid SSL private key found in secret %v: %v", secretKey, err)
				continue
			}

			tlsCred, err := lbc.nghttpx.AddOrUpdateCertAndKey(fmt.Sprintf("%v-%v", ing.Namespace, secretName), cert, key)
			if err != nil {
				glog.Errorf("Could not create private key and certificate files %v: %v", secretKey, err)
				continue
			}

			pems = append(pems, tlsCred)
		}
	}

	return pems
}

func (lbc *loadBalancerController) secrReferenced(namespace, name string) bool {
	for _, ingIf := range lbc.ingLister.Store.List() {
		ing := ingIf.(*extensions.Ingress)
		if ing.Namespace != namespace {
			continue
		}
		for _, tls := range ing.Spec.TLS {
			if tls.SecretName == name {
				return true
			}
		}
	}
	return false
}

// getEndpoints returns a list of <endpoint ip>:<port> for a given
// service/target port combination.  portBackendConfig is additional
// per-port configuration for backend, which must not be nil.
func (lbc *loadBalancerController) getEndpoints(s *api.Service, servicePort intstr.IntOrString, proto api.Protocol, portBackendConfig *PortBackendConfig) []nghttpx.UpstreamServer {
	glog.V(3).Infof("getting endpoints for service %v/%v and port %v", s.Namespace, s.Name, servicePort.String())
	ep, err := lbc.endpLister.GetServiceEndpoints(s)
	if err != nil {
		glog.Warningf("unexpected error obtaining service endpoints: %v", err)
		return []nghttpx.UpstreamServer{}
	}

	upsServers := []nghttpx.UpstreamServer{}

	for _, ss := range ep.Subsets {
		for _, epPort := range ss.Ports {

			if !reflect.DeepEqual(epPort.Protocol, proto) {
				continue
			}

			var targetPort int
			switch servicePort.Type {
			case intstr.Int:
				if int(epPort.Port) == servicePort.IntValue() {
					targetPort = int(epPort.Port)
				}
			case intstr.String:
				val, ok := serviceAnnotation(s.ObjectMeta.Annotations).getPort(servicePort.StrVal)
				if ok {
					port, err := strconv.Atoi(val)
					if err != nil {
						glog.Warningf("%v is not valid as a port", val)
						continue
					}

					targetPort = port
				} else {
					newnp, err := lbc.checkSvcForUpdate(s)
					if err != nil {
						glog.Warningf("error mapping service ports: %v", err)
						continue
					}
					val, ok := serviceAnnotation(newnp).getPort(servicePort.StrVal)
					if ok {
						port, err := strconv.Atoi(val)
						if err != nil {
							glog.Warningf("%v is not valid as a port", val)
							continue
						}

						targetPort = port
					}
				}
			}

			if targetPort == 0 {
				continue
			}

			for _, epAddress := range ss.Addresses {
				ups := nghttpx.UpstreamServer{
					Address:  epAddress.IP,
					Port:     fmt.Sprintf("%v", targetPort),
					Protocol: portBackendConfig.Proto,
					TLS:      portBackendConfig.TLS,
					SNI:      portBackendConfig.SNI,
				}
				upsServers = append(upsServers, ups)
			}
		}
	}

	glog.V(3).Infof("endpoints found: %+v", upsServers)
	return upsServers
}

// Stop stops the loadbalancer controller.
func (lbc *loadBalancerController) Stop() error {
	// Stop is invoked from the http endpoint.
	lbc.stopLock.Lock()
	defer lbc.stopLock.Unlock()

	// Only try draining the workqueue if we haven't already.
	if !lbc.shutdown {
		lbc.shutdown = true
		close(lbc.stopCh)

		ings := lbc.ingLister.Store.List()
		glog.Infof("removing IP address %v from ingress rules", lbc.podInfo.NodeIP)
		lbc.removeFromIngress(ings)

		glog.Infof("Shutting down controller queues")
		lbc.syncQueue.shutdown()
		lbc.ingQueue.shutdown()

		return nil
	}

	return fmt.Errorf("shutdown already in progress")
}

func (lbc *loadBalancerController) removeFromIngress(ings []interface{}) {
	glog.Infof("updating %v Ingress rule/s", len(ings))
	for _, cur := range ings {
		ing := cur.(*extensions.Ingress)

		ingClient := lbc.client.Extensions().Ingress(ing.Namespace)
		currIng, err := ingClient.Get(ing.Name)
		if err != nil {
			glog.Errorf("unexpected error searching Ingress %v/%v: %v", ing.Namespace, ing.Name, err)
			continue
		}

		lbIPs := ing.Status.LoadBalancer.Ingress
		if len(lbIPs) > 0 && lbc.isStatusIPDefined(lbIPs) {
			glog.Infof("Updating loadbalancer %v/%v. Removing IP %v", ing.Namespace, ing.Name, lbc.podInfo.NodeIP)

			for idx, lbStatus := range currIng.Status.LoadBalancer.Ingress {
				if lbStatus.IP == lbc.podInfo.NodeIP {
					currIng.Status.LoadBalancer.Ingress = append(currIng.Status.LoadBalancer.Ingress[:idx],
						currIng.Status.LoadBalancer.Ingress[idx+1:]...)
					break
				}
			}

			if _, err := ingClient.UpdateStatus(currIng); err != nil {
				lbc.recorder.Eventf(currIng, api.EventTypeWarning, "UPDATE", "error: %v", err)
				continue
			}

			lbc.recorder.Eventf(currIng, api.EventTypeNormal, "DELETE", "ip: %v", lbc.podInfo.NodeIP)
		}
	}
}

// Run starts the loadbalancer controller.
func (lbc *loadBalancerController) Run() {
	glog.Infof("starting nghttpx loadbalancer controller")
	lbc.nghttpx.Start()

	go lbc.ingController.Run(lbc.stopCh)
	go lbc.endpController.Run(lbc.stopCh)
	go lbc.svcController.Run(lbc.stopCh)
	go lbc.secrController.Run(lbc.stopCh)
	go lbc.mapController.Run(lbc.stopCh)

	go lbc.syncQueue.run(time.Second, lbc.stopCh)
	go lbc.ingQueue.run(time.Second, lbc.stopCh)

	<-lbc.stopCh
}

func defaultPortBackendConfig() PortBackendConfig {
	return PortBackendConfig{
		Proto: "http/1.1",
	}
}