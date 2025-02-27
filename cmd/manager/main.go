/*
Copyright 2018 The Kubernetes Authors.

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

package main

import (
	"flag"
	"net/http"
	_ "net/http/pprof"
	"time"

	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/klog"
	"k8s.io/klog/klogr"
	capa "sigs.k8s.io/cluster-api-provider-aws/pkg/apis"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/actuators/cluster"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/controller"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/record"
	capi "sigs.k8s.io/cluster-api/pkg/apis"
	"sigs.k8s.io/cluster-api/pkg/apis/cluster/common"
	"sigs.k8s.io/cluster-api/pkg/client/clientset_generated/clientset"
	capicluster "sigs.k8s.io/cluster-api/pkg/controller/cluster"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/runtime/signals"
)

func main() {
	klog.InitFlags(nil)
	flag.Set("logtostderr", "true")

	watchNamespace := flag.String("namespace", "",
		"Namespace that the controller watches to reconcile cluster-api objects. If unspecified, the controller watches for cluster-api objects across all namespaces.")

	profilerAddress := flag.String("profiler-address", "", "Bind address to expose the pprof profiler (e.g. localhost:6060)")

	metricsAddress := flag.String("metrics-address", "", "Bind address to expose the metrics (e.g. localhost:8080)")

	flag.Parse()
	if *watchNamespace != "" {
		klog.Infof("Watching cluster-api objects only in namespace %q for reconciliation", *watchNamespace)
	}

	if *profilerAddress != "" {
		klog.Infof("Profiler listening for requests at %s", *profilerAddress)
		go func() {
			klog.Info(http.ListenAndServe(*profilerAddress, nil))
		}()
	}

	// Setup a Manager
	syncPeriod := 10 * time.Minute

	// Setup controller-runtime logger.
	log.SetLogger(klogr.New())

	// Get a config to talk to the api-server.
	cfg := config.GetConfigOrDie()
	mgr, err := manager.New(cfg, manager.Options{
		SyncPeriod:         &syncPeriod,
		Namespace:          *watchNamespace,
		MetricsBindAddress: *metricsAddress,
	})
	if err != nil {
		klog.Fatalf("Failed to set up overall controller manager: %v", err)
	}

	cs, err := clientset.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("Failed to create client from configuration: %v", err)
	}

	coreClient, err := corev1.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("Failed to create corev1 client from configuration: %v", err)
	}

	// Initialize event recorder.
	record.InitFromRecorder(mgr.GetEventRecorderFor("aws-controller"))

	// Initialize cluster actuator.
	clusterActuator := cluster.NewActuator(cluster.ActuatorParams{
		Client:         mgr.GetClient(),
		CoreClient:     coreClient,
		ClusterClient:  cs.ClusterV1alpha2(),
		LoggingContext: "[cluster-actuator]",
	})

	// Register our cluster deployer (the interface is in clusterctl and we define the Deployer interface on the actuator)
	common.RegisterClusterProvisioner("aws", clusterActuator)

	if err := capi.AddToScheme(mgr.GetScheme()); err != nil {
		klog.Fatal(err)
	}

	if err := capa.AddToScheme(mgr.GetScheme()); err != nil {
		klog.Fatal(err)
	}

	capicluster.AddWithActuator(mgr, clusterActuator)

	// Setup all Controllers.
	if err := controller.AddToManager(mgr); err != nil {
		klog.Fatal(err)
	}

	if err := mgr.Start(signals.SetupSignalHandler()); err != nil {
		klog.Fatalf("Failed to run manager: %v", err)
	}
}
