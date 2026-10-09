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

package controller

import (
	"fmt"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/cloudevent"
)

// Options are the knobs the operator binary passes to the controllers.
type Options struct {
	Agents *agent.Registry
	// Events receives a copy of each recorded event; nil means no feed.
	Events cloudevent.Sink
	// AllowPluginVolumes lets a group name a spec.extraPlugins claim. An
	// operational switch, not a security boundary: whoever can write the group
	// can write the claim.
	AllowPluginVolumes bool
	// AllowFileVolumes lets a group name a spec.extraFiles claim; an
	// operational switch like AllowPluginVolumes.
	AllowFileVolumes bool
	// AllowMountVolumes lets a group name a PersistentVolumeClaim in
	// spec.mounts; an operational switch like AllowPluginVolumes.
	AllowMountVolumes bool
	AOTCache          bool
	// WorldSync: spawnery-worldsync runs on the nodes and the operator holds
	// the bucket; groups with storage.backend ObjectStore get pods only then.
	WorldSync         bool
	WorldSyncInterval time.Duration
	// Retention publishes each group's storage.retention for the node
	// agents; nil without world sync.
	Retention            RetentionPublisher
	Clock                func() time.Time
	StartupDeadline      time.Duration
	PlayerStatusInterval time.Duration
	// ReportInterval is how often the agents are told to report.
	ReportInterval time.Duration
	OrphanInterval time.Duration
	Registrar      Registrar
	// Bootstrapper puts the CA bundle and the agent ServiceAccount into a
	// namespace before the first pod is created there. Required.
	Bootstrapper  *Bootstrapper
	AgentEndpoint string
	// OperatorNamespace is where the operator runs, for the NetworkPolicy's
	// egress rule; it must agree with AgentEndpoint.
	OperatorNamespace string
	// Proxies tells a surplus proxy to stop taking connections. Required.
	Proxies ProxyReadinessSetter
	// DrainTaintKeys mark a node as departing beside spec.unschedulable, for
	// autoscalers that taint without cordoning.
	DrainTaintKeys []string
}

// Leader election. spawnery-system is a placeholder controller-gen needs to
// emit a namespaced Role; hack/chart-templates.sh rewrites it to the release
// namespace.
// +kubebuilder:rbac:groups=coordination.k8s.io,namespace=spawnery-system,resources=leases,verbs=create;get;update

func SetupAll(mgr ctrl.Manager, opts Options) error {
	if opts.Bootstrapper == nil {
		return fmt.Errorf("no bootstrapper: the server controller cannot create pods " +
			"without one, and the network controller cannot keep a namespace's CA current")
	}
	if opts.Proxies == nil {
		return fmt.Errorf("no proxies: the proxy group controller cannot set readiness without one")
	}
	if mgr.GetAPIReader() == nil {
		return fmt.Errorf("no API reader: the network controller cannot read forwarding secrets without one")
	}

	NetworkMetrics.Bind(mgr.GetClient(), opts.Agents)

	if err := newNetworkReconciler(mgr, opts).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup network controller: %w", err)
	}

	if err := newServerGroupReconciler(mgr, opts).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup server group controller: %w", err)
	}

	if err := (&ServerReconciler{
		Client:               mgr.GetClient(),
		Scheme:               mgr.GetScheme(),
		Recorder:             cloudevent.Recorder{Inner: mgr.GetEventRecorder("server"), Sink: opts.Events},
		Agents:               opts.Agents,
		Clock:                opts.Clock,
		StartupDeadline:      opts.StartupDeadline,
		PlayerStatusInterval: opts.PlayerStatusInterval,
		Registrar:            opts.Registrar,
		Bootstrap:            opts.Bootstrapper,
		AgentEndpoint:        opts.AgentEndpoint,
		AOTCache:             opts.AOTCache,
		WorldSync:            opts.WorldSync,
		WorldSyncInterval:    opts.WorldSyncInterval,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup server controller: %w", err)
	}

	if err := newProxyGroupReconciler(mgr, opts).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup proxy group controller: %w", err)
	}

	// No Recorder: the objects an event would hang off are already gone.
	if err := mgr.Add(&OrphanReconciler{
		Client:   mgr.GetClient(),
		Agents:   opts.Agents,
		Interval: opts.OrphanInterval,
		Clock:    opts.Clock,
	}); err != nil {
		return fmt.Errorf("add orphan sweep: %w", err)
	}

	return nil
}

// The new*Reconciler functions exist so tests can assert the Options wiring,
// which SetupAll alone gives them no seam for.
func newNetworkReconciler(mgr ctrl.Manager, opts Options) *NetworkReconciler {
	return &NetworkReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Recorder:          cloudevent.Recorder{Inner: mgr.GetEventRecorder("network"), Sink: opts.Events},
		OperatorNamespace: opts.OperatorNamespace,
		SecretReader:      mgr.GetAPIReader(),
		Bootstrap:         opts.Bootstrapper,
		Agents:            opts.Agents,
		ReportInterval:    opts.ReportInterval,
	}
}

func newServerGroupReconciler(mgr ctrl.Manager, opts Options) *ServerGroupReconciler {
	return &ServerGroupReconciler{
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		Recorder:           cloudevent.Recorder{Inner: mgr.GetEventRecorder("servergroup"), Sink: opts.Events},
		Agents:             opts.Agents,
		Clock:              opts.Clock,
		Expectations:       newExpectations(opts.Clock),
		DrainTaintKeys:     opts.DrainTaintKeys,
		AllowPluginVolumes: opts.AllowPluginVolumes,
		AllowFileVolumes:   opts.AllowFileVolumes,
		AllowMountVolumes:  opts.AllowMountVolumes,
		ClaimReader:        mgr.GetAPIReader(),
		Retention:          opts.Retention,
	}
}

func newProxyGroupReconciler(mgr ctrl.Manager, opts Options) *ProxyGroupReconciler {
	return &ProxyGroupReconciler{
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		Recorder:           cloudevent.Recorder{Inner: mgr.GetEventRecorder("proxygroup"), Sink: opts.Events},
		Agents:             opts.Agents,
		Bootstrap:          opts.Bootstrapper,
		AgentEndpoint:      opts.AgentEndpoint,
		OperatorNamespace:  opts.OperatorNamespace,
		Proxies:            opts.Proxies,
		Clock:              opts.Clock,
		Expectations:       newExpectations(opts.Clock),
		Divergence:         newReadinessDivergence(opts.Clock),
		DrainTaintKeys:     opts.DrainTaintKeys,
		AllowPluginVolumes: opts.AllowPluginVolumes,
		AllowFileVolumes:   opts.AllowFileVolumes,
		AllowMountVolumes:  opts.AllowMountVolumes,
		ClaimReader:        mgr.GetAPIReader(),
	}
}
