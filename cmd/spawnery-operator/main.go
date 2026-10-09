/*
Copyright paul_wtf.

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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
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
	"github.com/spawnery/spawnery/internal/cloudevent"
	"github.com/spawnery/spawnery/internal/controller"
	"github.com/spawnery/spawnery/internal/grpcauth"
	"github.com/spawnery/spawnery/internal/netstate"
	"github.com/spawnery/spawnery/internal/netstatus"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/proxyreg"
	"github.com/spawnery/spawnery/internal/rbacaudit"
	"github.com/spawnery/spawnery/internal/serverreg"
	"github.com/spawnery/spawnery/internal/version"
	"github.com/spawnery/spawnery/internal/worldsync"
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

// agentEndpoint is the Service, not the pod: the gRPC endpoint only runs on
// the leader.
func agentEndpoint(namespace string) string {
	return fmt.Sprintf("%s.%s.svc:%d", podspec.AgentServiceName, namespace, agentserver.DefaultPort)
}

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
	if token := time.Duration(podspec.TokenExpirationSeconds) * time.Second; hardDeadline > token {
		return fmt.Errorf("--agent-session-deadline (%s) is above the agent token's lifetime (%s): "+
			"a stream is authenticated once, so a stolen token would stay useful past its expiry",
			hardDeadline, token)
	}
	return nil
}

// rescueWindowWarning is empty unless the rescue window for a dead node is
// shorter than one resync. A warning, not a refusal: everything else the
// report interval governs still works.
func rescueWindowWarning(reportInterval time.Duration) string {
	// Zero means the shipped read timeout; what a proxy actually reports
	// reaches the Network's RescueWindowShort condition.
	window := phase.RescueWindow(reportInterval, 0)
	if window >= controller.ResyncInterval {
		return ""
	}
	if window <= 0 {
		return fmt.Sprintf(
			"--report-interval %s leaves no room at all to move players off a backend whose "+
				"node has died: a count goes stale after %s, and Velocity disconnects them "+
				"itself at %s. The operator would deregister the server after the kick "+
				"rather than before it",
			reportInterval, 2*reportInterval, phase.VelocityReadTimeout)
	}
	return fmt.Sprintf(
		"--report-interval %s leaves %s to move players off a backend whose node has died, "+
			"which is less than the %s resync the operator acts on -- so the drain may be "+
			"decided after Velocity has already disconnected them",
		reportInterval, window, controller.ResyncInterval)
}

// taintKeys collects the repeatable --drain-taint flag.
type taintKeys []string

func (t *taintKeys) String() string { return strings.Join(*t, ",") }

func (t *taintKeys) Set(value string) error {
	if value == "" {
		return fmt.Errorf("an empty taint key would match nothing")
	}
	// A whole key=value:Effect would match no taint, and nothing would ever
	// drain. A well-formed key absent from the cluster still cannot be told
	// from a typo.
	if errs := validation.IsQualifiedName(value); len(errs) > 0 {
		return fmt.Errorf(
			"%q is not a taint key: %s. This flag takes the key alone -- `node.kubernetes.io/unreachable`, "+
				"not `node.kubernetes.io/unreachable=true:NoExecute` -- because the effect is read "+
				"from the node's own taint and the value is not compared at all",
			value, strings.Join(errs, "; "))
	}
	*t = append(*t, value)
	return nil
}

// leaderReadyCheck reports ready only once this replica holds the leader lock:
// a standby serves no agent endpoint and must stay out of the Service. It must
// not block until election.
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

type managerFlags struct {
	metricsAddr       string
	probeAddr         string
	leaderElect       bool
	watchNamespace    string
	operatorNamespace string
}

func managerOptions(f managerFlags) manager.Options {
	opts := manager.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: f.metricsAddr},
		HealthProbeBindAddress: f.probeAddr,
		LeaderElection:         f.leaderElect,
		LeaderElectionID:       "spawnery-operator.spawnery.cloud",
		// Left empty, controller-runtime reads it from the ServiceAccount
		// mount, which a local `go run` does not have.
		LeaderElectionNamespace: f.operatorNamespace,
	}
	if f.watchNamespace != "" {
		opts.Cache.DefaultNamespaces = map[string]cache.Config{f.watchNamespace: {}}
	}
	// Without the label restrictions the cache would hold every ConfigMap,
	// ServiceAccount and claim in the cluster.
	managed := labels.SelectorFromSet(labels.Set{podspec.LabelManagedBy: podspec.ManagedByValue})
	opts.Cache.ByObject = map[client.Object]cache.ByObject{
		&corev1.ConfigMap{}:      {Label: managed},
		&corev1.ServiceAccount{}: {Label: managed},
		// A claim missing the label is invisible to growClaim and
		// readResizePending.
		&corev1.PersistentVolumeClaim{}: {Label: managed},
		// status.images is tens of kilobytes per node and nothing reads it.
		&corev1.Node{}: {
			Transform: func(obj any) (any, error) {
				if node, ok := obj.(*corev1.Node); ok {
					node.Status.Images = nil
				}
				return obj, nil
			},
		},
	}
	return opts
}

func main() {
	var (
		metricsAddr             string
		probeAddr               string
		leaderElect             bool
		watchNamespace          string
		reportInterval          time.Duration
		startupDeadline         time.Duration
		playerStatusInterval    time.Duration
		orphanInterval          time.Duration
		operatorNamespace       string
		agentBindAddress        string
		permissionCheckInterval time.Duration
		renewAfter              time.Duration
		hardDeadline            time.Duration
		drainTaints             taintKeys
		allowPluginVolumes      bool
		allowFileVolumes        bool
		allowMountVolumes       bool
		aotCache                bool
		worldSync               bool
		worldSyncInterval       time.Duration
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
	flag.DurationVar(&permissionCheckInterval, "permission-check-interval", rbacaudit.DefaultCheckInterval,
		"how often the operator asks the API server whether it still has the permissions it needs. "+
			"A negative value checks once at startup and never again, which is what it did before "+
			"0.2.4; the cost of the repeat is 73 SelfSubjectAccessReviews, measured at 54ms.")
	flag.StringVar(&agentBindAddress, "agent-bind-address", fmt.Sprintf(":%d", agentserver.DefaultPort),
		"address the agent gRPC endpoint binds to")
	flag.DurationVar(&renewAfter, "agent-session-renew-after", 8*time.Minute,
		"when an agent should open a fresh stream; must be below the hard deadline")
	flag.DurationVar(&hardDeadline, "agent-session-deadline", 10*time.Minute,
		"when the operator closes an agent stream regardless")
	flag.Var(&drainTaints, "drain-taint",
		"taint key that marks a node as departing, beside spec.unschedulable; repeatable. "+
			"A bare key, not key=value:Effect -- the value is ignored and only NoSchedule and "+
			"NoExecute are honoured. A key that is simply absent from the cluster cannot be told "+
			"from a typo by anything here, so confirm with `kubectl describe node` that the taint "+
			"is present with one of those two effects; the operator warns only for the well-known "+
			"keys it recognises.")

	flag.BoolVar(&allowPluginVolumes, "allow-plugin-volumes", false,
		"let a group name a spec.extraPlugins claim, whose contents are copied "+
			"into every server's plugins directory on start. Off by default -- an "+
			"installation that has not turned this on refuses a group that names a "+
			"claim, so \"this cluster runs no third-party plugins\" is a fact rather "+
			"than a convention. It is not a security boundary: a PersistentVolumeClaim "+
			"is a namespaced object in the same trust domain as the group that names "+
			"it. Until 0.2.x this flag also governed spec.mounts; that is now "+
			"--allow-mount-volumes.")

	flag.BoolVar(&allowFileVolumes, "allow-file-volumes", false,
		"let a group name a spec.extraFiles claim whose tree is copied into "+
			"every server's working directory on start. Not a security control: "+
			"a claim is a namespaced object in the same trust domain as the group "+
			"naming it. It lets an installation say it runs no administrator-supplied files.")

	flag.BoolVar(&allowMountVolumes, "allow-mount-volumes", false,
		"let a group's spec.mounts name a PersistentVolumeClaim. Not a security "+
			"control: a claim is a namespaced object in the same trust domain as the "+
			"group naming it. Until 0.2.x this was governed by --allow-plugin-volumes, "+
			"which now governs only spec.extraPlugins.")

	flag.BoolVar(&aotCache, "aot-cache", false,
		"mount the startup cache published with spawnery's own Purpur images "+
			"(0.23.0 and later) into their servers, as an image volume. Needs Kubernetes "+
			"with the ImageVolume feature and a runtime that supports it; without them the "+
			"API server drops the volume's source and refuses every server pod. Off by default.")

	flag.BoolVar(&worldSync, "world-sync", false,
		"serve groups with spec.storage.backend ObjectStore; needs spawnery-worldsync on the nodes "+
			"and WORLDSYNC_ENDPOINT, WORLDSYNC_REGION, WORLDSYNC_BUCKET, AWS_ACCESS_KEY_ID, "+
			"AWS_SECRET_ACCESS_KEY in the environment; WORLDSYNC_PREFIX, if set, is the key prefix in the bucket")
	flag.DurationVar(&worldSyncInterval, "world-sync-snapshot-interval", 5*time.Minute,
		"how often a member of an ObjectStore group asks for a snapshot; the most play a node loss costs")

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

	mgrOptions := managerOptions(managerFlags{
		metricsAddr:       metricsAddr,
		probeAddr:         probeAddr,
		leaderElect:       leaderElect,
		watchNamespace:    watchNamespace,
		operatorNamespace: operatorNamespace,
	})
	restConfig := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(restConfig, mgrOptions)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Secrets bypass the cache: the role grants no list or watch on them.
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
		// No cloudevent.Recorder: the operator's own namespace has no agents.
		Recorder: mgr.GetEventRecorder("certs"),
		// A CA rotation waits out the stream deadline before switching the
		// serving certificate, so it must be the same value.
		AgentSessionDeadline: hardDeadline,
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

	if err := mgr.Add(&rbacaudit.Checker{
		Reviewer: clientset.AuthorizationV1().SelfSubjectAccessReviews(),
		Scopes:   rbacaudit.DefaultScopes(operatorNamespace),
		Interval: permissionCheckInterval,
	}); err != nil {
		setupLog.Error(err, "unable to add the permission self-check")
		os.Exit(1)
	}

	if warning := rescueWindowWarning(reportInterval); warning != "" {
		setupLog.Info("the rescue window for a dead node is short", "warning", warning)
	}

	started := time.Now()
	registry := agent.New(time.Now, reportInterval, started)

	// One Source for both fan-outs, so backends and proxies get the same
	// picture.
	state := netstate.Source{Reader: mgr.GetClient(), Agents: registry}

	proxies := proxyreg.New(proxyreg.Options{Reader: mgr.GetClient(), State: state})

	servers := serverreg.New(serverreg.Options{State: state})
	if err := mgr.Add(servers); err != nil {
		setupLog.Error(err, "unable to add the server fanout")
		os.Exit(1)
	}
	if err := mgr.Add(proxies); err != nil {
		setupLog.Error(err, "unable to add the proxy resync")
		os.Exit(1)
	}

	fleet := &agentserver.FleetCounter{Pods: mgr.GetClient()}
	if err := mgr.Add(fleet); err != nil {
		setupLog.Error(err, "unable to add the fleet counter")
		os.Exit(1)
	}

	var worlds agentserver.WorldDeleter
	// An interface left nil, not a nil *Policies, when world sync is off.
	var retention controller.RetentionPublisher
	if worldSync {
		cfg, base, err := worldsync.S3ConfigFromEnv(os.Getenv)
		if err != nil {
			setupLog.Error(err, "--world-sync needs the bucket")
			os.Exit(1)
		}
		st, err := worldsync.NewS3Store(cfg)
		if err != nil {
			setupLog.Error(err, "world sync store")
			os.Exit(1)
		}
		worlds = worldsync.BucketWorlds{Store: st, Base: base}
		retention = &worldsync.Policies{Store: st, Base: base}
		if err := mgr.Add(&worldsync.Sweeper{Store: st, Base: base, Interval: time.Minute,
			StaleAfter: worldsync.StaleAfter, Log: ctrl.Log.WithName("worldsync")}); err != nil {
			setupLog.Error(err, "add world deletion sweeper")
			os.Exit(1)
		}
	}

	if err := mgr.Add(agentserver.New(agentserver.Options{
		Addr:     agentBindAddress,
		Provider: provider,
		Auth: &grpcauth.Authenticator{
			Reviews:  clientset.AuthenticationV1().TokenReviews(),
			Pods:     &grpcauth.ClientPodChecker{Client: mgr.GetClient()},
			Audience: podspec.AgentTokenAudience,
			Cache:    grpcauth.NewReviewCache(time.Now),
			Limiter:  grpcauth.NewPeerLimiter(time.Now),
		},
		Agents:  registry,
		Proxies: proxies,
		Servers: servers,
		State:   state,
		Writer:  agentserver.KubeWriter{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Clock: time.Now, Worlds: worlds},
		Status: netstatus.Source{
			Reader:  mgr.GetClient(),
			Agents:  registry,
			Metrics: netstatus.APIMetrics{REST: clientset.Discovery().RESTClient()},
			Clock:   time.Now,
			Log:     ctrl.Log.WithName("netstatus"),
		},
		Recorder:       mgr.GetEventRecorder("agentserver"),
		Fleet:          fleet.Size,
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
		Events:               cloudevent.Fanout{Backends: servers, Proxies: proxies},
		AllowPluginVolumes:   allowPluginVolumes,
		AllowFileVolumes:     allowFileVolumes,
		AllowMountVolumes:    allowMountVolumes,
		AOTCache:             aotCache,
		WorldSync:            worldSync,
		WorldSyncInterval:    worldSyncInterval,
		Retention:            retention,
		ReportInterval:       reportInterval,
		Clock:                time.Now,
		StartupDeadline:      startupDeadline,
		PlayerStatusInterval: playerStatusInterval,
		OrphanInterval:       orphanInterval,
		Registrar:            proxies,
		Bootstrapper: &controller.Bootstrapper{
			Client: mgr.GetClient(),
			Reader: mgr.GetAPIReader(),
			CA:     provider.CABundle,
		},
		AgentEndpoint:     agentEndpoint(operatorNamespace),
		OperatorNamespace: operatorNamespace,
		Proxies:           proxies,
		DrainTaintKeys:    drainTaints,
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
