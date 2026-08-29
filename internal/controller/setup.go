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
	// Agents is the shared runtime state of all connected agents.
	Agents *agent.Registry
	// Events is where a recorded event's copy goes on its way to somebody's
	// chat. Nil means no feed, which is what every test that does not care
	// about one passes -- see cloudevent.Recorder.
	Events cloudevent.Sink
	// AllowPluginVolumes lets a group name a spec.extraPlugins claim.
	//
	// **An operational switch and not a security boundary.** A
	// PersistentVolumeClaim is a namespaced object in the same trust domain as
	// the group that names it: anybody who can write one can write the other,
	// so this stops nobody who was not already stopped. What it is for is an
	// operator being able to say "this installation runs no third-party
	// plugins" and have that be true rather than a convention.
	//
	// Off by default for that reason and not for safety. Documenting it as a
	// security control would be the kind of check that reads like a bound and
	// cannot fail, which is worse than no check -- the next reader would trust
	// it.
	AllowPluginVolumes bool
	// Clock is the time source. Injectable for tests.
	Clock func() time.Time
	// StartupDeadline is how long a server may take to reach Ready.
	StartupDeadline time.Duration
	// PlayerStatusInterval throttles player-count writes into etcd.
	PlayerStatusInterval time.Duration
	// ReportInterval is how often the agents are told to report. It is here
	// because half the rescue-window arithmetic is this number and the other
	// half is what a proxy reports, and the Network controller compares them.
	ReportInterval time.Duration
	// OrphanInterval is how often the orphan sweep runs.
	OrphanInterval time.Duration
	// Registrar reaches the proxies. Milestone 1 wires the no-op.
	Registrar Registrar
	// Bootstrapper puts the CA bundle and the agent ServiceAccount into a
	// namespace before the first pod is created there. Required: without it
	// every pod would mount a ConfigMap that does not exist.
	Bootstrapper *Bootstrapper
	// AgentEndpoint is the address the in-game agent dials to reach the
	// operator's gRPC endpoint.
	AgentEndpoint string
	// OperatorNamespace is where the operator itself runs. The per-Network
	// NetworkPolicy needs it for its egress rule, which has to name the
	// namespace the agents dial into; AgentEndpoint above is built from the
	// same flag, and the two must not be allowed to disagree.
	OperatorNamespace string
	// Proxies is how the ProxyGroup controller tells a surplus proxy to stop
	// taking connections. Required: the production binary always supplies the
	// real *proxyreg.Fleet, and SetupAll refuses a nil value for the same
	// reason it refuses a nil Bootstrapper.
	Proxies ProxyReadinessSetter
	// DrainTaintKeys are the taint keys that mark a node as departing, beside
	// spec.unschedulable. spec.unschedulable is what kubectl cordon and
	// kubectl drain set, and it is always honoured; an autoscaler may taint a
	// node without cordoning it first, which is what this list is for. Empty
	// is the default: only cordoned nodes count.
	DrainTaintKeys []string
}

// Leader election locks on a Lease in the operator's own namespace. It is not
// tied to any single controller, which is why the marker lives here on the
// wiring rather than on a reconciler. The namespace qualifier puts the right in
// a namespaced Role — granting it cluster-wide would let the operator take a
// leader lock anywhere.
//
// spawnery-system below is not where the operator runs; it is the literal
// controller-gen requires to emit a namespaced Role at all. Real placement is
// decided at install time, when hack/chart-templates.sh rewrites this
// namespace to Helm's release namespace as part of `make manifests`.
// +kubebuilder:rbac:groups=coordination.k8s.io,namespace=spawnery-system,resources=leases,verbs=create;get;update

// SetupAll registers every controller and the orphan sweep with the manager.
func SetupAll(mgr ctrl.Manager, opts Options) error {
	// Refused here rather than at the first pod creation: a nil Bootstrapper
	// would surface as a panic inside a reconcile, minutes after start and in
	// a goroutine, instead of as a startup error.
	if opts.Bootstrapper == nil {
		return fmt.Errorf("no bootstrapper: the server controller cannot create pods " +
			"without one, and the network controller cannot keep a namespace's CA current")
	}
	// Refused for the same reason as a nil Bootstrapper: a nil Proxies would
	// surface as a panic inside a reconcile, minutes after start and in a
	// goroutine, instead of as a startup error.
	if opts.Proxies == nil {
		return fmt.Errorf("no proxies: the proxy group controller cannot set readiness without one")
	}
	// The Network controller's SecretReader, refused for the same reason. It is
	// not an Options field — the reconciler below takes it from the manager —
	// so the check is on the manager rather than on opts. A nil reader is how a
	// defect in this milestone was found: as a nil-interface panic inside
	// Reconcile, where readForwardingSecret calls Get on it for every Network
	// that owns its namespace.
	if mgr.GetAPIReader() == nil {
		return fmt.Errorf("no API reader: the network controller cannot read forwarding secrets without one")
	}

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
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup server controller: %w", err)
	}

	if err := newProxyGroupReconciler(mgr, opts).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup proxy group controller: %w", err)
	}

	// No Recorder. The sweep emits no event -- it deletes a pod whose Server is
	// gone and a Server whose group is gone, both of which are already absent
	// from the object an event would hang off. The field it used to have was
	// residue of the migration off tools/record and was read by nothing.
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

// newNetworkReconciler builds the reconciler that reads
// Options.OperatorNamespace, and it is a function for the same reason the two
// below are: SetupAll offers no seam a test can reach through, so an
// assignment made only there is an assignment nothing can observe. Deleting
// `OperatorNamespace: opts.OperatorNamespace` left internal/controller green,
// while the rendered egress peer would have carried
// `kubernetes.io/metadata.name: ""` — a namespace selector matching nothing.
// Under an enforcing CNI every agent in every namespace stops dialling at
// once, and every object still reads as correct.
func newNetworkReconciler(mgr ctrl.Manager, opts Options) *NetworkReconciler {
	return &NetworkReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		Recorder:          cloudevent.Recorder{Inner: mgr.GetEventRecorder("network"), Sink: opts.Events},
		OperatorNamespace: opts.OperatorNamespace,
		// Uncached, for the reason SecretReader's own comment gives. The
		// Bootstrapper takes the same reader for the same reason.
		SecretReader: mgr.GetAPIReader(),
		Bootstrap:    opts.Bootstrapper,
		// The two halves of the rescue-window arithmetic: the interval this
		// operator dictates, and what the namespace's proxies report about
		// their own read timeout.
		Agents:         opts.Agents,
		ReportInterval: opts.ReportInterval,
	}
}

// newServerGroupReconciler and newProxyGroupReconciler build the two
// reconcilers that read Options.DrainTaintKeys.
//
// They are functions rather than composite literals inline in SetupAll for
// one reason: the wiring is otherwise unassertable. Most of the other Options
// fields reach reconcilers that tests already observe doing something with
// them — Agents, Clock, StartupDeadline, Registrar, Bootstrapper,
// AgentEndpoint and Proxies all have at least one test elsewhere in this
// package whose outcome would change if the field were wired wrong or not at
// all. Two do not: PlayerStatusInterval is only ever set to a fixed value in
// fixtures, and no test exercises mirrorPlayerCount's throttle at a value
// that would tell a wrong one apart; OrphanInterval is never set by a test at
// all, and every orphan-sweep test calls the sweep directly rather than
// through Start's ticker, so nothing there has ever depended on it either.
// DrainTaintKeys is set nowhere outside SetupAll and the operator binary,
// no fixture reconciler sets it, and deleting either assignment left the
// whole suite green — so acceptance criterion 4, that a node carrying a
// configured taint key is treated like a cordoned one, had no test that could
// fail. SetupAll has no seam a test can reach through, and these do.
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
		Proxies:            opts.Proxies,
		Clock:              opts.Clock,
		Expectations:       newExpectations(opts.Clock),
		Divergence:         newReadinessDivergence(opts.Clock),
		DrainTaintKeys:     opts.DrainTaintKeys,
		AllowPluginVolumes: opts.AllowPluginVolumes,
	}
}
