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
 * Copyright 2017, nghttpx Ingress controller contributors
 *
 * For the full copyright and license information, please view the LICENSE
 * file that was distributed with this source code.
 */

package main

import (
	"bytes"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"text/template"
	"time"

	"github.com/spf13/pflag"
	networking "k8s.io/api/networking/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/client-go/discovery"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"github.com/zlabjp/nghttpx-ingress-lb/pkg/controller"
	"github.com/zlabjp/nghttpx-ingress-lb/pkg/nghttpx"
)

func init() {
	klog.InitFlags(flag.CommandLine)
}

var (
	// value overwritten during build. This can be used to resolve issues.
	version = ""
	gitRepo = ""

	flags = pflag.NewFlagSet("", pflag.ExitOnError)

	defaultSvc = flags.String("default-backend-service", "",
		`(Required) Service used to serve a 404 page for the default backend. Takes the form
    namespace/name. The controller uses the first node port of this Service for
    the default backend.`)

	ngxConfigMap = flags.String("nghttpx-configmap", "",
		`Namespace/name of the ConfigMap that contains the custom nghttpx configuration to use.  Takes the form namespace/name.`)

	kubeconfig = flags.String("kubeconfig", "", `Path to kubeconfig file which overrides in-cluster configuration.`)

	resyncPeriod = flags.Duration("sync-period", 30*time.Second,
		`(deprecated) Resync resources this often.`)

	watchNamespace = flags.String("watch-namespace", metav1.NamespaceAll,
		`Namespace to watch for Ingress. Default is to watch all namespaces`)

	healthzPort = flags.Int("healthz-port", 11249, "port for healthz endpoint.")

	nghttpxHealthPort = flags.Int("nghttpx-health-port", 10901, "port for nghttpx health monitor endpoint.")

	nghttpxAPIPort = flags.Int("nghttpx-api-port", 10902, "port for nghttpx API endpoint.")

	profiling = flags.Bool("profiling", true, `Enable profiling via web interface host:port/debug/pprof/`)

	allowInternalIP = flags.Bool("allow-internal-ip", false, `Allow to use address of type NodeInternalIP when fetching
                external IP address. This is the workaround for the cluster configuration where NodeExternalIP or
                NodeLegacyHostIP is not assigned or cannot be used.`)

	defaultTLSSecret = flags.String("default-tls-secret", "",
		`Optional, name of the Secret that contains TLS server certificate and secret key to enable TLS by default.  For those client connections which are not TLS encrypted, they are redirected to https URI permanently.`)

	ingressClass = flags.String("ingress-class", "nghttpx",
		`Ingress class which this controller is responsible for.  This is the value of the deprecated "kubernetes.io/ingress.class" annotation.  For Kubernetes v1.18 or later, use ingress-class-controller flag and IngressClass resource.`)

	ingressClassController = flags.String("ingress-class-controller", "zlab.co.jp/nghttpx",
		`The name of IngressClass controller for this controller.  This is the value specified in IngressClass.spec.controller.  Only works with Kubernetes v1.18 or later `)

	nghttpxConfDir = flags.String("nghttpx-conf-dir", "/etc/nghttpx",
		`Path to the directory which contains nghttpx configuration files.  The controller reads and writes these configuration files.`)

	nghttpxExecPath = flags.String("nghttpx-exec-path", "/usr/local/bin/nghttpx",
		`Path to the nghttpx executable.`)

	nghttpxHTTPPort = flags.Int("nghttpx-http-port", 80,
		`Port to listen to for HTTP (non-TLS) requests.  Specifying 0 disables HTTP port.`)

	nghttpxHTTPSPort = flags.Int("nghttpx-https-port", 443,
		`Port to listen to for HTTPS (TLS) requests.  Specifying 0 disables HTTPS port.`)

	fetchOCSPRespFromSecret = flags.Bool("fetch-ocsp-resp-from-secret", false,
		`Fetch OCSP response from TLS secret.`)

	proxyProto = flags.Bool("proxy-proto", false,
		`Enable proxyproto for all public-facing frontends (api and health frontends are ignored)`)

	ocspRespKey = flags.String("ocsp-resp-key", "tls.ocsp-resp", `A key for OCSP response in TLS secret.`)

	publishSvc = flags.String("publish-service", "", `Specify namespace/name of Service whose hostnames/IP addresses are set in Ingress resource instead of addresses of Ingress controller Pods.  Takes the form namespace/name.`)

	endpointSlices = flags.Bool("endpoint-slices", false, `Get endpoints from EndpointSlice resource instead of Endpoints resource`)

	reloadRate = flags.Float64("reload-rate", 1.0, `Rate (QPS) of reloading nghttpx configuration to deal with frequent backend updates in a single batch`)

	reloadBurst = flags.Int("reload-burst", 1, `Reload burst that can exceed reload-rate`)

	configOverrides clientcmd.ConfigOverrides
)

func main() {
	// We use math/rand to choose interval of resync
	rand.Seed(time.Now().UTC().UnixNano())

	flags.AddGoFlagSet(flag.CommandLine)

	clientcmd.BindOverrideFlags(&configOverrides, flags, clientcmd.RecommendedConfigOverrideFlags(""))

	flags.Parse(os.Args)

	klog.Infof("Using build: %v - %v", gitRepo, version)

	var (
		defaultSvcKey       types.NamespacedName
		defaultTLSSecretKey *types.NamespacedName
		nghttpxConfigMapKey *types.NamespacedName
		publishSvcKey       *types.NamespacedName
	)

	if *defaultSvc == "" {
		klog.Exitf("default-backend-service cannot be empty")
	}
	if ns, name, err := cache.SplitMetaNamespaceKey(*defaultSvc); err != nil {
		klog.Exitf("default-backend-service: invalid Service identifier %v: %v", *defaultSvc, err)
	} else {
		defaultSvcKey = types.NamespacedName{
			Namespace: ns,
			Name:      name,
		}
	}

	if *publishSvc != "" {
		if ns, name, err := cache.SplitMetaNamespaceKey(*publishSvc); err != nil {
			klog.Exitf("publish-service: invalid Service identifier %v: %v", *publishSvc, err)
		} else {
			publishSvcKey = &types.NamespacedName{
				Namespace: ns,
				Name:      name,
			}
		}
	}

	if *ngxConfigMap != "" {
		if ns, name, err := cache.SplitMetaNamespaceKey(*ngxConfigMap); err != nil {
			klog.Exitf("nghttpx-configmap: invalid ConfigMap identifier %v: %v", *ngxConfigMap, err)
		} else {
			nghttpxConfigMapKey = &types.NamespacedName{
				Namespace: ns,
				Name:      name,
			}
		}
	}

	if *defaultTLSSecret != "" {
		if ns, name, err := cache.SplitMetaNamespaceKey(*defaultTLSSecret); err != nil {
			klog.Exitf("default-tls-secret: invalid Secret identifier %v: %v", *defaultTLSSecret, err)
		} else {
			defaultTLSSecretKey = &types.NamespacedName{
				Namespace: ns,
				Name:      name,
			}
		}
	}

	var (
		err    error
		config *rest.Config
	)

	if *kubeconfig == "" {
		config, err = rest.InClusterConfig()
	} else {
		loadingRules := clientcmd.ClientConfigLoadingRules{
			ExplicitPath: *kubeconfig,
		}
		config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(&loadingRules, &configOverrides).ClientConfig()
	}
	if err != nil {
		klog.Exitf("Could not get clientConfig: %v", err)
	}

	clientset, err := clientset.NewForConfig(config)
	if err != nil {
		klog.Exitf("Failed to create clientset: %v", err)
	}

	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		klog.Exitf("Failed to create discoveryClient: %v", err)
	}

	runtimePodInfo := &types.NamespacedName{
		Name:      os.Getenv("POD_NAME"),
		Namespace: os.Getenv("POD_NAMESPACE"),
	}

	if runtimePodInfo.Name == "" {
		klog.Exit("POD_NAME environment variable cannot be empty.")
	}
	if runtimePodInfo.Namespace == "" {
		klog.Exit("POD_NAMESPACE environment variable cannot be empty.")
	}

	controllerConfig := controller.Config{
		DefaultBackendService:   defaultSvcKey,
		WatchNamespace:          *watchNamespace,
		NghttpxConfigMap:        nghttpxConfigMapKey,
		NghttpxHealthPort:       *nghttpxHealthPort,
		NghttpxAPIPort:          *nghttpxAPIPort,
		NghttpxConfDir:          *nghttpxConfDir,
		NghttpxExecPath:         *nghttpxExecPath,
		NghttpxHTTPPort:         *nghttpxHTTPPort,
		NghttpxHTTPSPort:        *nghttpxHTTPSPort,
		DefaultTLSSecret:        defaultTLSSecretKey,
		IngressClass:            *ingressClass,
		IngressClassController:  *ingressClassController,
		EnableIngressClass:      checkIngressClassAvailability(discoveryClient),
		AllowInternalIP:         *allowInternalIP,
		OCSPRespKey:             *ocspRespKey,
		FetchOCSPRespFromSecret: *fetchOCSPRespFromSecret,
		ProxyProto:              *proxyProto,
		PublishSvc:              publishSvcKey,
		EnableEndpointSlice:     *endpointSlices,
		ReloadRate:              *reloadRate,
		ReloadBurst:             *reloadBurst,
	}

	if err := generateDefaultNghttpxConfig(*nghttpxConfDir, *nghttpxHealthPort, *nghttpxAPIPort); err != nil {
		klog.Exit(err)
	}

	lbc := controller.NewLoadBalancerController(clientset, nghttpx.NewManager(*nghttpxAPIPort), &controllerConfig, runtimePodInfo)

	go registerHandlers(lbc)
	go handleSigterm(lbc)

	lbc.Run()
}

// healthzChecker implements healthz.HealthzChecker interface.
type healthzChecker struct {
	// targetURI is the nghttpx health monitor endpoint.
	targetURI string
}

// newHealthzChecker returns new healthzChecker.
func newHealthzChecker(healthPort int) *healthzChecker {
	return &healthzChecker{
		targetURI: fmt.Sprintf("http://127.0.0.1:%v/healthz", healthPort),
	}
}

// Name returns the healthcheck name
func (hc healthzChecker) Name() string {
	return "nghttpx"
}

// Check returns if the nghttpx healthz endpoint is returning ok (status code 200)
func (hc healthzChecker) Check(_ *http.Request) error {
	res, err := http.Get(hc.targetURI)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		return fmt.Errorf("nghttpx is unhealthy")
	}

	return nil
}

func registerHandlers(lbc *controller.LoadBalancerController) {
	mux := http.NewServeMux()
	healthz.InstallHandler(mux, newHealthzChecker(*nghttpxHealthPort))

	http.HandleFunc("/build", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "build: %v - %v", gitRepo, version)
	})

	http.HandleFunc("/stop", func(w http.ResponseWriter, r *http.Request) {
		lbc.Stop()
	})

	if *profiling {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	}

	server := &http.Server{
		Addr:    fmt.Sprintf(":%v", *healthzPort),
		Handler: mux,
	}
	klog.Exit(server.ListenAndServe())
}

func handleSigterm(lbc *controller.LoadBalancerController) {
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGTERM)
	<-signalChan
	klog.Infof("Received SIGTERM, shutting down")

	lbc.Stop()
}

// generateDefaultNghttpxConfig generates default configuration file for nghttpx.
func generateDefaultNghttpxConfig(nghttpxConfDir string, nghttpxHealthPort, nghttpxAPIPort int) error {
	if err := nghttpx.MkdirAll(nghttpxConfDir); err != nil {
		return err
	}

	var buf bytes.Buffer
	t := template.Must(template.New("default.tmpl").ParseFiles("./default.tmpl"))
	if err := t.Execute(&buf, map[string]interface{}{
		"HealthPort": nghttpxHealthPort,
		"APIPort":    nghttpxAPIPort,
	}); err != nil {
		return fmt.Errorf("Could not create default configuration file for nghttpx: %v", err)
	}

	if err := nghttpx.WriteFile(nghttpx.NghttpxConfigPath(nghttpxConfDir), buf.Bytes()); err != nil {
		return fmt.Errorf("Could not create default configuration file for nghttpx: %v", err)
	}

	return nil
}

func checkIngressClassAvailability(d discovery.DiscoveryInterface) bool {
	resList, err := d.ServerResourcesForGroupVersion(networking.SchemeGroupVersion.String())
	if err != nil {
		klog.Exitf("Could not get Server resources %v", err)
	}

	for i := range resList.APIResources {
		r := &resList.APIResources[i]
		if r.Kind == "IngressClass" {
			return true
		}
	}

	klog.Infof("Server does not support %v IngressClass", networking.SchemeGroupVersion.String())

	return false
}
