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

// Package agent holds the runtime state the in-game agents report. The
// controllers read snapshots from here and never talk to an agent directly.
//
// Player counts live in memory, not in etcd: at a few hundred servers every
// report would be dozens of writes per second.
package agent

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

// Role separates the two kinds of agents. A server agent may never act as a
// proxy agent.
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
	Ready   bool
	Players int32
	Slots   int32
	// PlayableSlots is the last figure the plugin set, 0 if it set none.
	PlayableSlots int32
	// TPS and MSPT are a server agent's last tick report, zero if it never sent
	// one. They arrive in the same report as the player count.
	TPS  float64
	MSPT float64
	// HeapUsed and HeapMax are the agent's JVM heap in bytes; 0 when it has
	// not reported them.
	HeapUsed int64
	HeapMax  int64
	// PlayersStale is true if the count is older than twice the report
	// interval, or if the pod is unknown. Stale counts as occupied.
	PlayersStale bool
	// PlayersReportedAt is when the count arrived, zero if it never did. Unlike
	// PlayersStale it answers whether the count was taken after a given moment,
	// such as the start of a drain.
	PlayersReportedAt time.Time
	// StreamDownFor is how long the stream has been down. Zero while up. For
	// an unknown pod it is the time since the operator started, so agents get
	// a grace period to reconnect after an operator restart.
	StreamDownFor time.Duration
	// AcceptingJoins is whether this server wants new players routed to it.
	// True unless the server said otherwise, and true for an unknown pod, so an
	// older agent or a restarted operator changes nothing about routing.
	AcceptingJoins bool
	// RoundEnded is whether this server has said its round is over; false for an
	// unknown pod.
	RoundEnded bool
	// EmptyFor is how long the agent has been reporting zero players; zero while
	// players are on and before the first report. Every rule that reads it also
	// checks players == 0 && !PlayersStale, because scaleDownStabilizationSeconds
	// may be 0.
	EmptyFor time.Duration
}

// Announcement is what one server last said about itself. The operator carries
// it and acts on none of it; see the proto's AnnounceRequest.
type Announcement struct {
	State      string
	Attributes map[string]string
}

type entry struct {
	role           Role
	connected      bool
	ready          bool
	players        int32
	slots          int32
	playable       int32
	tps            float64
	mspt           float64
	heapUsed       int64
	heapMax        int64
	emptySince     time.Time
	lastReportAt   time.Time
	disconnectedAt time.Time

	// namespace and backends are a proxy's report about each backend it knows
	// (see AttachedTo); both stay zero for a server agent.
	namespace string
	backends  map[string]int32
	// roster is the only place in the operator that holds a player's name. It
	// stays in memory: no CR, no default-verbosity log line, no metric label.
	roster []RosterEntry
	// rosterAt and backendsAt are separate from lastReportAt so that an older
	// agent sending counts but no roster or backends is not read as having
	// reported them.
	rosterAt   time.Time
	backendsAt time.Time

	// server comes from the authenticated identity, never from a message; the
	// pod is named after its Server, so it cannot choose this name.
	server string
	// joinsClosed stores the refusal, so the zero value is a server that takes
	// players. Like announcement, it survives a disconnect.
	joinsClosed bool
	// roundEnded is set once this server has said its round is over. It is
	// never cleared: a round does not restart inside one pod.
	roundEnded bool
	// announcement has no timestamp and never goes stale: it is sent only when
	// it changes, so its age says nothing. It survives a disconnect so that a
	// make-before-break renewal does not blank it for every other agent.
	announcement *Announcement

	// readTimeout is what a proxy reported on its Hello; zero for an older agent
	// and for every server agent. It cannot change without a restart, which is a
	// new stream, so it has no timestamp.
	readTimeout time.Duration
}

// Registry is the in-memory state of all connected agents. It is safe for
// concurrent use.
type Registry struct {
	mu             sync.RWMutex
	entries        map[string]*entry
	now            func() time.Time
	reportInterval time.Duration
	startedAt      time.Time
	servingAt      time.Time
}

// New creates a registry. startedAt is when the operator process came up; an
// unknown pod's StreamDownFor counts from it until MarkServing moves it.
func New(clock func() time.Time, reportInterval time.Duration, startedAt time.Time) *Registry {
	return &Registry{
		entries:        make(map[string]*entry),
		now:            clock,
		reportInterval: reportInterval,
		startedAt:      startedAt,
	}
}

// MarkServing records that agents can reach this operator from now on. An
// unknown pod's StreamDownFor counts from here, because no agent could connect
// during leader election.
func (r *Registry) MarkServing() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.servingAt.IsZero() {
		r.servingAt = r.now()
	}
}

// Connect records a new agent stream. Readiness and emptiness are cleared: the
// process behind it may have restarted, so only its own Hello may say it is
// ready again.
func (r *Registry) Connect(key string, role Role) {
	r.connect(key, role, false)
}

// Supersede records a stream that takes over from one still live for the same
// pod, and keeps its readiness. Under make-before-break the new stream is
// registered before its Hello arrives; clearing readiness for that round trip
// would deregister the server from the proxies once per renewal.
func (r *Registry) Supersede(key string, role Role) {
	r.connect(key, role, true)
}

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
		e.emptySince = time.Time{}
	}
	e.disconnectedAt = time.Time{}
}

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
	// A repeated zero must not restart the stabilization window.
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

// ReportTicks records a server's tick rate, refusing values no server can
// produce.
func (r *Registry) ReportTicks(key string, tps, mspt float64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.entries[key]
	if !ok || !e.connected {
		return fmt.Errorf("no live stream for %q", key)
	}
	if math.IsNaN(tps) || math.IsNaN(mspt) || tps < 0 || tps > 100 || mspt < 0 || mspt > 600000 {
		return fmt.Errorf("impossible tick report for %q: %v TPS, %v ms", key, tps, mspt)
	}
	e.tps = tps
	e.mspt = mspt
	return nil
}

func (r *Registry) ReportHeap(key string, used, max int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.entries[key]
	if !ok || !e.connected {
		return fmt.Errorf("no live stream for %q", key)
	}
	if used < 0 || max < 0 {
		return fmt.Errorf("impossible heap report for %q: %d of %d bytes", key, used, max)
	}
	e.heapUsed = used
	e.heapMax = max
	return nil
}

func (r *Registry) ReportPlayableSlots(key string, playable int32) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.entries[key]
	if !ok || !e.connected {
		return fmt.Errorf("no live stream for %q", key)
	}
	if playable < 0 {
		return fmt.Errorf("negative playable slots for %q: %d", key, playable)
	}
	e.playable = playable
	return nil
}

// ReportBackends records how many of a proxy's players are on, or heading to,
// each backend. Each report carries the complete map and replaces the previous
// one, so there is no "left" message to miss. namespace comes from the
// authenticated identity, never from the message.
func (r *Registry) ReportBackends(key, namespace string, backends map[string]int32) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.entries[key]
	if !ok || !e.connected {
		return fmt.Errorf("no live stream for %q", key)
	}
	if e.role != RoleProxy {
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

// RosterEntry is one player as a proxy last saw them.
type RosterEntry struct {
	// UUID is what everything keys on: a name can be changed and reused.
	UUID string
	Name string
	// Server is the backend this player is on or heading for, empty for neither;
	// the same field BackendPlayers counts.
	Server string
}

// ReportRoster records who a proxy is serving. namespace comes from the
// authenticated identity, never from the message.
func (r *Registry) ReportRoster(key, namespace string, players []RosterEntry) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.entries[key]
	if !ok || !e.connected {
		return fmt.Errorf("no live stream for %q", key)
	}
	if e.role != RoleProxy {
		return fmt.Errorf("roster from a %s agent %q", e.role, key)
	}
	e.namespace = namespace
	e.roster = players
	e.rosterAt = r.now()
	return nil
}

// ReportAnnouncement records what one server says about itself. namespace and
// server come from the authenticated identity; the request handler enforces the
// size bounds. A proxy is refused: nothing reads a per-proxy announcement.
func (r *Registry) ReportAnnouncement(key, namespace, server string, a Announcement) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.entries[key]
	if !ok || !e.connected {
		return fmt.Errorf("no live stream for %q", key)
	}
	if e.role != RoleServer {
		return fmt.Errorf("announcement from a %s agent %q", e.role, key)
	}
	e.namespace = namespace
	e.server = server
	// Copied: the caller still owns the message the map came in.
	attributes := make(map[string]string, len(a.Attributes))
	for k, v := range a.Attributes {
		attributes[k] = v
	}
	e.announcement = &Announcement{State: a.State, Attributes: attributes}
	return nil
}

// ReportAcceptJoins records whether a server wants new players, and reports
// whether that changed. A proxy is refused: it is the routing table, not in
// one.
func (r *Registry) ReportAcceptJoins(key, namespace string, accept, roundEnded bool) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.entries[key]
	if !ok || !e.connected {
		return false, fmt.Errorf("no live stream for %q", key)
	}
	if e.role != RoleServer {
		return false, fmt.Errorf("accept-joins from a %s agent %q", e.role, key)
	}
	e.namespace = namespace
	changed := e.joinsClosed == accept
	e.joinsClosed = !accept
	if roundEnded {
		e.roundEnded = true
	}
	return changed, nil
}

// Announcements is what every server in a namespace last said, keyed by server
// name. Servers that said nothing are absent.
func (r *Registry) Announcements(namespace string) map[string]Announcement {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make(map[string]Announcement)
	for _, e := range r.entries {
		if e.role != RoleServer || e.namespace != namespace || e.announcement == nil || e.server == "" {
			continue
		}
		attributes := make(map[string]string, len(e.announcement.Attributes))
		for k, v := range e.announcement.Attributes {
			attributes[k] = v
		}
		out[e.server] = Announcement{State: e.announcement.State, Attributes: attributes}
	}
	return out
}

// ClosedDoors is every pod in a namespace whose server closed its door, keyed
// by pod UID (a ServerState's Incarnation) rather than server name: after a
// persistent server's pod restarts, the old entry lives on until the orphan
// sweep, and two entries share one name.
func (r *Registry) ClosedDoors(namespace string) map[string]bool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make(map[string]bool)
	for key, e := range r.entries {
		if e.role != RoleServer || e.namespace != namespace || !e.joinsClosed {
			continue
		}
		out[key] = true
	}
	return out
}

// Roster is every player the live proxies of a namespace are serving, and
// whether any proxy's answer is too old to believe. During a proxy handover two
// proxies may report the same player; the newer report wins so nobody appears
// twice. Staleness follows AttachedTo.
func (r *Registry) Roster(namespace string) ([]RosterEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	type dated struct {
		entry RosterEntry
		at    time.Time
	}
	byPlayer := make(map[string]dated)
	stale := false

	for _, e := range r.entries {
		if e.role != RoleProxy || e.namespace != namespace {
			continue
		}
		if e.rosterAt.IsZero() {
			continue
		}
		if !e.connected || r.now().Sub(e.rosterAt) > 2*r.reportInterval {
			stale = true
			continue
		}
		for _, p := range e.roster {
			if prev, ok := byPlayer[p.UUID]; ok && prev.at.After(e.rosterAt) {
				continue
			}
			byPlayer[p.UUID] = dated{entry: p, at: e.rosterAt}
		}
	}

	out := make([]RosterEntry, 0, len(byPlayer))
	for _, d := range byPlayer {
		out = append(out, d.entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UUID < out[j].UUID })
	return out, stale
}

// AttachedTo reports how many players the proxies say are on, or heading to,
// one backend, and whether any proxy's answer is too old to believe.
//
// A backend counts a player only from the play phase on (Velocity calls
// addPlayer from BackendPlaySessionHandler.activated()), so a drain reading the
// backend's own count would delete the pod under a player still configuring.
//
// Callers add this to occupancy and never subtract it. A proxy that has never
// reported backends is an old agent: it contributes nothing and is not stale,
// or one un-upgraded proxy would hold every server occupied.
//
// since is the moment the caller's question is about; a report not newer than
// it counts as stale. Pass the zero time when there is no such moment.
func (r *Registry) AttachedTo(namespace, server string, since time.Time) (players int32, stale bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, e := range r.entries {
		if e.role != RoleProxy || e.namespace != namespace {
			continue
		}
		if e.backendsAt.IsZero() {
			continue
		}
		if !since.IsZero() && !e.backendsAt.After(since) {
			stale = true
			continue
		}
		if !e.connected || r.now().Sub(e.backendsAt) > 2*r.reportInterval {
			// Stale rather than a count, so the caller treats unknown as occupied.
			stale = true
			continue
		}
		players += e.backends[server]
	}
	return players, stale
}

// ReportReadTimeout records the backend read timeout a connected proxy
// reported on its Hello. Non-positive values are ignored: ShortestReadTimeout
// takes the minimum, and a zero would override every real one.
func (r *Registry) ReportReadTimeout(key, namespace string, timeout time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	e, ok := r.entries[key]
	if !ok || e.role != RoleProxy || timeout <= 0 {
		return
	}
	// Set here too: the Hello arrives a report interval before the first
	// backend map.
	e.namespace = namespace
	e.readTimeout = timeout
}

// ShortestReadTimeout is the smallest read timeout any connected proxy in this
// namespace reported, and whether any did. The smallest, because the first
// proxy to give up is the one that kicks the players.
func (r *Registry) ShortestReadTimeout(namespace string) (time.Duration, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var shortest time.Duration
	for _, e := range r.entries {
		if e.role != RoleProxy || !e.connected || e.namespace != namespace {
			continue
		}
		if e.readTimeout <= 0 {
			continue
		}
		if shortest == 0 || e.readTimeout < shortest {
			shortest = e.readTimeout
		}
	}
	return shortest, shortest > 0
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

func (r *Registry) Keys() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	keys := make([]string, 0, len(r.entries))
	for k := range r.entries {
		keys = append(keys, k)
	}
	return keys
}

func (r *Registry) Lookup(key string) Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()

	now := r.now()
	e, ok := r.entries[key]
	if !ok {
		since := r.startedAt
		if !r.servingAt.IsZero() {
			since = r.servingAt
		}
		return Snapshot{
			PlayersStale:   true,
			AcceptingJoins: true,
			StreamDownFor:  now.Sub(since),
		}
	}

	snap := Snapshot{
		Known:             true,
		AcceptingJoins:    !e.joinsClosed,
		RoundEnded:        e.roundEnded,
		Connected:         e.connected,
		Ready:             e.ready,
		Players:           e.players,
		Slots:             e.slots,
		PlayableSlots:     e.playable,
		TPS:               e.tps,
		MSPT:              e.mspt,
		HeapUsed:          e.heapUsed,
		HeapMax:           e.heapMax,
		PlayersReportedAt: e.lastReportAt,
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
