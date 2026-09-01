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
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"github.com/spawnery/spawnery/internal/agent"
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

	// BoostDefaultDuration is how long a boost runs when the request names no
	// duration.
	//
	// An hour. What /cloud start usually means is an event, a rush, a Saturday
	// night, and the failure mode of a permanent one is well known: the boost
	// from last weekend is still there in March and nobody remembers why the
	// lobby runs four servers.
	BoostDefaultDuration = time.Hour
	// BoostMaxDuration is the longest one a request may ask for.
	//
	// Twelve hours, which covers any single evening and no more. A need that
	// outlives an evening belongs in the group's own file, where a person
	// reviews it -- and this bound is the only thing that makes an admin
	// discover that file rather than typing a week-long boost every week.
	BoostMaxDuration = 12 * time.Hour

	// AnnounceMaxStateLength is the longest state a server may announce.
	//
	// Sixty-four characters, which is a word or a short phrase and not a
	// sentence. The state is meant to be compared -- a plugin asks whether a
	// server is in the state it cares about -- and a value long enough to
	// carry a message is one somebody will put a message in.
	AnnounceMaxStateLength = 64
	// AnnounceMaxAttributes is how many attributes one announcement may carry.
	AnnounceMaxAttributes = 16
	// AnnounceMaxKeyLength is the longest attribute key.
	AnnounceMaxKeyLength = 64
	// AnnounceMaxValueLength is the longest attribute value.
	//
	// The three numbers above bound one announcement at roughly five
	// kilobytes, and that figure is the one that matters rather than any of
	// them alone: an announcement is carried to every agent in the namespace
	// on every resync, so a network of forty servers pays this forty times
	// over, every resync, for as long as it runs. Generous enough for a
	// description and far too small for a payload.
	AnnounceMaxValueLength = 256
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

// answerCloudRequest routes one request to the verb that answers it.
//
// One dispatcher and not one per direction: a server session and a proxy
// session ask the same questions and differ only in the envelope the answer
// travels in, which each caller wraps. Before this existed the oneof was
// unpacked at both call sites, and the second verb would have made that four
// copies of a chain whose last branch -- the answer for a request kind this
// operator does not know -- is the one nobody would notice going missing.
//
// The unknown-request answer is a refusal and not a silence, and that matters
// more than it looks: an agent waiting on an id it will never hear about holds
// its caller's future until the deadline, so a silent operator turns "this
// operator is older than your plugin" into "the cloud is slow".
func (s *Server) answerCloudRequest(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	req *agentpb.CloudRequest,
) *agentpb.CloudResponse {
	// The bound is here and not in each verb, and that placement is the whole
	// of its reliability: a per-verb check is a line the next verb's author
	// can forget, and forgetting it is invisible -- the verb works, and only
	// a pod asking in a loop ever finds out. An unknown request spends a
	// token too, which is deliberate: it is still work a pod can make the
	// operator do.
	if !s.requestRate.allow(id.PodUID) {
		return refuse(req.GetId(), agentpb.RequestError_RATE_LIMITED,
			"this pod has asked more often than the operator will answer")
	}

	switch {
	case req.GetConnect() != nil:
		return s.answerConnect(ctx, logger, id, req.GetId(), req.GetConnect())
	case req.GetRetire() != nil:
		return s.answerRetire(ctx, logger, id, req.GetId(), req.GetRetire())
	case req.GetBoost() != nil:
		return s.answerBoost(ctx, logger, id, req.GetId(), req.GetBoost())
	case req.GetStopBoost() != nil:
		return s.answerStopBoost(ctx, logger, id, req.GetId(), req.GetStopBoost())
	case req.GetAnnounce() != nil:
		return s.answerAnnounce(logger, id, req.GetId(), req.GetAnnounce())
	default:
		return refuse(req.GetId(), agentpb.RequestError_REASON_UNSPECIFIED,
			"this operator does not know that request")
	}
}

// answerRetire asks one server to stop taking joins.
//
// # The first request that writes, and what bounds it
//
// **The namespace bound is structural, as it is for connect.** A RetireRequest
// names a server and nothing else, and both the resolution and the patch
// happen inside id.Namespace -- the namespace the pod's own ServiceAccount
// token authenticated. There is no field an agent could put another network's
// server in.
//
// Unlike connect, this resolves nothing against the network snapshot, and an
// earlier draft that did was measured to be dead weight: removing the check
// changed no test, because the writer's own namespaced Get already answers
// NOT_FOUND for a name this network does not have. A screening step that reads
// like a bound but cannot fail on its own is worse than none -- the next
// person to touch this would have to work out, as this comment had to, that
// the bound is the Get's key and never was the snapshot.
//
// Refusing an already-retiring server is the one bound here that is not about
// safety. The patch is idempotent and a second one would cost nothing; what it
// would cost is the meaning of the answer. An operator that says "done" to the
// second admin to type the command has told them the first attempt did
// nothing, and the thing people do next is type it again harder.
func (s *Server) answerRetire(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	reqID uint64,
	req *agentpb.RetireRequest,
) *agentpb.CloudResponse {
	// The namespace is the token's, never the message's, and it is the key
	// this resolves under -- so a server this network does not have is
	// answered by the writer rather than screened out beforehand.
	applied, err := s.opts.Writer.Retire(ctx, id.Namespace, req.GetServer())
	switch {
	case errors.Is(err, ErrNoSuchServer):
		// The snapshot said it was there and the cluster says otherwise --
		// ordinary, since the snapshot is allowed to be a moment stale.
		return refuse(reqID, agentpb.RequestError_NOT_FOUND,
			"no server by that name is on this network")
	case err != nil:
		logger.V(1).Info("could not retire a server", "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE,
			"the operator could not write that just now")
	case !applied:
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"that server is already retiring")
	}

	return retired(reqID, &agentpb.RetireResult{Server: req.GetServer()})
}

// answerBoost adds capacity to a group for a while.
//
// # What it refuses, and why each refusal is separate
//
// The namespace bound is structural, as it is for retire: the group is
// resolved under id.Namespace and the request has no field that could name
// another.
//
// Four things are refused and each says which one it was, because an admin who
// is told only "refused" retypes the same command. **A group with no
// spec.scaling**, because a boost on a persistent group would be created,
// counted in the status, and change nothing -- see ErrGroupNotScalable. **Too
// long**, at BoostMaxDuration, which is the bound that makes somebody discover
// the file they should be editing instead. **Too many**, at what the group's
// own ceiling leaves, because §4.4's rule is that a ceiling is an instruction
// and a command typed in a chat window must not lift it. And **fewer than one
// server**, which is not a boost.
//
// Refused rather than quietly capped. A boost that silently becomes something
// other than what was typed is the class of surprise this repository avoids
// everywhere else, and the admin who asked for six and got three would find
// out only by counting servers.
func (s *Server) answerBoost(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	reqID uint64,
	req *agentpb.BoostRequest,
) *agentpb.CloudResponse {
	if req.GetReplicas() < 1 {
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"a boost has to add at least one server")
	}

	// A duration on the wire and an instant here: the two sides do not share a
	// clock, so the operator's is the one that decides when this ends. See
	// agentpb.BoostRequest.
	duration := time.Duration(req.GetDurationSeconds()) * time.Second
	if duration <= 0 {
		duration = BoostDefaultDuration
	}
	if duration > BoostMaxDuration {
		return refuse(reqID, agentpb.RequestError_REFUSED,
			fmt.Sprintf("the longest a boost may run is %s; a need that outlives an evening "+
				"belongs in the group's own file", BoostMaxDuration))
	}

	headroom, err := s.opts.Writer.Headroom(ctx, id.Namespace, req.GetGroup())
	switch {
	case errors.Is(err, ErrNoSuchGroup):
		return refuse(reqID, agentpb.RequestError_NOT_FOUND,
			"no group by that name is on this network")
	case errors.Is(err, ErrGroupNotScalable):
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"that group is sized by its own replica count, so a boost would change nothing")
	case err != nil:
		logger.V(1).Info("could not read a group for a boost request", "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE,
			"the operator could not read that group just now")
	}
	if room := headroom.Room(); req.GetReplicas() > room {
		return refuse(reqID, agentpb.RequestError_REFUSED,
			fmt.Sprintf("that group has room for %d more, not %d", room, req.GetReplicas()))
	}

	expiresAt := s.opts.Clock().Add(duration)
	if err := s.opts.Writer.Boost(ctx, id.Namespace, req.GetGroup(), req.GetReplicas(), expiresAt); err != nil {
		if errors.Is(err, ErrNoSuchGroup) {
			// Deleted between the two calls. Ordinary.
			return refuse(reqID, agentpb.RequestError_NOT_FOUND,
				"no group by that name is on this network")
		}
		logger.V(1).Info("could not create a boost", "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE,
			"the operator could not write that just now")
	}

	return &agentpb.CloudResponse{
		Id: reqID,
		Result: &agentpb.CloudResponse_Boost{Boost: &agentpb.BoostResult{
			Replicas:      req.GetReplicas(),
			ExpiresAtUnix: expiresAt.Unix(),
		}},
	}
}

// answerStopBoost ends a group's boosts early.
//
// It refuses nothing but a read it could not do. A group with no boosts
// answers zero, which is what an admin who expected some needs to hear -- and
// a group that does not exist answers zero as well, because there is nothing
// to distinguish and nothing that a person would do differently. That last
// choice is the one worth stating: an admin who mistypes a group name is told
// "no boosts", not "no such group", and the two readings agree on the only
// thing that matters, which is that nothing was removed.
func (s *Server) answerStopBoost(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	reqID uint64,
	req *agentpb.StopBoostRequest,
) *agentpb.CloudResponse {
	removed, err := s.opts.Writer.StopBoosts(ctx, id.Namespace, req.GetGroup())
	if err != nil {
		logger.V(1).Info("could not stop boosts", "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE,
			"the operator could not write that just now")
	}
	return &agentpb.CloudResponse{
		Id: reqID,
		Result: &agentpb.CloudResponse_StopBoost{
			StopBoost: &agentpb.StopBoostResult{Removed: int32(removed)},
		},
	}
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
// The remaining checks each have their own test, because a single test
// asserting "it was refused" passes when the wrong one fired. The rate bound
// is not among them: it lives in answerCloudRequest, so that every verb has it
// whether or not its author remembered.
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
// answerAnnounce records what a server says about itself.
//
// # The one request the operator stores and never reads
//
// Every other verb here changes something the operator then acts on. This one
// changes only what the operator repeats: the announcement is carried into the
// NetworkState every agent in the namespace receives, and no rule in this
// repository branches on it. That is what makes free-form text acceptable
// here and unacceptable anywhere else in this file.
//
// **The server names itself, and the message does not.** The name stored is
// id.PodName, from the pod's own authenticated token; a pod is named after its
// Server, so the identity already carries the name and there is no field an
// agent could put another server's name in. An AnnounceRequest that carried a
// name would be the first message on this channel that could describe
// somebody else.
//
// **A proxy is refused rather than accepted and dropped.** A network's picture
// has a record per server and none per proxy, so an announcement from a proxy
// would be stored where nothing could read it. Storing it silently would leave
// a plugin author watching for a description that was never going to appear,
// with nothing anywhere saying why.
//
// **Too big is refused and never trimmed**, and each bound says which one it
// was. A description silently cut in half is worse than a refusal twice over:
// the plugin believes it published what it wrote, and the truncation lands
// wherever the cut happened to fall rather than where a reader could see it.
func (s *Server) answerAnnounce(
	logger logr.Logger,
	id grpcauth.Identity,
	reqID uint64,
	req *agentpb.AnnounceRequest,
) *agentpb.CloudResponse {
	if id.Role != agent.RoleServer {
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"only a server can describe itself: a network's picture has a record per server and none per proxy")
	}
	if message, ok := announcementRefusal(req); !ok {
		return refuse(reqID, agentpb.RequestError_REFUSED, message)
	}

	// The namespace and the name are the token's; only the words are the
	// message's.
	if err := s.opts.Agents.ReportAnnouncement(id.PodUID, id.Namespace, id.PodName,
		agent.Announcement{State: req.GetState(), Attributes: req.GetAttributes()}); err != nil {
		// The stream this arrived on was superseded between the read and here,
		// which is ordinary during a renewal and is the caller's to retry.
		logger.V(1).Info("could not record an announcement", "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE,
			"the operator could not record that just now")
	}

	return &agentpb.CloudResponse{
		Id:     reqID,
		Result: &agentpb.CloudResponse_Announce{Announce: &agentpb.AnnounceResult{}},
	}
}

// announcementRefusal reports whether an announcement is within its bounds,
// and says which one it broke when it is not.
//
// The message names the bound and the offending key, because the caller is a
// plugin author reading a log line and "too many attributes" without the
// number is a bound they then have to find in this file.
func announcementRefusal(req *agentpb.AnnounceRequest) (string, bool) {
	if len(req.GetState()) > AnnounceMaxStateLength {
		return fmt.Sprintf("that state is %d characters and the operator carries at most %d",
			len(req.GetState()), AnnounceMaxStateLength), false
	}
	if len(req.GetAttributes()) > AnnounceMaxAttributes {
		return fmt.Sprintf("that announcement has %d attributes and the operator carries at most %d",
			len(req.GetAttributes()), AnnounceMaxAttributes), false
	}
	for key, value := range req.GetAttributes() {
		if key == "" {
			return "an attribute with no name is one nothing can ask for", false
		}
		if len(key) > AnnounceMaxKeyLength {
			return fmt.Sprintf("an attribute name is %d characters and the operator carries at most %d",
				len(key), AnnounceMaxKeyLength), false
		}
		if len(value) > AnnounceMaxValueLength {
			return fmt.Sprintf("the value of %q is %d characters and the operator carries at most %d",
				key, len(value), AnnounceMaxValueLength), false
		}
	}
	return "", true
}

func refuse(reqID uint64, reason agentpb.RequestError_Reason, message string) *agentpb.CloudResponse {
	RequestsRefused.WithLabelValues(reason.String()).Inc()
	return &agentpb.CloudResponse{
		Id: reqID,
		Result: &agentpb.CloudResponse_Error{
			Error: &agentpb.RequestError{Reason: reason, Message: message},
		},
	}
}

// retired wraps a successful retire answer.
func retired(reqID uint64, result *agentpb.RetireResult) *agentpb.CloudResponse {
	return &agentpb.CloudResponse{
		Id:     reqID,
		Result: &agentpb.CloudResponse_Retire{Retire: result},
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
