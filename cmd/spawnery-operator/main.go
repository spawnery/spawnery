/*
Copyright The Spawnery Authors.

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

// Command spawnery-operator runs the Spawnery controllers.
package main

import (
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/controller"
	"github.com/spawnery/spawnery/internal/version"
)

var scheme = runtime.NewScheme()

func init() {
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := spawneryv1alpha1.AddToScheme(scheme); err != nil {
		panic(err)
	}
}

func main() {
	var (
		metricsAddr          string
		probeAddr            string
		leaderElect          bool
		watchNamespace       string
		reportInterval       time.Duration
		startupDeadline      time.Duration
		playerStatusInterval time.Duration
		orphanInterval       time.Duration
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "address the metrics endpoint binds to")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "address the probe endpoint binds to")
	flag.BoolVar(&leaderElect, "leader-elect", true,
		"run leader election; on from the start so extra replicas are not an architecture change later")
	flag.StringVar(&watchNamespace, "namespace", "",
		"namespace to watch; empty means all namespaces")
	flag.DurationVar(&reportInterval, "report-interval", 5*time.Second,
		"how often agents report; a count older than twice this counts as stale")
	flag.DurationVar(&startupDeadline, "startup-deadline", 5*time.Minute,
		"how long a server may take to reach Ready before it counts as failed")
	flag.DurationVar(&playerStatusInterval, "player-status-interval", 30*time.Second,
		"how often unchanged player counts are written into the CR status")
	flag.DurationVar(&orphanInterval, "orphan-interval", controller.DefaultOrphanInterval,
		"how often the orphan sweep runs")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")
	setupLog.Info("starting spawnery-operator", "version", version.Version)

	mgrOptions := manager.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "spawnery-operator.spawnery.cloud",
	}
	if watchNamespace != "" {
		mgrOptions.Cache.DefaultNamespaces = map[string]cache.Config{watchNamespace: {}}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), mgrOptions)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	started := time.Now()
	registry := agent.New(time.Now, reportInterval, started)

	if err := controller.SetupAll(mgr, controller.Options{
		Agents:               registry,
		Clock:                time.Now,
		StartupDeadline:      startupDeadline,
		PlayerStatusInterval: playerStatusInterval,
		OrphanInterval:       orphanInterval,
		// Milestone 3 replaces this with the proxy broadcast.
		Registrar: controller.NoopRegistrar{},
	}); err != nil {
		setupLog.Error(err, "unable to set up controllers")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to add health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to add ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited with an error")
		os.Exit(1)
	}
}
