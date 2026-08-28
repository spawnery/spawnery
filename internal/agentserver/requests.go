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

package agentserver

import (
	"context"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/grpcauth"
)

const (
	// RequestBurst is how many requests one pod may make back to back.
	//
	// Eight, which is generous for a plugin reacting to a player and cheap for
	// the operator: each one is a cache read and a broadcast to a handful of
	// proxies. The bound exists so that a compromised pod cannot make the
	// operator do unbounded work, not so that a busy plugin has to pace
	// itself.
	RequestBurst = 8
	// RequestRefill is how long one token takes to come back.
	RequestRefill = time.Second
)

// requestLimiter is a token bucket per pod.
//
// grpcauth.PeerLimiter has the same shape and is not reused: it is keyed by
// peer address for a different question -- how often a pod may miss the
// TokenReview cache -- and its allow is unexported. Two buckets for two
// questions is the honest split; sharing one would tie a connection's budget
// to a plugin's.
type requestLimiter struct {
	now func() time.Time

	mu      sync.Mutex
	buckets map[string]struct {
		tokens float64
		last   time.Time
	}
}

func newRequestLimiter(now func() time.Time) *requestLimiter {
	return &requestLimiter{
		now: now,
		buckets: map[string]struct {
			tokens float64
			last   time.Time
		}{},
	}
}

func (l *requestLimiter) allow(pod string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, seen := l.buckets[pod]
	if !seen {
		b.tokens = RequestBurst
	} else {
		b.tokens += now.Sub(b.last).Seconds() / RequestRefill.Seconds()
		if b.tokens > RequestBurst {
			b.tokens = RequestBurst
		}
	}
	b.last = now
	if b.tokens < 1 {
		l.buckets[pod] = b
		return false
	}
	b.tokens--
	l.buckets[pod] = b
	return true
}

// answerConnect resolves a move request and says what the operator did with
// it.
//
// # The bounds, and which of them is not a check
//
// **The namespace bound is structural.** A ConnectRequest names a player and a
// target and carries no namespace at all, and both are resolved inside
// id.Namespace -- the namespace the pod's own ServiceAccount token
// authenticated. There is no field an agent could put another network's name
// in, which is why this one has no `if` and cannot be forgotten in a later
// edit. Milestone 2a's promise that a compromised pod cannot harm another is
// carried here by the shape rather than by a guard.
//
// The remaining two are checks and each has its own test, because a single
// test asserting "it was refused" passes when the wrong one fired.
//
// # What it promises
//
// Nothing about the player arriving. The proxy that carries the move does not
// wait on Velocity's own future -- see agentpb.ConnectResult -- so ordered
// means the instruction reached the proxies of this namespace and no more.
func (s *Server) answerConnect(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	reqID uint64,
	req *agentpb.ConnectRequest,
) *agentpb.CloudResponse {
	if !s.requestRate.allow(id.PodUID) {
		return refuse(reqID, agentpb.RequestError_RATE_LIMITED,
			"this pod has asked more often than the operator will answer")
	}

	// The namespace is the token's, never the message's. Everything below
	// resolves inside it, which is the whole of the cross-network bound.
	state, err := s.opts.State.Build(ctx, id.Namespace)
	if err != nil {
		logger.V(1).Info("could not read the network for a connect request", "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE,
			"the operator could not read this network just now")
	}

	var player *agentpb.RosterEntry
	for _, p := range state.GetPlayers() {
		if p.GetUuid() == req.GetPlayerUuid() {
			player = p
			break
		}
	}
	if player == nil {
		// Ordinary rather than exceptional: a player who logged out between a
		// plugin's call and this request lands here, and so does one on
		// another network -- which is the cross-network bound observed from
		// the outside, since there is no namespace to have got wrong.
		return refuse(reqID, agentpb.RequestError_NOT_FOUND,
			"no player with that id is on this network")
	}

	target, ok := resolveTarget(state, req)
	if !ok {
		return refuse(reqID, agentpb.RequestError_NOT_FOUND,
			"no server or group by that name is on this network")
	}

	if player.GetServer() == target {
		// Nothing ordered and nothing wrong.
		return connected(reqID, &agentpb.ConnectResult{AlreadyThere: true, Target: target})
	}

	s.opts.Proxies.Move(id.Namespace, req.GetPlayerUuid(), target)
	return connected(reqID, &agentpb.ConnectResult{Ordered: true, Target: target})
}

// resolveTarget turns a request's target into one server name.
//
// A named server has to exist and be registered: an unregistered one is a
// server the proxies cannot route to, so ordering a move there would put the
// player nowhere. A named group is the operator's choice among that group's
// registered servers, and it takes the one with the most room -- which is the
// figure a plugin has no way to compare for itself without racing the mirror.
func resolveTarget(state *agentpb.NetworkState, req *agentpb.ConnectRequest) (string, bool) {
	switch {
	case req.GetServer() != "":
		for _, srv := range state.GetServers() {
			if srv.GetName() == req.GetServer() && srv.GetRegistered() {
				return srv.GetName(), true
			}
		}
	case req.GetGroup() != "":
		best, bestFree := "", -1
		for _, srv := range state.GetServers() {
			if srv.GetGroup() != req.GetGroup() || !srv.GetRegistered() {
				continue
			}
			if free := int(srv.GetSlots() - srv.GetPlayers()); free > bestFree {
				best, bestFree = srv.GetName(), free
			}
		}
		if best != "" {
			return best, true
		}
	}
	return "", false
}

// refuse builds an error answer and counts it.
//
// The message is free text for a person; the reason is what a caller branches
// on. Neither carries a player's name into the counter -- see RequestsRefused.
func refuse(reqID uint64, reason agentpb.RequestError_Reason, message string) *agentpb.CloudResponse {
	RequestsRefused.WithLabelValues(reason.String()).Inc()
	return &agentpb.CloudResponse{
		Id: reqID,
		Result: &agentpb.CloudResponse_Error{
			Error: &agentpb.RequestError{Reason: reason, Message: message},
		},
	}
}

// connected wraps a successful answer, so the two success paths above cannot
// disagree about the id they echo.
func connected(reqID uint64, result *agentpb.ConnectResult) *agentpb.CloudResponse {
	return &agentpb.CloudResponse{
		Id:     reqID,
		Result: &agentpb.CloudResponse_Connect{Connect: result},
	}
}
