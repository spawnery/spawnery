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

package agentserver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/grpcauth"
	"github.com/spawnery/spawnery/internal/instance"
	"github.com/spawnery/spawnery/internal/netstate"
	"github.com/spawnery/spawnery/internal/netstatus"
)

const (
	// RequestBurst is how many requests one pod may make back to back. It
	// bounds the work a compromised pod can cause, not a busy plugin.
	RequestBurst = 8
	// RequestRefill is how long one token takes to come back.
	RequestRefill = time.Second

	// BoostDefaultDuration is how long a boost runs when the request names no
	// duration; a boost is meant for an event, not to linger for weeks.
	BoostDefaultDuration = time.Hour
	// BoostMaxDuration covers one evening. A longer need belongs in the
	// group's own file.
	BoostMaxDuration = 12 * time.Hour

	// AnnounceMaxStateLength keeps the state a word to compare, not a message.
	AnnounceMaxStateLength = 64
	AnnounceMaxAttributes  = 16
	AnnounceMaxKeyLength   = 64
	// AnnounceMaxValueLength: with the bounds above, one announcement is at
	// most about 5 KB, and it is resent to every agent in the namespace on
	// every resync.
	AnnounceMaxValueLength = 256
)

const requestMaxBuckets = 4096

// requestLimiter is a token bucket per pod. grpcauth.PeerLimiter is not reused
// because it is keyed by peer address and budgets TokenReview misses, a
// different question.
type requestLimiter struct {
	now func() time.Time
	// maxBuckets is when fully refilled buckets are swept, not a hard cap:
	// refusing a legitimate pod to make room is what a limiter must not do.
	maxBuckets int

	mu      sync.Mutex
	buckets map[string]struct {
		tokens float64
		last   time.Time
	}
}

func newRequestLimiter(now func() time.Time) *requestLimiter {
	return &requestLimiter{
		now:        now,
		maxBuckets: requestMaxBuckets,
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
	if !seen && len(l.buckets) >= l.maxBuckets {
		for key, other := range l.buckets {
			// Refilled by now, not as stored: a bucket only refills when its
			// pod next asks, and a pod that is gone never asks again.
			if other.tokens+now.Sub(other.last).Seconds()/RequestRefill.Seconds() >= RequestBurst {
				delete(l.buckets, key)
			}
		}
	}
	l.buckets[pod] = b
	return true
}

// answerCloudRequest routes one request to the verb that answers it, for both
// server and proxy sessions.
//
// An unknown request is refused rather than ignored: an agent waiting on an id
// it will never hear about holds its caller until the deadline.
func (s *Server) answerCloudRequest(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	req *agentpb.CloudRequest,
) *agentpb.CloudResponse {
	// Here rather than per verb, so a new verb cannot forget it. Unknown
	// requests spend a token too.
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
	case req.GetAcceptJoins() != nil:
		return s.answerAcceptJoins(ctx, logger, id, req.GetId(), req.GetAcceptJoins())
	case req.GetStartServer() != nil:
		return s.answerStartServer(ctx, logger, id, req.GetId(), req.GetStartServer())
	case req.GetStopServer() != nil:
		return s.answerStopServer(ctx, logger, id, req.GetId(), req.GetStopServer())
	case req.GetDeleteServer() != nil:
		return s.answerDeleteServer(ctx, logger, id, req.GetId(), req.GetDeleteServer())
	case req.GetUnretire() != nil:
		return s.answerUnretire(ctx, logger, id, req.GetId(), req.GetUnretire())
	case req.GetStatus() != nil:
		return s.answerStatus(ctx, logger, id, req.GetId(), req.GetStatus())
	case req.GetScale() != nil:
		return s.answerScale(ctx, logger, id, req.GetId(), req.GetScale())
	case req.GetForceStop() != nil:
		return s.answerForceStop(ctx, logger, id, req.GetId(), req.GetForceStop())
	case req.GetExecute() != nil:
		return s.answerExecute(ctx, logger, id, req.GetId(), req.GetExecute())
	case req.GetListRestorePoints() != nil:
		return s.answerListRestorePoints(ctx, logger, id, req.GetId(), req.GetListRestorePoints())
	case req.GetRestoreWorld() != nil:
		return s.answerRestoreWorld(ctx, logger, id, req.GetId(), req.GetRestoreWorld())
	default:
		return refuse(req.GetId(), agentpb.RequestError_REASON_UNSPECIFIED,
			"this operator does not know that request")
	}
}

// answerRetire asks one server to stop taking joins. The namespace bound is
// structural: the server is looked up under id.Namespace, from the pod's
// token, and the request has no namespace field.
//
// An already-retiring server is refused although the patch is idempotent, so
// a second admin is not told "done" as if the first attempt had done nothing.
func (s *Server) answerRetire(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	reqID uint64,
	req *agentpb.RetireRequest,
) *agentpb.CloudResponse {
	applied, err := s.opts.Writer.Retire(ctx, id.Namespace, req.GetServer())
	switch {
	case errors.Is(err, ErrNoSuchServer):
		return refuse(reqID, agentpb.RequestError_NOT_FOUND,
			"no server or proxy by that name is on this network")
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

// answerBoost adds capacity to a group for a while, resolved under
// id.Namespace. A boost beyond the group's ceiling is refused rather than
// capped: a command typed in chat must not lift a ceiling, and an admin who
// asked for six and silently got three would not notice.
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

	// A duration on the wire, because agent and operator do not share a clock.
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

// answerStopBoost ends a group's boosts early. A group that does not exist
// answers zero removed, not NOT_FOUND: either way nothing was removed.
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
// it. Everything resolves under id.Namespace, from the pod's token; the
// request carries no namespace.
//
// It resolves against the asking pod's own picture, so a backend cannot send
// a player to a private server it is not shown. That is refused as REFUSED
// rather than NOT_FOUND, which would claim a running server does not exist.
//
// A group target naming an on-demand group is refused for both session kinds,
// before resolution: the operator would pick whichever stranger's world has
// room, and a proxy is shown the members.
//
// Ordered means the instruction reached this namespace's proxies, not that the
// player arrived; see agentpb.ConnectResult.
func (s *Server) answerConnect(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	reqID uint64,
	req *agentpb.ConnectRequest,
) *agentpb.CloudResponse {
	state, err := s.opts.State.Build(ctx, id.Namespace, netstate.AudienceOf(id.Role))
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
		return refuse(reqID, agentpb.RequestError_NOT_FOUND,
			"no player with that id is on this network")
	}

	if s.namesAnOnDemandGroup(ctx, id.Namespace, req) {
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"the members of an on-demand group are addressed by name, because each one belongs to somebody")
	}

	target, ok := resolveTarget(state, req)
	if !ok {
		if id.Role != agent.RoleProxy && s.namesAPrivateServer(ctx, id.Namespace, req) {
			return refuse(reqID, agentpb.RequestError_REFUSED,
				"a private server is addressed through a proxy, not from a backend, and only once it is running")
		}
		return refuse(reqID, agentpb.RequestError_NOT_FOUND,
			"no server or group by that name is on this network")
	}

	if player.GetServer() == target {
		return connected(reqID, &agentpb.ConnectResult{AlreadyThere: true, Target: target})
	}

	s.opts.Proxies.Move(id.Namespace, req.GetPlayerUuid(), target)
	return connected(reqID, &agentpb.ConnectResult{Ordered: true, Target: target})
}

// resolveTarget turns a request's target into one registered server name; for
// a group, the registered member with the most free playable slots.
func resolveTarget(state *agentpb.NetworkState, req *agentpb.ConnectRequest) (string, bool) {
	switch {
	case req.GetServer() != "":
		for _, srv := range state.GetServers() {
			if srv.GetName() == req.GetServer() && srv.GetRegistered() {
				return srv.GetName(), true
			}
		}
	case req.GetGroup() != "":
		best, bestFree, bestRoom := "", -1, -1
		for _, srv := range state.GetServers() {
			if srv.GetGroup() != req.GetGroup() || !srv.GetRegistered() {
				continue
			}
			free, room := playableFree(srv), int(srv.GetSlots()-srv.GetPlayers())
			if free > bestFree || (free == bestFree && room > bestRoom) {
				best, bestFree, bestRoom = srv.GetName(), free, room
			}
		}
		if best != "" {
			return best, true
		}
	}
	return "", false
}

func playableFree(srv *agentpb.ServerState) int {
	playable := srv.GetPlayableSlots()
	if playable <= 0 || playable > srv.GetSlots() {
		playable = srv.GetSlots()
	}
	return max(0, int(playable-srv.GetPlayers()))
}

// namesAPrivateServer reports whether the server a request names is a member
// of an on-demand group. It is a deliberate hole in a backend's picture, only
// to word a refusal: one name the caller supplied, one bit back, nothing
// resolved from it.
func (s *Server) namesAPrivateServer(ctx context.Context, namespace string, req *agentpb.ConnectRequest) bool {
	name := req.GetServer()
	if name == "" {
		return false
	}
	var srv spawneryv1alpha1.Server
	if err := s.opts.State.Reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &srv); err != nil {
		return false
	}
	return netstate.IsPrivateServer(&srv)
}

// namesAnOnDemandGroup is the same narrow hole as namesAPrivateServer, for
// groups.
func (s *Server) namesAnOnDemandGroup(ctx context.Context, namespace string, req *agentpb.ConnectRequest) bool {
	name := req.GetGroup()
	if name == "" {
		return false
	}
	var group spawneryv1alpha1.ServerGroup
	if err := s.opts.State.Reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &group); err != nil {
		return false
	}
	return group.IsOnDemand()
}

// answerAcceptJoins opens or closes the asking server's own door; the request
// has no field naming another server. Unlike retire it is reversible and the
// phase does not move: a closed server is Ready and not registered.
func (s *Server) answerAcceptJoins(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	reqID uint64,
	req *agentpb.AcceptJoinsRequest,
) *agentpb.CloudResponse {
	if id.Role != agent.RoleServer {
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"only a server has a door to close: a proxy is not in a routing table, it is the routing table")
	}
	changed, err := s.opts.Agents.ReportAcceptJoins(id.PodUID, id.Namespace,
		req.GetAccept(), req.GetRoundEnded())
	if err != nil {
		logger.V(1).Info("could not record a join preference", "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE,
			"the operator could not record that just now")
	}
	if changed {
		s.opts.Proxies.SendState(ctx, id.Namespace)
	}

	return &agentpb.CloudResponse{
		Id:     reqID,
		Result: &agentpb.CloudResponse_AcceptJoins{AcceptJoins: &agentpb.AcceptJoinsResult{}},
	}
}

// answerAnnounce records what a server says about itself. The operator only
// repeats it in the NetworkState and never branches on it, which is why
// free-form text is acceptable here. The name stored is id.PodName, from the
// token; the request carries none. Too big is refused, never trimmed.
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

	if err := s.opts.Agents.ReportAnnouncement(id.PodUID, id.Namespace, id.PodName,
		agent.Announcement{State: req.GetState(), Attributes: req.GetAttributes()}); err != nil {
		// The stream was superseded, ordinary during a renewal.
		logger.V(1).Info("could not record an announcement", "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE,
			"the operator could not record that just now")
	}

	return &agentpb.CloudResponse{
		Id:     reqID,
		Result: &agentpb.CloudResponse_Announce{Announce: &agentpb.AnnounceResult{}},
	}
}

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

func (s *Server) answerUnretire(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	reqID uint64,
	req *agentpb.UnretireRequest,
) *agentpb.CloudResponse {
	err := s.opts.Writer.Unretire(ctx, id.Namespace, req.GetServer())
	switch {
	case errors.Is(err, ErrNoSuchServer):
		return refuse(reqID, agentpb.RequestError_NOT_FOUND,
			"no server by that name is on this network")
	case errors.Is(err, ErrServerStopping):
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"that server is already stopping")
	case errors.Is(err, ErrNotRetiring):
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"that server is not retiring")
	case err != nil:
		logger.V(1).Info("could not unretire a server", "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE,
			"the operator could not write that just now")
	}
	return &agentpb.CloudResponse{
		Id:     reqID,
		Result: &agentpb.CloudResponse_Unretire{Unretire: &agentpb.UnretireResult{Server: req.GetServer()}},
	}
}

func (s *Server) answerStatus(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	reqID uint64,
	req *agentpb.StatusRequest,
) *agentpb.CloudResponse {
	if s.opts.Status == nil {
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE, "this operator cannot report status")
	}
	res, err := s.opts.Status.Status(ctx, id.Namespace, netstate.AudienceOf(id.Role), req.GetTarget())
	switch {
	case errors.Is(err, netstatus.ErrUnknownTarget):
		return refuse(reqID, agentpb.RequestError_NOT_FOUND,
			"no group, server or proxy by that name is on this network")
	case err != nil:
		logger.V(1).Info("could not build a status answer", "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE,
			"the operator could not read the network just now")
	}
	return &agentpb.CloudResponse{Id: reqID, Result: &agentpb.CloudResponse_Status{Status: res}}
}

func retired(reqID uint64, result *agentpb.RetireResult) *agentpb.CloudResponse {
	return &agentpb.CloudResponse{
		Id:     reqID,
		Result: &agentpb.CloudResponse_Retire{Retire: result},
	}
}

func connected(reqID uint64, result *agentpb.ConnectResult) *agentpb.CloudResponse {
	return &agentpb.CloudResponse{
		Id:     reqID,
		Result: &agentpb.CloudResponse_Connect{Connect: result},
	}
}

func startedServer(reqID uint64, result *agentpb.StartServerResult) *agentpb.CloudResponse {
	return &agentpb.CloudResponse{
		Id:     reqID,
		Result: &agentpb.CloudResponse_StartServer{StartServer: result},
	}
}

func stoppedServer(reqID uint64, result *agentpb.StopServerResult) *agentpb.CloudResponse {
	return &agentpb.CloudResponse{
		Id:     reqID,
		Result: &agentpb.CloudResponse_StopServer{StopServer: result},
	}
}

// answerStartServer creates the member of an on-demand group that carries a
// key, resolved under id.Namespace.
//
// Unlike the admin verbs, a key that is already running is answered, not
// refused: the caller is a plugin, and a player may press the button twice.
// Waiting on a deletion already under way is UNAVAILABLE, so the caller can
// simply ask again.
func (s *Server) answerStartServer(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	reqID uint64,
	req *agentpb.StartServerRequest,
) *agentpb.CloudResponse {
	member, err := s.opts.Writer.StartServer(ctx, id.Namespace, req.GetGroup(), req.GetKey())
	switch {
	case errors.Is(err, ErrNoSuchGroup):
		return refuse(reqID, agentpb.RequestError_NOT_FOUND,
			"no group by that name is on this network")
	case errors.Is(err, ErrGroupNotOnDemand):
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"that group's servers are counted rather than named, so it has no member to ask for")
	case errors.Is(err, ErrTooManyInstances):
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"that group is at spec.maxInstances")
	case errors.Is(err, ErrNoCeiling):
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"that group has no spec.maxInstances, so nothing bounds how many members it could have; "+
				"an admin has to set one before it can start any")
	case errors.Is(err, ErrNameTaken):
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"a server of another group already has the name that group and key compose; "+
				"another key, or group names that cannot run together, is what fixes it")
	case errors.Is(err, ErrInstancesDraining):
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE,
			"that group is at spec.maxInstances right now and one of its members is stopping; "+
				"the same request fits once that one is gone")
	case errors.Is(err, instance.ErrBadKey):
		return refuse(reqID, agentpb.RequestError_REFUSED, err.Error())
	case errors.Is(err, ErrInstanceStopping):
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE,
			"that member is still stopping; the same request starts a fresh one once it is gone")
	case errors.Is(err, ErrWorldDeleting):
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE,
			"that member's world is still being deleted; the same request starts a fresh one once it is gone")
	case errors.Is(err, ErrWorldSyncOff):
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"that group keeps its worlds in an object store, and this operator runs without --world-sync")
	case err != nil:
		logger.V(1).Info("could not start an on-demand server", "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE,
			"the operator could not write that just now")
	}

	return startedServer(reqID, &agentpb.StartServerResult{
		Server:         member.Name,
		AlreadyRunning: member.AlreadyRunning,
	})
}

// answerStopServer deletes one on-demand member, and refuses any other server
// so a mistyped name cannot take a lobby down.
func (s *Server) answerStopServer(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	reqID uint64,
	req *agentpb.StopServerRequest,
) *agentpb.CloudResponse {
	err := s.opts.Writer.StopServer(ctx, id.Namespace, req.GetServer())
	switch {
	case errors.Is(err, ErrNoSuchServer):
		return refuse(reqID, agentpb.RequestError_NOT_FOUND,
			"no server by that name is on this network")
	case errors.Is(err, ErrNotAnInstance):
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"that server is not a member of an on-demand group")
	case err != nil:
		logger.V(1).Info("could not stop an on-demand server", "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE,
			"the operator could not write that just now")
	}

	return stoppedServer(reqID, &agentpb.StopServerResult{Server: req.GetServer()})
}

func deletedServer(reqID uint64, result *agentpb.DeleteServerResult) *agentpb.CloudResponse {
	return &agentpb.CloudResponse{
		Id:     reqID,
		Result: &agentpb.CloudResponse_DeleteServer{DeleteServer: result},
	}
}

// answerDeleteServer deletes a member and its world. The OnDemand bound and
// the claim's labels, both in the writer, are what keep it to that world.
func (s *Server) answerDeleteServer(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	reqID uint64,
	req *agentpb.DeleteServerRequest,
) *agentpb.CloudResponse {
	deleted, err := s.opts.Writer.DeleteServer(ctx, id.Namespace, req.GetGroup(), req.GetKey())
	switch {
	case errors.Is(err, ErrNoSuchGroup):
		return refuse(reqID, agentpb.RequestError_NOT_FOUND,
			"no group by that name is on this network")
	case errors.Is(err, ErrGroupNotOnDemand):
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"that group is not on-demand, so it has no member to delete")
	case errors.Is(err, ErrWorldSyncOff):
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"that group keeps its worlds in an object store, and this operator runs without --world-sync")
	case errors.Is(err, instance.ErrBadKey):
		return refuse(reqID, agentpb.RequestError_REFUSED, err.Error())
	case errors.Is(err, ErrForeignClaim):
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"a claim of that name exists, but this operator did not make it for that group")
	case errors.Is(err, ErrUnkeyedWorld):
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"that world was made before worlds carried their key, so only an admin can delete it")
	case errors.Is(err, ErrNoSuchServer):
		return refuse(reqID, agentpb.RequestError_NOT_FOUND,
			"that key has neither a server nor a world")
	case err != nil:
		logger.V(1).Info("could not delete an on-demand server", "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE,
			"the operator could not write that just now")
	}
	return deletedServer(reqID, &agentpb.DeleteServerResult{Server: deleted.Name, World: deleted.World})
}
