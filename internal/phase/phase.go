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

// Package phase implements the Server state machine as a pure function.
// It knows nothing about Kubernetes: the controller collects the inputs,
// calls Decide and executes the returned decision. Every rule about
// registration and deletion lives here and nowhere else.
package phase

import "time"

// Phase is the position of a Server in its lifecycle.
type Phase string

const (
	// Pending means the CR exists but the pod is not running yet.
	Pending Phase = "Pending"
	// Starting means the pod runs but at least one ready signal is missing.
	Starting Phase = "Starting"
	// Ready means the server is registered with the proxies and takes players.
	Ready Phase = "Ready"
	// Retiring is soft drain: the server is deregistered and takes no new
	// joins, but its players are left alone until they leave of their own
	// accord. It is what a rolling update puts a stale server into, and it
	// is deliberately not Draining: no players are moved, and
	// spec.drain.timeoutSeconds does not hang over it, because a lobby can
	// legitimately sit here for hours. spec.update.maxStaleSeconds is the
	// only thing that bounds it, and only when configured non-zero.
	Retiring Phase = "Retiring"
	// Draining means the server is deregistered and players are being moved.
	// There is no way back to Ready.
	Draining Phase = "Draining"
	// Terminating means the pod is being deleted.
	Terminating Phase = "Terminating"
	// Failed means the server is broken. It is kept for diagnosis and cleaned
	// up after the group's retention.
	Failed Phase = "Failed"
	// Finished means the server's round is over: it said so, and then its pod
	// stopped. It is terminal like Failed and its group replaces it at once,
	// but it is not a fault -- it is not counted against the backoff, it
	// raises no Degraded, and it is kept for its own short retention rather
	// than the hour a failure gets for diagnosis.
	Finished Phase = "Finished"
)

const (
	// StreamDownGrace is how long the agent stream of a Ready server may be
	// down before the server counts as unplayable (design spec 4.4).
	StreamDownGrace = 15 * time.Second

	// ReconnectGrace is StreamDownGrace for a server the operator has not
	// heard from since it began serving agents: the whole fleet dialling
	// back in after an operator restart, not one server going quiet. The
	// agent's own worst case rather than a number chosen between two harms:
	// a 30 s backoff cap, plus its jitter, plus one report interval. Measured
	// 2026-08-26, an agent greets again 12.5-17.8 s after the operator is
	// back, so the 15 s above lost about half the fleet on every restart.
	ReconnectGrace = 45 * time.Second

	// FlapResetWindow is how long a server must stay Ready before its
	// readiness-loss counter is forgiven.
	FlapResetWindow = 10 * time.Minute

	// VelocityReadTimeout is what a proxy waits on a silent backend before it
	// disconnects the players on it, and it is the deadline the drain in the
	// Ready branch below is racing.
	//
	// It duplicates internal/render/defaults/velocity.default.toml, because a
	// number in a TOML file nothing reads is a number that drifts;
	// TestTheShippedVelocityDefaultMatchesTheConstant keeps the two together.
	// It is Velocity's own default as well, so a cluster that replaces the
	// file rather than overlaying it agrees with this by luck.
	VelocityReadTimeout = 30 * time.Second
)

// RescueWindow is how long the operator has to move players off a backend
// whose node has died, before Velocity disconnects them itself.
//
// The arithmetic is the whole of it. Reports stop the instant the node does,
// and PlayersStale says so after twice the report interval; from that moment
// the Ready branch below deregisters the server and starts a drain. Velocity
// starts its own clock at the same instant and fires at VelocityReadTimeout,
// with no event and nothing a plugin can intervene in. So the room the
// operator has is what is left of the read timeout after the staleness rule
// has spent its share: twenty seconds at the default five-second report
// interval, and negative above fifteen.
//
// readTimeout comes from the proxy's Hello, which reads what Velocity
// actually parsed -- after the image's velocity.toml, after any configOverlay
// the user mounted. The operator can see none of those: the overlay is
// somebody else's ConfigMap, mounted by name and never read here.
//
// Zero means no proxy has said, and falls back to VelocityReadTimeout. A
// namespace with several proxies answers with the smallest
// (agent.Registry.ShortestReadTimeout): whichever gives up first is the one
// that kicks the players.
func RescueWindow(reportInterval, readTimeout time.Duration) time.Duration {
	if readTimeout <= 0 {
		readTimeout = VelocityReadTimeout
	}
	return readTimeout - 2*reportInterval
}

// MaxReadinessLosses is the number of readiness losses after which a server is
// considered broken rather than flapping.
const MaxReadinessLosses int32 = 3

// Reasons carried in the decision and mirrored into the CR condition.
const (
	ReasonPodPending        = "PodPending"
	ReasonPodRunning        = "PodRunning"
	ReasonReadyGatePassed   = "ReadyGatePassed"
	ReasonReadinessLost     = "ReadinessLost"
	ReasonDeletionRequested = "DeletionRequested"
	ReasonDrained           = "Drained"
	ReasonDrainTimeout      = "DrainTimeout"
	ReasonPodLost           = "PodLost"
	ReasonPodNeverCreated   = "PodNeverCreated"
	ReasonPodTerminal       = "PodTerminal"
	ReasonRetiring          = "Retiring"
	// ReasonJoinsOpen marks a server returning to the proxies' routing table
	// because its round has not ended.
	ReasonJoinsOpen       = "JoinsOpen"
	ReasonMaxStaleElapsed = "MaxStaleElapsed"
	// ReasonDrainingBeforeCleanup marks a Failed server whose players are being
	// moved off before its pod is removed.
	ReasonDrainingBeforeCleanup = "DrainingBeforeCleanup"
	ReasonStartupTimeout        = "StartupTimeout"
	ReasonFlapping              = "Flapping"
	ReasonRetentionElapsed      = "RetentionElapsed"
	ReasonTerminating           = "Terminating"
	ReasonUnknownPhase          = "UnknownPhase"
	// ReasonRoundFinished marks the round's end, not the pod's: a Ready server
	// carries it on deregistering while still running, and only later, once
	// the pod itself stops, does the same reason carry it into Finished.
	ReasonRoundFinished = "RoundFinished"
	// ReasonFinishedRetentionElapsed marks a finished server that has been
	// kept long enough.
	ReasonFinishedRetentionElapsed = "FinishedRetentionElapsed"
)

// Inputs is everything the state machine may look at. The controller fills it
// from the CR status, the pod and the agent registry.
type Inputs struct {
	// DeletionRequested is true once the Server CR carries a deletion
	// timestamp, or the group decided to remove this server.
	DeletionRequested bool

	// PodExists is true if the pod backing this server was found.
	PodExists bool
	// PodLost is true if status.podName is set but the pod is gone. The players
	// of that pod are gone with it.
	PodLost bool
	// PodRunning is true if the pod reached phase Running.
	PodRunning bool
	// PodReady is true if the readiness probe (the SLP health check) is green.
	PodReady bool
	// PodTerminal is true if the pod reached phase Failed or Succeeded, or a
	// container is in CrashLoopBackOff past the operator's tolerance.
	PodTerminal bool

	// StartupDeadlineReached is true if the current attempt to become playable
	// has run past the operator's startup deadline. The clock is re-armed on
	// every entry into Starting, so this bounds the attempt and not the age of
	// the pod: a long-lived server that loses readiness gets a full deadline to
	// recover in, and is failed if it does not.
	StartupDeadlineReached bool

	// PodCreationDeadlineReached is true if no pod was ever created for this
	// server and the wait for one has run out. Separate from
	// StartupDeadlineReached because status.podName stays empty here, so
	// PodLost never applies and nothing else gives such a server a clock: a
	// namespace whose policy or quota refuses the pod would otherwise leave it
	// Pending indefinitely, counting against its group's replicas.
	PodCreationDeadlineReached bool

	// AgentReady is true if the in-game agent reported readiness on a live
	// stream.
	AgentReady bool
	// AgentConnected is true while the agent stream is up. It separates "the
	// agent is telling us it is not ready" from "we cannot hear the agent" —
	// the first is immediate, the second is tolerated for StreamDownGrace.
	AgentConnected bool
	// AgentStreamDownFor is how long the agent stream has been broken. Zero
	// while the stream is up.
	AgentStreamDownFor time.Duration
	// AgentUnheard is true for a pod the operator has not heard from since
	// it began serving agents, which after a restart is every pod. Such a
	// stream is down because the operator was away, and it gets
	// ReconnectGrace rather than StreamDownGrace.
	AgentUnheard bool

	// AgentSilent is true when the stream is up, the agent has reported before,
	// and it has stopped. That is not the same state as a broken stream.
	//
	// A node that is hard-powered off, or a network that black-holes, sends no
	// FIN and no RST, so the operator's socket goes on looking connected for
	// minutes while TCP retransmits, AgentConnected stays true, and a Ready
	// server on a dead node would go on being sent new players. The reports
	// are the signal that does move: they stop at once. An operator restart
	// would look identical but leaves AgentConnected false.
	//
	// It is also why the operator sets no transport keepalive: breaking this
	// stream would turn this state into the ordinary broken one below, which
	// is tolerated for StreamDownGrace and carries no StartDrain -- and would
	// do it more slowly than the reports do. internal/agentserver's
	// MaxConnectionIdle carries the same argument from the other side.
	AgentSilent bool

	// ReadinessLosses is how often this server already fell out of Ready.
	ReadinessLosses int32
	// ReadyFor is how long the server has been continuously Ready.
	ReadyFor time.Duration

	// WasRegistered is true if this server was ever registered with the proxies
	// during the life of its current pod. A Starting server that fell out of
	// Ready still has its players connected — deregistering stopped new joins,
	// it did not move anyone — so the phase alone cannot tell us whether players
	// are at risk.
	WasRegistered bool

	// PlayersOnline is the last reported player count.
	PlayersOnline int32
	// PlayersStale is true if that count is older than twice the report
	// interval. A stale count counts as occupied.
	PlayersStale bool
	// Slots is the reported capacity. Informational for the decision.
	Slots int32

	// ProxyAttached is how many players the proxies say are on, or on their
	// way to, this server, and ProxyAttachStale says a proxy that used to
	// report stopped. Both come from agent.Registry.AttachedTo.
	//
	// They exist because this server's own count cannot see a player who is
	// arriving: a backend counts a player only once they finish the
	// configuration phase, so a drain can read a server empty while a
	// connection to it is still in flight.
	//
	// Occupied adds them and never subtracts, which is what makes a fleet with
	// proxies too old to report safe: such a proxy contributes zero.
	ProxyAttached    int32
	ProxyAttachStale bool

	// CountPredatesDrain is true when this server's own player count was
	// taken before the drain it is being asked about began.
	//
	// Freshness and recency are different questions. PlayersStale says whether
	// the number is recent enough to believe; this says whether it is about
	// the right moment. A count from four seconds ago is perfectly fresh and
	// still says nothing about a player who joined three seconds ago. Every
	// source is asked the same way, which is why AttachedTo folds the
	// identical rule into ProxyAttachStale.
	CountPredatesDrain bool

	// DrainDeadlineReached is true once drain.timeoutSeconds elapsed.
	DrainDeadlineReached bool
	// FailedRetentionElapsed is true once failedRetentionSeconds elapsed.
	FailedRetentionElapsed bool

	// RetirementRequested is the group's instruction to enter soft drain,
	// read from Server.spec.retire. The group decides, because only it knows
	// the generation, the update budget and whether a replacement is ready;
	// this package only carries out the transition.
	RetirementRequested bool

	// RoundEnded is set once the server has said its round is over.
	//
	// Read from status.roundEndedAt and not from the registry, which is
	// memory: an operator restart between the pod stopping and the pass that
	// reads this would otherwise turn a finished round into a failure. It is
	// the server's own word, and false for a server that never said it.
	RoundEnded bool

	// FinishedRetentionElapsed is true once a Finished server has been kept
	// for its group's finished retention.
	FinishedRetentionElapsed bool

	// Registered is whether the proxies currently have this server.
	Registered bool
	// MaxStaleReached is true once a retiring server has waited longer than
	// spec.update.maxStaleSeconds. It is measured from status.retiringSince
	// — the wait in soft drain — and not from the group's generation change.
	MaxStaleReached bool
}

// Occupied reports whether the server must be treated as carrying players.
// A stale count counts as occupied: one server too many beats one kick.
//
// Three terms, and every one of them can only make the answer true. That is
// the property to preserve: a rule that could turn occupied into empty by
// adding a source would make a fleet's upgrade order load-bearing. The proxy
// terms come from agents that may be older than this operator and say
// nothing, which is indistinguishable from saying zero and is meant to be.
// See Inputs.ProxyAttached for why they are asked and
// Inputs.CountPredatesDrain for why recency is not freshness; that one costs
// at most a report interval against a drain timeout measured in minutes.
func (in Inputs) Occupied() bool {
	return in.PlayersStale || in.PlayersOnline > 0 ||
		in.ProxyAttachStale || in.ProxyAttached > 0 ||
		in.CountPredatesDrain
}

// Decision is what the controller has to do. Next is always set.
type Decision struct {
	// Next is the phase to write into the status.
	Next Phase
	// Register asks the proxies to take this server into their registry.
	Register bool
	// Deregister asks the proxies to drop it. Set on every exit from Ready.
	Deregister bool
	// StartDrain asks the proxies to move the players off this server.
	StartDrain bool
	// CountReadinessLoss increments status.readinessLosses.
	CountReadinessLoss bool
	// ResetReadinessLosses zeroes status.readinessLosses.
	ResetReadinessLosses bool
	// DeletePod means the pod may go: no players are at risk.
	DeletePod bool
	// Reason is the machine-readable cause, mirrored into the condition.
	Reason string
	// Message is the human-readable cause.
	Message string
}

// Decide maps the current phase and the observed inputs to the next phase.
func Decide(current Phase, in Inputs) Decision {
	switch current {
	case Terminating:
		return Decision{
			Next: Terminating, DeletePod: true,
			Reason: ReasonTerminating, Message: "pod is being deleted",
		}

	case Failed:
		if in.DeletionRequested || in.FailedRetentionElapsed {
			// A server can fail with its sessions untouched: flapping
			// readiness deregisters to stop new joins, it does not move
			// anyone off. Cleaning such a server up without draining first
			// would drop every player still on it.
			if in.Occupied() && in.WasRegistered && !in.PodLost && !in.PodTerminal &&
				!in.DrainDeadlineReached {
				return Decision{
					Next: Failed, StartDrain: true,
					Reason:  ReasonDrainingBeforeCleanup,
					Message: "moving players off a failed server before removing it",
				}
			}
			if in.DeletionRequested {
				return Decision{
					Next: Terminating, DeletePod: true,
					Reason: ReasonDeletionRequested, Message: "deletion requested for a failed server",
				}
			}
			return Decision{
				Next: Terminating, DeletePod: true,
				Reason: ReasonRetentionElapsed, Message: "failed retention elapsed",
			}
		}
		return Decision{
			Next:   Failed,
			Reason: ReasonPodTerminal, Message: "kept for diagnosis",
		}

	case Finished:
		if in.DeletionRequested || in.FinishedRetentionElapsed {
			// No drain: the pod is terminal, so its sessions went with it.
			// That is the same reasoning the terminal branch below gives, and
			// it is why this case is shorter than Failed's -- a Finished
			// server is only ever reached through a terminal pod.
			reason := ReasonFinishedRetentionElapsed
			message := "finished retention elapsed"
			if in.DeletionRequested {
				reason, message = ReasonDeletionRequested, "deletion requested for a finished server"
			}
			return Decision{
				Next: Terminating, DeletePod: true,
				Reason: reason, Message: message,
			}
		}
		return Decision{
			Next:   Finished,
			Reason: ReasonRoundFinished, Message: "the round is over",
		}

	case Draining:
		if in.PodLost {
			return Decision{
				Next: Terminating, DeletePod: true,
				Reason: ReasonPodLost, Message: "pod disappeared during drain",
			}
		}
		if in.PodTerminal {
			return Decision{
				Next: Terminating, DeletePod: true,
				Reason: ReasonPodTerminal, Message: "pod reached a terminal phase during drain, its players are already gone",
			}
		}
		if !in.Occupied() {
			return Decision{
				Next: Terminating, DeletePod: true,
				Reason: ReasonDrained, Message: "no players left",
			}
		}
		if in.DrainDeadlineReached {
			return Decision{
				Next: Terminating, DeletePod: true,
				Reason: ReasonDrainTimeout, Message: "drain deadline reached with players online",
			}
		}
		return Decision{
			Next:   Draining,
			Reason: ReasonDeletionRequested, Message: "waiting for players to leave",
		}

	case Retiring:
		if in.PodLost {
			return Decision{
				Next: Terminating, DeletePod: true,
				Reason: ReasonPodLost, Message: "pod disappeared while retiring",
			}
		}
		if in.PodTerminal {
			return Decision{
				Next: Terminating, DeletePod: true,
				Reason:  ReasonPodTerminal,
				Message: "pod reached a terminal phase while retiring, its players are already gone",
			}
		}
		// Empty first, and before the two escalations below: a retiring
		// server that has already run empty needs neither a drain nor a
		// deadline, and sending it through Draining would cost a reconcile
		// and emit a move for nobody.
		if !in.Occupied() {
			return Decision{
				Next: Terminating, DeletePod: true,
				Reason: ReasonDrained, Message: "no players left",
			}
		}
		if in.DeletionRequested {
			return Decision{
				Next: Draining, StartDrain: true,
				Reason: ReasonDeletionRequested, Message: "deletion requested, moving players off",
			}
		}
		if in.MaxStaleReached {
			return Decision{
				Next: Draining, StartDrain: true,
				Reason:  ReasonMaxStaleElapsed,
				Message: "stale deadline reached, moving players off",
			}
		}
		// No Deregister here: entering Retiring did that once. Repeating it
		// every pass would re-emit the proxy call for the whole wait.
		return Decision{
			Next:   Retiring,
			Reason: ReasonRetiring, Message: "waiting for players to leave",
		}

	case Pending, Starting, Ready:
		// handled below
	default:
		return Decision{
			Next:   Pending,
			Reason: ReasonUnknownPhase, Message: "unknown phase, restarting the state machine",
		}
	}

	// From here on current is Pending, Starting or Ready.

	if in.PodLost {
		return Decision{
			Next: Terminating, Deregister: current == Ready, DeletePod: true,
			Reason: ReasonPodLost, Message: "pod disappeared",
		}
	}

	if in.DeletionRequested {
		// A Ready server is currently registered with the proxies. A Starting
		// server that fell out of Ready (WasRegistered) may still have players
		// connected from before: the readiness-loss fallback deregisters to stop
		// new joins, it does not move anyone off. Both cases must drain. Only a
		// server that was never registered can go straight away.
		if current == Ready || in.WasRegistered {
			return Decision{
				Next: Draining, Deregister: current == Ready, StartDrain: true,
				Reason: ReasonDeletionRequested, Message: "deletion requested, moving players off",
			}
		}
		return Decision{
			Next: Terminating, DeletePod: true,
			Reason: ReasonDeletionRequested, Message: "deletion requested before the server was ever registered",
		}
	}

	if in.PodTerminal {
		// A terminal pod is never drained: the process is already down and its
		// sessions went with it, so there is nobody left to move off.
		//
		// The server's own word decides which terminal phase this is. Not the
		// exit code: a System.exit(0) in a shutdown hook would make a crash
		// read as a win, and the word is the one thing only the server knows.
		if in.RoundEnded {
			return Decision{
				Next: Finished, Deregister: current == Ready,
				Reason: ReasonRoundFinished, Message: "the round is over and the pod stopped",
			}
		}
		return Decision{
			Next: Failed, Deregister: current == Ready,
			Reason: ReasonPodTerminal, Message: "pod reached a terminal phase",
		}
	}

	// From here on the pod is neither lost nor terminal — both returned above.
	// A server that failed while it was registered still has live sessions on
	// it, so failing it has to take its players off rather than strand them.
	drainOnFailure := in.WasRegistered

	if in.ReadinessLosses >= MaxReadinessLosses {
		return Decision{
			Next: Failed, Deregister: current == Ready, StartDrain: drainOnFailure,
			Reason: ReasonFlapping, Message: "too many readiness losses",
		}
	}

	// The startup deadline bounds the current attempt to become playable. The
	// controller re-arms status.startedAt on every entry into Starting, so this
	// measures the attempt and not the age of the pod: a server that has served
	// for hours and blips once gets a fresh deadline to recover in, while one
	// that fell out of Ready and cannot come back is still failed — the flap
	// counter alone would never catch it, because losses are only counted on a
	// Ready -> Starting transition that a permanently red probe never repeats.
	if in.StartupDeadlineReached && current != Ready {
		return Decision{
			Next: Failed, StartDrain: drainOnFailure,
			Reason: ReasonStartupTimeout, Message: "server did not become ready in time",
		}
	}

	switch current {
	case Pending:
		if in.PodExists && in.PodRunning {
			return Decision{Next: Starting, Reason: ReasonPodRunning, Message: "pod is running"}
		}
		if in.PodCreationDeadlineReached {
			// No drain and no deregistration: a server that never had a pod
			// was never registered and nobody is on it. Failed rather than
			// Terminating, so the group's backoff counts it and the object
			// stays for its retention as the record of what happened -- the
			// condition set beside it names what refused the create.
			return Decision{
				Next:    Failed,
				Reason:  ReasonPodNeverCreated,
				Message: "no pod was ever created for this server",
			}
		}
		return Decision{Next: Pending, Reason: ReasonPodPending, Message: "waiting for the pod"}

	case Starting:
		if in.PodExists && in.PodRunning && in.PodReady && in.AgentReady && in.AgentStreamDownFor < StreamDownGrace {
			return Decision{
				Next: Ready, Register: true,
				Reason: ReasonReadyGatePassed, Message: "probe green and agent ready",
			}
		}
		return Decision{
			Next:   Starting,
			Reason: ReasonPodPending, Message: "waiting for both ready signals",
		}

	default: // Ready
		lost := !in.PodReady
		if !lost {
			if in.AgentConnected {
				// A live stream that reports not-ready is an immediate loss,
				// and so is one that has stopped reporting at all -- see
				// Inputs.AgentSilent for why the second is a different state
				// from a broken stream and why nothing else catches it.
				lost = !in.AgentReady || in.AgentSilent
			} else {
				// A broken stream is tolerated until the grace expires; the
				// player count goes stale meanwhile, so the server counts as
				// occupied and is protected from deletion either way.
				grace := StreamDownGrace
				if in.AgentUnheard {
					grace = ReconnectGrace
				}
				lost = in.AgentStreamDownFor >= grace
			}
		}
		if lost {
			// StartDrain when the agent went silent, and not otherwise.
			//
			// Deregistering stops new joins and does nothing for the players
			// already there, which is fine when a server is merely
			// unhealthy -- it may come back, and moving people costs them a
			// loading screen. It is not fine when the backend is gone: those
			// players sit on a socket that will never answer, and Velocity
			// disconnects them outright on its read timeout without firing a
			// KickedFromServerEvent, so the agent's own Rescue never sees
			// them. RescueWindow is the room that leaves.
			//
			// Starting rather than Draining, so a server whose agent was
			// merely wedged can come back. What a false positive costs is a
			// server switch; what it buys is that a real one is not a kick.
			return Decision{
				Next: Starting, Deregister: true, CountReadinessLoss: true,
				StartDrain: in.AgentSilent,
				Reason:     ReasonReadinessLost, Message: "server lost a ready signal",
			}
		}
		// After the readiness check on purpose: a server that has just lost a
		// ready signal is already being deregistered by that path, and letting
		// retirement overtake it would swallow the readiness loss the flap
		// counter needs.
		if in.RetirementRequested {
			return Decision{
				Next: Retiring, Deregister: true,
				Reason: ReasonRetiring, Message: "retiring for a rolling update",
			}
		}
		// The round's end takes a server out of the table; a closed door does
		// not. Deregistration is about whether anybody can reach this server
		// at all, the door about whether its seats are capacity.
		//
		// Both directions are conditioned on what the proxies currently have,
		// so this speaks only on a change: otherwise a finished server would
		// be deregistered again on every pass, and each one is a broadcast to
		// every proxy in the namespace.
		if in.RoundEnded && in.Registered {
			return Decision{
				Next: Ready, Deregister: true,
				Reason: ReasonRoundFinished, Message: "the round is over",
			}
		}
		if !in.RoundEnded && !in.Registered {
			return Decision{
				Next: Ready, Register: true,
				Reason: ReasonJoinsOpen, Message: "the server is reachable",
			}
		}
		return Decision{
			Next:                 Ready,
			ResetReadinessLosses: in.ReadinessLosses > 0 && in.ReadyFor >= FlapResetWindow,
			Reason:               ReasonReadyGatePassed, Message: "serving players",
		}
	}
}
