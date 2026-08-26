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

// Package agent holds the runtime state the in-game agents report. It is the
// port the gRPC service of milestone 2 plugs into: the controllers only read
// snapshots from here and never talk to an agent directly.
//
// Player counts live in memory on purpose. At 200 servers, writing every
// report into etcd would be dozens of writes per second; the CR status is for
// observers, not for the control loop.
package agent

import (
	"fmt"
	"sync"
	"time"
)

// Role separates the two kinds of agents. A server agent may never act as a
// proxy agent; milestone 2 derives the role from the pod's ServiceAccount.
type Role string

const (
	// RoleServer is a Paper agent.
	RoleServer Role = "server"
	// RoleProxy is a Velocity agent.
	RoleProxy Role = "proxy"
)

// Snapshot is a consistent read of one agent's state.
type Snapshot struct {
	// Known is false if the registry never saw this pod.
	Known bool
	// Connected is true while the agent stream is up.
	Connected bool
	// Ready is true if the agent reported readiness on the current stream.
	Ready bool
	// Players is the last reported player count.
	Players int32
	// Slots is the last reported capacity.
	Slots int32
	// PlayersStale is true if the count is older than twice the report
	// interval, or if the pod is unknown. Stale counts as occupied.
	PlayersStale bool
	// StreamDownFor is how long the stream has been down. Zero while up. For
	// an unknown pod it is the time since the operator started, so agents get
	// a grace period to reconnect after an operator restart.
	StreamDownFor time.Duration
	// EmptyFor is how long the agent has been reporting zero players. It is
	// zero while players are on, and zero before the first report — a server
	// that has never reported is not known to be empty.
	//
	// It never decides anything on its own. Every rule that reads it also asks
	// players == 0 && !PlayersStale, because a server that was never empty
	// reports zero here too, and scaleDownStabilizationSeconds may be 0.
	EmptyFor time.Duration
}

type entry struct {
	role           Role
	connected      bool
	ready          bool
	players        int32
	slots          int32
	emptySince     time.Time
	lastReportAt   time.Time
	disconnectedAt time.Time

	// namespace and backends are a proxy's report of how many of its players
	// are on, or heading to, each backend it knows -- see AttachedTo. Both are
	// zero for a server agent, which reports about itself and not about
	// anybody else.
	namespace string
	backends  map[string]int32
	// backendsAt is when that map last arrived. Separate from lastReportAt
	// because the two messages are separate: an agent that reported players
	// and not backends is an old agent, and reading its player timestamp as
	// though it had said something about backends would invent an answer.
	backendsAt time.Time
}

// Registry is the in-memory state of all connected agents. It is safe for
// concurrent use.
type Registry struct {
	mu             sync.RWMutex
	entries        map[string]*entry
	now            func() time.Time
	reportInterval time.Duration
	startedAt      time.Time
}

// New creates a registry. The clock is injectable so the staleness rules are
// testable; startedAt is when the operator process came up.
func New(clock func() time.Time, reportInterval time.Duration, startedAt time.Time) *Registry {
	return &Registry{
		entries:        make(map[string]*entry),
		now:            clock,
		reportInterval: reportInterval,
		startedAt:      startedAt,
	}
}

// Connect records a new agent stream. Readiness is not implied: the agent has
// to state it, either through MarkReady or through Hello{ready:true}. The
// process behind a fresh stream may have restarted, so only its own Hello may
// say it is ready again.
func (r *Registry) Connect(key string, role Role) {
	r.connect(key, role, false)
}

// Supersede records a stream that takes over from one that is still live for
// the same pod. Unlike Connect it carries the readiness of the displaced
// stream over.
//
// The difference matters because of make-before-break: an agent opens its next
// stream before the current one ends, and the new stream is registered before
// its Hello arrives. Were that registered as a plain Connect, readiness would
// be false for the length of one round trip, and a reconcile landing in that
// window reads "connected but not ready" — an immediate readiness loss that
// deregisters the server from the proxies and counts against its flap budget,
// once per renewal period per server. Since the old stream was still live, the
// agent process never went away and the readiness it reported still holds.
func (r *Registry) Supersede(key string, role Role) {
	r.connect(key, role, true)
}

// connect is the shared body: keepReady decides whether the readiness of a
// previous stream survives. Both callers set it in the same critical section
// as connected, so no reader can observe the pair half-updated.
func (r *Registry) connect(key string, role Role, keepReady bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.entries[key]
	if !ok {
		e = &entry{}
		r.entries[key] = e
	}
	e.role = role
	e.connected = true
	if !keepReady {
		e.ready = false
		// A fresh stream may have a restarted process behind it, and that
		// process has reported nothing yet — the same reasoning that clears
		// readiness here. A superseding stream cannot: the displaced one was
		// still live, so the emptiness it observed still holds.
		e.emptySince = time.Time{}
	}
	e.disconnectedAt = time.Time{}
}

// MarkReady records that the agent reported readiness.
func (r *Registry) MarkReady(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if e, ok := r.entries[key]; ok && e.connected {
		e.ready = true
	}
}

// ReportPlayers records a player count. Counts above the reported capacity are
// rejected as defense in depth against a compromised agent.
func (r *Registry) ReportPlayers(key string, players, slots int32) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.entries[key]
	if !ok || !e.connected {
		return fmt.Errorf("no live stream for %q", key)
	}
	if players < 0 || slots < 0 {
		return fmt.Errorf("negative report for %q: %d/%d", key, players, slots)
	}
	if players > slots {
		return fmt.Errorf("report for %q exceeds capacity: %d/%d", key, players, slots)
	}
	e.players = players
	e.slots = slots
	// The stabilization window measures from the moment the server became
	// empty, not from the last report that found it empty, so a repeated zero
	// must not restart it.
	if players == 0 {
		if e.emptySince.IsZero() {
			e.emptySince = r.now()
		}
	} else {
		e.emptySince = time.Time{}
	}
	e.lastReportAt = r.now()
	return nil
}

// ReportBackends records a proxy's per-backend attachment map: how many of its
// players are on, or on their way to, each backend it has been told about.
//
// The whole map replaces the whole map. Every report carries the complete
// state, so a server that has dropped out of it has nobody attaching to it --
// there is no separate "left" message to miss, and a dropped report costs one
// interval of freshness rather than a count stranded forever.
//
// namespace comes from the authenticated identity rather than from the
// message, for the reason the package comment gives about every other fact
// here: an agent may not say which namespace's servers it is talking about.
func (r *Registry) ReportBackends(key, namespace string, backends map[string]int32) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.entries[key]
	if !ok || !e.connected {
		return fmt.Errorf("no live stream for %q", key)
	}
	if e.role != RoleProxy {
		// A server agent has no view of anybody else's backends, so a report
		// from one is a bug in an agent rather than a state to store.
		return fmt.Errorf("backend report from a %s agent %q", e.role, key)
	}
	for name, n := range backends {
		if n < 0 {
			return fmt.Errorf("negative backend report for %q: %s=%d", key, name, n)
		}
	}
	e.namespace = namespace
	e.backends = backends
	e.backendsAt = r.now()
	return nil
}

// AttachedTo reports how many players the proxies say are on, or heading to,
// one backend, and whether any proxy's answer is too old to believe.
//
// # Why this exists beside the backend's own count
//
// A backend counts a player only once they have finished the configuration
// phase. Disassembling velocity 3.5.1 build 615,
// VelocityRegisteredServer.addPlayer is called from exactly one place --
// BackendPlaySessionHandler.activated(), the backend's *play* phase -- so a
// player still handshaking is invisible to the backend and to the proxy's own
// getPlayersConnected() alike. The drain's exit condition read the first of
// those and deleted the pod under such a player.
//
// # How a caller must use it
//
// Add it to occupancy; never subtract from it. A proxy too old to send the
// report contributes nothing here and the caller behaves exactly as it did
// before this existed, which is what lets a fleet upgrade in any order. A
// proxy that does send it can only make the caller more careful.
//
// stale is true when a proxy that has reported backends at some point has not
// done so recently, and it is deliberately not true for a proxy that has never
// reported at all: that is an old agent, not a silent one, and treating the
// two the same would hold every server in the installation occupied for as
// long as one un-upgraded proxy ran.
func (r *Registry) AttachedTo(namespace, server string) (players int32, stale bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, e := range r.entries {
		if e.role != RoleProxy || e.namespace != namespace {
			continue
		}
		if e.backendsAt.IsZero() {
			continue
		}
		if !e.connected || r.now().Sub(e.backendsAt) > 2*r.reportInterval {
			// A proxy whose report has aged out may be holding players on
			// this backend and no longer saying so. Reported as stale rather
			// than as a count, so the caller can treat unknown as occupied
			// the way it does everywhere else.
			stale = true
			continue
		}
		players += e.backends[server]
	}
	return players, stale
}

// Disconnect records that the stream broke. The last player count is kept, so
// the server stays protected until the count goes stale.
func (r *Registry) Disconnect(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if e, ok := r.entries[key]; ok {
		e.connected = false
		e.ready = false
		e.disconnectedAt = r.now()
	}
}

// Forget drops a pod entirely. The controllers call it once a pod is gone for
// good, so the map does not grow without bound.
func (r *Registry) Forget(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, key)
}

// Keys returns every pod key the registry currently knows.
func (r *Registry) Keys() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	keys := make([]string, 0, len(r.entries))
	for k := range r.entries {
		keys = append(keys, k)
	}
	return keys
}

// Lookup returns a consistent snapshot of one agent.
func (r *Registry) Lookup(key string) Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()

	now := r.now()
	e, ok := r.entries[key]
	if !ok {
		return Snapshot{
			PlayersStale:  true,
			StreamDownFor: now.Sub(r.startedAt),
		}
	}

	snap := Snapshot{
		Known:     true,
		Connected: e.connected,
		Ready:     e.ready,
		Players:   e.players,
		Slots:     e.slots,
	}
	if !e.connected {
		snap.StreamDownFor = now.Sub(e.disconnectedAt)
	}
	snap.PlayersStale = e.lastReportAt.IsZero() ||
		now.Sub(e.lastReportAt) >= 2*r.reportInterval
	if !e.emptySince.IsZero() {
		snap.EmptyFor = now.Sub(e.emptySince)
	}
	return snap
}
