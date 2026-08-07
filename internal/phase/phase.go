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
	// Draining means the server is deregistered and players are being moved.
	// There is no way back to Ready.
	Draining Phase = "Draining"
	// Terminating means the pod is being deleted.
	Terminating Phase = "Terminating"
	// Failed means the server is broken. It is kept for diagnosis and cleaned
	// up after the group's retention.
	Failed Phase = "Failed"
)

const (
	// StreamDownGrace is how long the agent stream of a Ready server may be
	// down before the server counts as unplayable (design spec 4.4).
	StreamDownGrace = 15 * time.Second

	// FlapResetWindow is how long a server must stay Ready before its
	// readiness-loss counter is forgiven.
	FlapResetWindow = 10 * time.Minute
)

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
	ReasonPodTerminal       = "PodTerminal"
	ReasonStartupTimeout    = "StartupTimeout"
	ReasonFlapping          = "Flapping"
	ReasonRetentionElapsed  = "RetentionElapsed"
	ReasonTerminating       = "Terminating"
	ReasonUnknownPhase      = "UnknownPhase"
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

	// StartupDeadlineReached is true if the server did not reach Ready within
	// the operator's startup deadline.
	StartupDeadlineReached bool

	// AgentReady is true if the in-game agent reported readiness on a live
	// stream.
	AgentReady bool
	// AgentStreamDownFor is how long the agent stream has been broken. Zero
	// while the stream is up.
	AgentStreamDownFor time.Duration

	// ReadinessLosses is how often this server already fell out of Ready.
	ReadinessLosses int32
	// ReadyFor is how long the server has been continuously Ready.
	ReadyFor time.Duration

	// PlayersOnline is the last reported player count.
	PlayersOnline int32
	// PlayersStale is true if that count is older than twice the report
	// interval. A stale count counts as occupied.
	PlayersStale bool
	// Slots is the reported capacity. Informational for the decision.
	Slots int32

	// DrainDeadlineReached is true once drain.timeoutSeconds elapsed.
	DrainDeadlineReached bool
	// FailedRetentionElapsed is true once failedRetentionSeconds elapsed.
	FailedRetentionElapsed bool
}

// Occupied reports whether the server must be treated as carrying players.
// A stale count counts as occupied: one server too many beats one kick.
func (in Inputs) Occupied() bool {
	return in.PlayersStale || in.PlayersOnline > 0
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
		if in.FailedRetentionElapsed {
			return Decision{
				Next: Terminating, DeletePod: true,
				Reason: ReasonRetentionElapsed, Message: "failed retention elapsed",
			}
		}
		return Decision{
			Next:   Failed,
			Reason: ReasonPodTerminal, Message: "kept for diagnosis",
		}

	case Draining:
		if in.PodLost {
			return Decision{
				Next: Terminating, DeletePod: true,
				Reason: ReasonPodLost, Message: "pod disappeared during drain",
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
		// Only a Ready server can have players, because only a Ready server is
		// registered with the proxies. Everything else can go straight away.
		if current == Ready {
			return Decision{
				Next: Draining, Deregister: true, StartDrain: true,
				Reason: ReasonDeletionRequested, Message: "deletion requested, moving players off",
			}
		}
		return Decision{
			Next: Terminating, DeletePod: true,
			Reason: ReasonDeletionRequested, Message: "deletion requested before the server took players",
		}
	}

	if in.PodTerminal {
		return Decision{
			Next: Failed, Deregister: current == Ready,
			Reason: ReasonPodTerminal, Message: "pod reached a terminal phase",
		}
	}

	if in.ReadinessLosses >= MaxReadinessLosses {
		return Decision{
			Next: Failed, Deregister: current == Ready,
			Reason: ReasonFlapping, Message: "too many readiness losses",
		}
	}

	if in.StartupDeadlineReached && current != Ready {
		return Decision{
			Next:   Failed,
			Reason: ReasonStartupTimeout, Message: "server did not become ready in time",
		}
	}

	switch current {
	case Pending:
		if in.PodExists && in.PodRunning {
			return Decision{Next: Starting, Reason: ReasonPodRunning, Message: "pod is running"}
		}
		return Decision{Next: Pending, Reason: ReasonPodPending, Message: "waiting for the pod"}

	case Starting:
		if in.PodReady && in.AgentReady && in.AgentStreamDownFor < StreamDownGrace {
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
		if !in.PodReady || !in.AgentReady || in.AgentStreamDownFor >= StreamDownGrace {
			return Decision{
				Next: Starting, Deregister: true, CountReadinessLoss: true,
				Reason: ReasonReadinessLost, Message: "server lost a ready signal",
			}
		}
		return Decision{
			Next:                 Ready,
			ResetReadinessLosses: in.ReadinessLosses > 0 && in.ReadyFor >= FlapResetWindow,
			Reason:               ReasonReadyGatePassed, Message: "serving players",
		}
	}
}
