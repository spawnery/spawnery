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
	"github.com/spawnery/spawnery/internal/controller"
	"github.com/spawnery/spawnery/internal/grpcauth"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/proxyreg"
	"github.com/spawnery/spawnery/internal/rbacaudit"
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

// rescueWindowWarning is the one comparison between the two numbers that decide
// whether a player on a dead node is moved or kicked, and the empty string when
// there is nothing to say.
//
// A warning and not a refusal. A report interval this high is a degradation and
// not a broken configuration -- every other thing the interval governs goes on
// working, and refusing to start would turn a fleet that loses a rescue into a
// fleet that loses everything. It is also not the operator's place to decide
// how much room is enough: the threshold here is one this code owns, namely
// that the operator can only act at a resync, so a window shorter than one
// resync is one it may spend entirely on not having looked yet.
//
// phase.RescueWindow says what the arithmetic is and what half of it the
// operator cannot see.
func rescueWindowWarning(reportInterval time.Duration) string {
	// Zero for the timeout, which means the value this repository ships. This
	// runs before any proxy has connected and is a statement about the flag
	// alone; what a proxy actually parsed reaches the Network's
	// RescueWindowShort condition instead, per namespace, once one has said.
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

// taintKeys collects a repeatable flag, the same shape as the names collector
// in cmd/spawnery-stubop. A cluster may mark a departing node with more than
// one vendor's taint, and one flag per key needs no separator convention of
// its own.
type taintKeys []string

func (t *taintKeys) String() string { return strings.Join(*t, ",") }

func (t *taintKeys) Set(value string) error {
	if value == "" {
		return fmt.Errorf("an empty taint key would match nothing")
	}
	// A *key*, not a taint. Taints are written key=value:Effect nearly
	// everywhere a person meets them -- `kubectl taint`, node manifests, every
	// tutorial -- so passing the whole thing here is the mistake to expect, and
	// it is the one this operator was worst at surviving: a key with a colon or
	// an equals sign in it matches no taint that exists, so the flag would be
	// accepted, nothing would ever drain, and nothing would say why.
	// What stays true after this check, and is on the flag's own usage string:
	// a well-formed key that is simply absent from the cluster cannot be told
	// from a typo by anything here. The only case that warns is a node carrying
	// a well-known drain taint this operator was not configured for
	// (wellKnownDrainTaints, internal/controller/nodes.go); for a key of
	// somebody's own choosing there is no list to check against.
	//
	// IsQualifiedName is what Kubernetes itself validates a taint key with, so
	// this refuses exactly what the API server would refuse and nothing more. A
	// key that is well-formed but simply absent from the cluster still cannot
	// be told from a typo -- nothing can tell those apart -- and this does not
	// pretend to.
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

// managerFlags are the command-line values managerOptions reads. A struct
// rather than a run of same-typed parameters, so that a call site cannot
// silently swap two of them.
type managerFlags struct {
	metricsAddr       string
	probeAddr         string
	leaderElect       bool
	watchNamespace    string
	operatorNamespace string
}

// managerOptions builds the manager's configuration.
//
// Split out of main so the two decisions below can be asserted. Everything
// else here is a value handed straight through.
func managerOptions(f managerFlags) manager.Options {
	opts := manager.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: f.metricsAddr},
		HealthProbeBindAddress: f.probeAddr,
		LeaderElection:         f.leaderElect,
		LeaderElectionID:       "spawnery-operator.spawnery.cloud",
		// Left empty, controller-runtime reads the namespace out of the
		// ServiceAccount mount, which exists only inside a pod -- so a local
		// `go run` failed at startup and had to be told --leader-elect=false,
		// which is what every runbook in docs/ passes. The lease belongs in
		// the operator's own namespace either way, and --operator-namespace
		// already carries it (POD_NAMESPACE in the chart, from
		// metadata.namespace), so naming it here changes nothing in a cluster
		// and lets a local run keep leader election on.
		LeaderElectionNamespace: f.operatorNamespace,
	}
	if f.watchNamespace != "" {
		opts.Cache.DefaultNamespaces = map[string]cache.Config{f.watchNamespace: {}}
	}
	// The bootstrap touches exactly two kinds of object, one per namespace,
	// and both carry our label. Without this restriction the cache would hold
	// every ConfigMap in the cluster — kube-root-ca.crt from every namespace
	// included — for the sake of one per namespace that is ours.
	managed := labels.SelectorFromSet(labels.Set{podspec.LabelManagedBy: podspec.ManagedByValue})
	// Per-kind cache restrictions; see the comment on each entry for why it is
	// there.
	opts.Cache.ByObject = map[client.Object]cache.ByObject{
		&corev1.ConfigMap{}:      {Label: managed},
		&corev1.ServiceAccount{}: {Label: managed},
		// A persistent server's world claim carries the same label, and there
		// is one per server. Since 5b the Server controller reads claims back
		// as well as creating them — growClaim and readResizePending
		// (internal/controller/server_controller.go) — and both go through
		// this cache, so a claim missing the label is invisible to them: it
		// never grows and never reports a pending filesystem resize. Without
		// the restriction those reads would start an informer holding every
		// claim in every watched namespace, ours and everybody else's, for the
		// sake of the one they asked for.
		&corev1.PersistentVolumeClaim{}: {Label: managed},
		// The node cache exists for one bool per node — cordoned, or tainted to
		// repel. status.images is tens of kilobytes per node and nothing here
		// reads it, so it is dropped on the way in, for the same reason the
		// ConfigMap restriction above exists.
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
		Recorder:  mgr.GetEventRecorder("certs"),
		// The same value the agent endpoint below cuts streams off with. A CA
		// rotation waits it out before switching the serving certificate, so
		// that every stream opened before the new CA was published has been
		// closed and reopened by then; a second, independently configured
		// duration would make that a coincidence rather than a bound.
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

	// What the API server says this identity may actually do, asked now and
	// then again on an interval.
	//
	// Added as a Runnable so it runs after the manager has started and the
	// leader election, if any, has settled -- a check that ran before Start
	// would report before the process is in a position to act on anything
	// anyway. rbacaudit.Checker carries the rest of the reasoning: why it is
	// not leader-bound, why it is loud rather than fatal, and what asking
	// repeatedly costs, which was measured before it was decided.
	if err := mgr.Add(&rbacaudit.Checker{
		Reviewer: clientset.AuthorizationV1().SelfSubjectAccessReviews(),
		Scopes:   rbacaudit.DefaultScopes(operatorNamespace),
		Interval: permissionCheckInterval,
	}); err != nil {
		setupLog.Error(err, "unable to add the permission self-check")
		os.Exit(1)
	}

	// Said once, at startup, where a flag is set. It is not a condition on any
	// object because it is not about any object: it is about the pair of
	// numbers this process was started with.
	if warning := rescueWindowWarning(reportInterval); warning != "" {
		setupLog.Info("the rescue window for a dead node is short", "warning", warning)
	}

	started := time.Now()
	registry := agent.New(time.Now, reportInterval, started)

	// One Fleet for the whole process: the controllers write into it and the
	// gRPC endpoint reads from it. Two would mean a registration reaching a
	// fan-out nobody is streaming from.
	proxies := proxyreg.New(proxyreg.Options{Reader: mgr.GetClient()})
	if err := mgr.Add(proxies); err != nil {
		setupLog.Error(err, "unable to add the proxy resync")
		os.Exit(1)
	}

	// The pod count behind the fleet connection bound. It reads the manager's
	// cache, which already holds the pods for the controllers' sake, so this
	// adds a walk over them every FleetCountInterval and no API traffic at all.
	fleet := &agentserver.FleetCounter{Pods: mgr.GetClient()}
	if err := mgr.Add(fleet); err != nil {
		setupLog.Error(err, "unable to add the fleet counter")
		os.Exit(1)
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
		Agents:         registry,
		Proxies:        proxies,
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
