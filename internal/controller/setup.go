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
)

// Options are the knobs the operator binary passes to the controllers.
type Options struct {
	// Agents is the shared runtime state of all connected agents.
	Agents *agent.Registry
	// Clock is the time source. Injectable for tests.
	Clock func() time.Time
	// StartupDeadline is how long a server may take to reach Ready.
	StartupDeadline time.Duration
	// PlayerStatusInterval throttles player-count writes into etcd.
	PlayerStatusInterval time.Duration
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
// +kubebuilder:rbac:groups=coordination.k8s.io,namespace=spawnery-system,resources=leases,verbs=create;get;update

// SetupAll registers every controller and the orphan sweep with the manager.
func SetupAll(mgr ctrl.Manager, opts Options) error {
	// Refused here rather than at the first pod creation: a nil Bootstrapper
	// would surface as a panic inside a reconcile, minutes after start and in
	// a goroutine, instead of as a startup error.
	if opts.Bootstrapper == nil {
		return fmt.Errorf("no bootstrapper: the server controller cannot create pods without one")
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

	if err := (&NetworkReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("network"),
		Clock:    opts.Clock,
		// Uncached, for the reason SecretReader's own comment gives. The
		// Bootstrapper takes the same reader for the same reason.
		SecretReader: mgr.GetAPIReader(),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup network controller: %w", err)
	}

	if err := newServerGroupReconciler(mgr, opts).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup server group controller: %w", err)
	}

	if err := (&ServerReconciler{
		Client:               mgr.GetClient(),
		Scheme:               mgr.GetScheme(),
		Recorder:             mgr.GetEventRecorderFor("server"),
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

	if err := mgr.Add(&OrphanReconciler{
		Client:   mgr.GetClient(),
		Recorder: mgr.GetEventRecorderFor("orphan"),
		Agents:   opts.Agents,
		Interval: opts.OrphanInterval,
	}); err != nil {
		return fmt.Errorf("add orphan sweep: %w", err)
	}

	return nil
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
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		Recorder:       mgr.GetEventRecorderFor("servergroup"),
		Agents:         opts.Agents,
		Clock:          opts.Clock,
		Expectations:   newExpectations(opts.Clock),
		DrainTaintKeys: opts.DrainTaintKeys,
	}
}

func newProxyGroupReconciler(mgr ctrl.Manager, opts Options) *ProxyGroupReconciler {
	return &ProxyGroupReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		Recorder:       mgr.GetEventRecorderFor("proxygroup"),
		Agents:         opts.Agents,
		Bootstrap:      opts.Bootstrapper,
		AgentEndpoint:  opts.AgentEndpoint,
		Proxies:        opts.Proxies,
		Clock:          opts.Clock,
		Expectations:   newExpectations(opts.Clock),
		Divergence:     newReadinessDivergence(opts.Clock),
		DrainTaintKeys: opts.DrainTaintKeys,
	}
}
