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
	"fmt"
	"net/http"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentserver"
	"github.com/spawnery/spawnery/internal/certs"
	"github.com/spawnery/spawnery/internal/controller"
	"github.com/spawnery/spawnery/internal/grpcauth"
	"github.com/spawnery/spawnery/internal/podspec"
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

// agentEndpoint is the address a game server pod dials. It is the Service, not
// the pod: the gRPC endpoint only runs on the leader, and the Service is what
// keeps a standby out of the way.
func agentEndpoint(namespace string) string {
	return fmt.Sprintf("%s.%s.svc:%d", podspec.AgentServiceName, namespace, agentserver.DefaultPort)
}

// validateAgentFlags rejects a configuration that would only fail much later
// and somewhere else.
func validateAgentFlags(operatorNamespace string, renewAfter, hardDeadline time.Duration) error {
	if operatorNamespace == "" {
		return fmt.Errorf("--operator-namespace is empty and POD_NAMESPACE is unset: " +
			"without it the serving certificate would carry the wrong names and the agents " +
			"would be told to dial the wrong address")
	}
	if renewAfter >= hardDeadline {
		return fmt.Errorf("--agent-session-renew-after (%s) must be below --agent-session-deadline (%s), "+
			"or the operator would cut every stream off mid-renewal", renewAfter, hardDeadline)
	}
	return nil
}

// leaderReadyCheck reports ready only once this replica holds the leader lock.
//
// The agent gRPC service is a leader-bound runnable, so a standby serves
// nothing on port 9443. Were it a ready endpoint of the Service anyway, agents
// would land on it, fill a registry no controller reads, and their servers
// would never reach Ready. The check must not block: kubelet probes it on a
// timer, and a standby has to answer "no" promptly rather than hang until it
// is elected.
func leaderReadyCheck(elected <-chan struct{}) healthz.Checker {
	return func(_ *http.Request) error {
		select {
		case <-elected:
			return nil
		default:
			return fmt.Errorf("not the leader yet")
		}
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
		operatorNamespace    string
		agentBindAddress     string
		renewAfter           time.Duration
		hardDeadline         time.Duration
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
	flag.StringVar(&operatorNamespace, "operator-namespace", os.Getenv("POD_NAMESPACE"),
		"namespace the operator runs in; holds the TLS secret and the agent service")
	flag.StringVar(&agentBindAddress, "agent-bind-address", fmt.Sprintf(":%d", agentserver.DefaultPort),
		"address the agent gRPC endpoint binds to")
	flag.DurationVar(&renewAfter, "agent-session-renew-after", 8*time.Minute,
		"when an agent should open a fresh stream; must be below the hard deadline")
	flag.DurationVar(&hardDeadline, "agent-session-deadline", 10*time.Minute,
		"when the operator closes an agent stream regardless")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")
	setupLog.Info("starting spawnery-operator", "version", version.Version)

	if err := validateAgentFlags(operatorNamespace, renewAfter, hardDeadline); err != nil {
		setupLog.Error(err, "refusing to start")
		os.Exit(1)
	}

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
	// The bootstrap touches exactly two kinds of object, one per namespace,
	// and both carry our label. Without this restriction the cache would hold
	// every ConfigMap in the cluster — kube-root-ca.crt from every namespace
	// included — for the sake of one per namespace that is ours.
	managed := labels.SelectorFromSet(labels.Set{podspec.LabelManagedBy: podspec.ManagedByValue})
	mgrOptions.Cache.ByObject = map[client.Object]cache.ByObject{
		&corev1.ConfigMap{}:      {Label: managed},
		&corev1.ServiceAccount{}: {Label: managed},
	}

	restConfig := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(restConfig, mgrOptions)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// The TLS bundle is read and written directly, never through the cache: a
	// cached Secret would mean an informer over every Secret in the cluster,
	// and the operator's role deliberately grants no list or watch on them.
	directClient, err := client.New(restConfig, client.Options{
		Scheme:     scheme,
		Mapper:     mgr.GetRESTMapper(),
		HTTPClient: mgr.GetHTTPClient(),
	})
	if err != nil {
		setupLog.Error(err, "unable to build the uncached client")
		os.Exit(1)
	}

	provider := certs.NewProvider(&certs.Store{
		Client:    directClient,
		Namespace: operatorNamespace,
		Name:      certs.SecretName,
		DNSNames:  certs.ServingDNSNames(podspec.AgentServiceName, operatorNamespace),
		Clock:     time.Now,
	})
	if err := mgr.Add(provider); err != nil {
		setupLog.Error(err, "unable to add the certificate provider")
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		setupLog.Error(err, "unable to build the Kubernetes clientset")
		os.Exit(1)
	}

	started := time.Now()
	registry := agent.New(time.Now, reportInterval, started)

	if err := mgr.Add(agentserver.New(agentserver.Options{
		Addr:     agentBindAddress,
		Provider: provider,
		Auth: &grpcauth.Authenticator{
			Reviews:  clientset.AuthenticationV1().TokenReviews(),
			Pods:     &grpcauth.ClientPodChecker{Client: mgr.GetClient()},
			Audience: podspec.AgentTokenAudience,
		},
		Agents:         registry,
		ReportInterval: reportInterval,
		RenewAfter:     renewAfter,
		HardDeadline:   hardDeadline,
		Clock:          time.Now,
	})); err != nil {
		setupLog.Error(err, "unable to add the agent endpoint")
		os.Exit(1)
	}

	if err := controller.SetupAll(mgr, controller.Options{
		Agents:               registry,
		Clock:                time.Now,
		StartupDeadline:      startupDeadline,
		PlayerStatusInterval: playerStatusInterval,
		OrphanInterval:       orphanInterval,
		// Milestone 3 replaces this with the proxy broadcast.
		Registrar: controller.NoopRegistrar{},
		Bootstrapper: &controller.Bootstrapper{
			Client: mgr.GetClient(),
			Reader: mgr.GetAPIReader(),
			CA:     provider.CABundle,
		},
		AgentEndpoint: agentEndpoint(operatorNamespace),
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
	if err := mgr.AddReadyzCheck("leader", leaderReadyCheck(mgr.Elected())); err != nil {
		setupLog.Error(err, "unable to add ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager", "agentEndpoint", agentEndpoint(operatorNamespace))
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "manager exited with an error")
		os.Exit(1)
	}
}
