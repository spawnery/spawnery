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

// Package phase is the Server state machine as a pure function. Every rule
// about registration and deletion lives here.
package phase

import "time"

type Phase string

const (
	Pending  Phase = "Pending"
	Starting Phase = "Starting"
	Ready    Phase = "Ready"
	// Retiring is soft drain: deregistered, but players are left alone until
	// they leave. No drain timeout applies, because a lobby can legitimately
	// sit here for hours; only spec.update.maxStaleSeconds bounds it.
	Retiring Phase = "Retiring"
	// Draining: deregistered, players being moved. There is no way back to Ready.
	Draining    Phase = "Draining"
	Terminating Phase = "Terminating"
	// Failed is kept for diagnosis and cleaned up after the group's retention.
	Failed Phase = "Failed"
	// Finished means the server said its round is over and then its pod stopped.
	// Terminal like Failed, but not a fault: no backoff, no Degraded, and a
	// short retention instead of the diagnostic hour.
	Finished Phase = "Finished"
)

const (
	// StreamDownGrace is how long a Ready server's agent stream may be down
	// before the server counts as unplayable.
	StreamDownGrace = 15 * time.Second

	// ReconnectGrace replaces StreamDownGrace for a server not heard from since
	// the operator began serving agents, i.e. the fleet dialling back in after
	// a restart: the agent's 30 s backoff cap plus jitter plus one report
	// interval.
	ReconnectGrace = 45 * time.Second

	FlapResetWindow = 10 * time.Minute

	// VelocityReadTimeout is what a proxy waits on a silent backend before it
	// disconnects the players on it. It duplicates
	// internal/render/defaults/velocity.default.toml (kept equal by
	// TestTheShippedVelocityDefaultMatchesTheConstant) and is also Velocity's
	// own default.
	VelocityReadTimeout = 30 * time.Second
)

// RescueWindow is how long the operator has to move players off a backend
// whose node has died before Velocity disconnects them itself: Velocity's
// clock and the PlayersStale rule (two report intervals) start at the same
// instant. readTimeout is what the proxies reported in their Hello, since an
// overlay the operator never reads may have changed it; zero falls back to
// VelocityReadTimeout.
func RescueWindow(reportInterval, readTimeout time.Duration) time.Duration {
	if readTimeout <= 0 {
		readTimeout = VelocityReadTimeout
	}
	return readTimeout - 2*reportInterval
}

// Terminal reports whether no player can join the server, it holds no slot in
// its group, and its name may be reused. Callers ask this instead of spelling
// the disjunction, so a new terminal phase only has to be added here.
func Terminal(p Phase) bool {
	return p == Failed || p == Finished
}

const MaxReadinessLosses int32 = 3

// Reasons carried in the decision and mirrored into the CR condition.
const (
	ReasonPodPending            = "PodPending"
	ReasonPodRunning            = "PodRunning"
	ReasonReadyGatePassed       = "ReadyGatePassed"
	ReasonReadinessLost         = "ReadinessLost"
	ReasonDeletionRequested     = "DeletionRequested"
	ReasonDrained               = "Drained"
	ReasonDrainTimeout          = "DrainTimeout"
	ReasonPodLost               = "PodLost"
	ReasonPodNeverCreated       = "PodNeverCreated"
	ReasonPodTerminal           = "PodTerminal"
	ReasonRetiring              = "Retiring"
	ReasonRetirementWithdrawn   = "RetirementWithdrawn"
	ReasonJoinsOpen             = "JoinsOpen"
	ReasonMaxStaleElapsed       = "MaxStaleElapsed"
	ReasonDrainingBeforeCleanup = "DrainingBeforeCleanup"
	ReasonStartupTimeout        = "StartupTimeout"
	ReasonFlapping              = "Flapping"
	ReasonRetentionElapsed      = "RetentionElapsed"
	ReasonStoppingFailedPod     = "StoppingFailedPod"
	ReasonTerminating           = "Terminating"
	ReasonForceStopped          = "ForceStopped"
	ReasonUnknownPhase          = "UnknownPhase"
	// ReasonRoundFinished marks the round's end, not the pod's: a Ready server
	// carries it on deregistering while still running, and again into Finished
	// once the pod stops.
	ReasonRoundFinished            = "RoundFinished"
	ReasonFinishedRetentionElapsed = "FinishedRetentionElapsed"
)

type Inputs struct {
	// DeletionRequested is also set when the group decided to remove this server.
	DeletionRequested bool

	PodExists  bool
	PodLost    bool
	PodRunning bool
	// PodReady is the readiness probe (the SLP health check).
	PodReady bool
	// PodTerminal also covers CrashLoopBackOff past the operator's tolerance.
	PodTerminal         bool
	GroupHasReadyServer bool

	// StartupDeadlineReached bounds the current attempt to become playable, not
	// the pod's age: the clock is re-armed on every entry into Starting.
	StartupDeadlineReached bool

	// PodCreationDeadlineReached gives a server whose pod was never created a
	// clock: status.podName stays empty, so PodLost never applies, and a pod
	// refused by policy or quota would otherwise leave it Pending forever.
	PodCreationDeadlineReached bool

	AgentReady bool
	// AgentConnected separates "the agent says it is not ready" (immediate)
	// from "we cannot hear the agent" (tolerated for StreamDownGrace).
	AgentConnected     bool
	AgentStreamDownFor time.Duration
	// AgentUnheard: the operator has not heard from this pod since it began
	// serving agents; such a stream gets ReconnectGrace.
	AgentUnheard bool

	// AgentSilent: the stream is up, the agent has reported before, and it has
	// stopped. A hard-powered-off node sends no FIN or RST, so the socket looks
	// connected for minutes while the reports stop at once. This is also why
	// the operator sets no transport keepalive: breaking the stream would turn
	// this into the tolerated broken-stream case, which carries no StartDrain.
	AgentSilent bool

	ReadinessLosses int32
	ReadyFor        time.Duration

	// WasRegistered: registered during the life of the current pod. A Starting
	// server that fell out of Ready still has its players connected, since
	// deregistering only stopped new joins.
	WasRegistered bool

	PlayersOnline int32
	// PlayersStale: the count is older than twice the report interval.
	PlayersStale bool
	// Slots is informational for the decision.
	Slots int32

	// ProxyAttached is how many players the proxies say are on, or on their way
	// to, this server (agent.Registry.AttachedTo). A backend counts a player only
	// after the configuration phase, so its own count misses one in flight.
	// A proxy too old to report contributes zero, which Occupied tolerates.
	ProxyAttached    int32
	ProxyAttachStale bool

	// CountPredatesDrain: the server's own count was taken before the drain it
	// is asked about began. A fresh count can still predate a join.
	CountPredatesDrain bool

	DrainDeadlineReached   bool
	FailedRetentionElapsed bool

	// RetirementRequested is read from Server.spec.retire; the group decides,
	// since only it knows the generation, the budget and the replacement.
	RetirementRequested bool

	// ForceStopRequested is Server.spec.forceStop: kill the pod now, whatever
	// is on it.
	ForceStopRequested bool

	// RoundEnded is read from status.roundEndedAt, not the in-memory registry, so
	// an operator restart cannot turn a finished round into a failure.
	RoundEnded bool

	FinishedRetentionElapsed bool

	Registered bool
	// MaxStaleReached is measured from status.retiringSince, not from the
	// group's generation change.
	MaxStaleReached bool
}

// Occupied reports whether the server must be treated as carrying players.
// Every term can only make it true: a stale count counts as occupied, and a
// source that says nothing is indistinguishable from zero, so no upgrade
// order can turn occupied into empty.
func (in Inputs) Occupied() bool {
	return in.PlayersStale || in.PlayersOnline > 0 ||
		in.ProxyAttachStale || in.ProxyAttached > 0 ||
		in.CountPredatesDrain
}

type Decision struct {
	Next     Phase
	Register bool
	// Deregister is set on every exit from Ready.
	Deregister           bool
	StartDrain           bool
	CountReadinessLoss   bool
	ResetReadinessLosses bool
	// DeletePod means no players are at risk, or an administrator accepted the
	// risk with spec.forceStop.
	DeletePod bool
	Reason    string
	Message   string
}

func Decide(current Phase, in Inputs) Decision {
	if in.ForceStopRequested {
		return Decision{
			Next: Terminating, DeletePod: true, Deregister: in.Registered,
			Reason: ReasonForceStopped, Message: "force-stopped: the pod is killed without a drain",
		}
	}
	switch current {
	case Terminating:
		return Decision{
			Next: Terminating, DeletePod: true,
			Reason: ReasonTerminating, Message: "pod is being deleted",
		}

	case Failed:
		if in.DeletionRequested || in.FailedRetentionElapsed {
			// Flapping readiness deregisters without moving anyone off, so a failed
			// server can still carry players.
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
		// Stop a pod that came up after its server was failed, so it does not run
		// unused for the whole retention. Never registered means no proxy sent
		// anyone there, whatever a not-yet-reported agent would say.
		if in.PodExists && in.PodRunning && !in.PodTerminal && in.GroupHasReadyServer &&
			(!in.WasRegistered || !in.Occupied()) {
			return Decision{
				Next: Failed, DeletePod: true,
				Reason: ReasonStoppingFailedPod, Message: "stopping the late pod of a failed server; the object stays for diagnosis",
			}
		}
		return Decision{
			Next:   Failed,
			Reason: ReasonPodTerminal, Message: "kept for diagnosis",
		}

	case Finished:
		if in.DeletionRequested || in.FinishedRetentionElapsed {
			// Lost readiness after endRound leaves a Finished server with a running pod,
			// so players may still be on it.
			if in.Occupied() && in.WasRegistered && !in.PodLost && !in.PodTerminal &&
				!in.DrainDeadlineReached {
				return Decision{
					Next: Finished, StartDrain: true,
					Reason:  ReasonDrainingBeforeCleanup,
					Message: "moving players off a finished server before removing it",
				}
			}
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
		// Before the escalations below: an empty retiring server needs neither a
		// drain nor a deadline.
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
		if !in.RetirementRequested {
			if in.PodRunning && in.PodReady && in.AgentReady && in.AgentStreamDownFor < StreamDownGrace {
				return Decision{
					Next: Ready, Register: true,
					Reason: ReasonRetirementWithdrawn, Message: "retirement was withdrawn",
				}
			}
			return Decision{
				Next:   Retiring,
				Reason: ReasonRetiring, Message: "retirement was withdrawn, waiting for both ready signals",
			}
		}
		if in.MaxStaleReached {
			return Decision{
				Next: Draining, StartDrain: true,
				Reason:  ReasonMaxStaleElapsed,
				Message: "stale deadline reached, moving players off",
			}
		}
		// No Deregister: entering Retiring did that once.
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

	if in.PodLost {
		return Decision{
			Next: Terminating, Deregister: current == Ready, DeletePod: true,
			Reason: ReasonPodLost, Message: "pod disappeared",
		}
	}

	if in.DeletionRequested {
		// A Starting server that fell out of Ready may still carry players: the
		// readiness-loss path deregisters without moving anyone. Only a server
		// that was never registered can go straight away.
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
		// Never drained: the process is down and its sessions went with it. The
		// server's own word decides Finished vs Failed, not the exit code, since a
		// System.exit(0) in a shutdown hook would make a crash read as a win.
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

	drainOnFailure := in.WasRegistered

	if in.ReadinessLosses >= MaxReadinessLosses {
		return Decision{
			Next: Failed, Deregister: current == Ready, StartDrain: drainOnFailure,
			Reason: ReasonFlapping, Message: "too many readiness losses",
		}
	}

	// status.startedAt is re-armed on every entry into Starting. A server that
	// fell out of Ready and stays red is caught here, not by the flap counter,
	// which only counts Ready -> Starting transitions.
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
			// Failed rather than Terminating, so the group's backoff counts it and the
			// object stays as the record of what refused the create.
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
				lost = !in.AgentReady || in.AgentSilent
			} else {
				// The player count goes stale meanwhile, so the server counts as occupied.
				grace := StreamDownGrace
				if in.AgentUnheard {
					grace = ReconnectGrace
				}
				lost = in.AgentStreamDownFor >= grace
			}
		}
		// After endRound the stop is the shutdown the round asked for. A silent
		// agent is excluded: its players may be on a dead backend and need rescue.
		if lost && in.RoundEnded && !in.AgentSilent {
			return Decision{
				Next: Finished, Deregister: in.Registered,
				Reason: ReasonRoundFinished, Message: "the round is over and the server is shutting down",
			}
		}
		if lost {
			// StartDrain only when the agent went silent: the backend is likely gone,
			// and Velocity disconnects its players on the read timeout without a
			// KickedFromServerEvent, so the agent's own Rescue never sees them.
			// Starting rather than Draining, so a merely wedged agent can come back.
			return Decision{
				Next: Starting, Deregister: true, CountReadinessLoss: true,
				StartDrain: in.AgentSilent,
				Reason:     ReasonReadinessLost, Message: "server lost a ready signal",
			}
		}
		// After the readiness check: letting retirement overtake a readiness loss
		// would swallow the loss the flap counter needs.
		if in.RetirementRequested {
			return Decision{
				Next: Retiring, Deregister: true,
				Reason: ReasonRetiring, Message: "retiring for a rolling update",
			}
		}
		// The round's end takes a server out of the table; a closed door does not.
		// Both directions act only on a change, since each one is a broadcast to
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
