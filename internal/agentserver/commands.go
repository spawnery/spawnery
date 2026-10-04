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
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/grpcauth"
)

const (
	ScaleDefaultDuration = time.Hour
	// ScaleMaxDuration: a pin forgotten at 0 would otherwise keep a group off
	// for weeks without anybody noticing.
	ScaleMaxDuration = 7 * 24 * time.Hour

	issuerMaxLength = 64
)

// answerScale pins an ephemeral group, resolved under id.Namespace. A pin
// above maxReplicas is refused rather than capped, as a boost is.
func (s *Server) answerScale(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	reqID uint64,
	req *agentpb.ScaleRequest,
) *agentpb.CloudResponse {
	if req.GetReplicas() < 0 {
		return refuse(reqID, agentpb.RequestError_REFUSED, "a group cannot be pinned below zero servers")
	}
	seconds := req.GetDurationSeconds()
	if seconds < 0 {
		return refuse(reqID, agentpb.RequestError_REFUSED, "a pin cannot last a negative number of seconds")
	}
	if seconds > int64(ScaleMaxDuration/time.Second) {
		return refuse(reqID, agentpb.RequestError_REFUSED, "the longest a pin may hold is 7 days")
	}
	duration := time.Duration(seconds) * time.Second
	if duration == 0 {
		duration = ScaleDefaultDuration
	}

	headroom, err := s.opts.Writer.Headroom(ctx, id.Namespace, req.GetGroup())
	switch {
	case errors.Is(err, ErrNoSuchGroup):
		return refuse(reqID, agentpb.RequestError_NOT_FOUND, "no group by that name is on this network")
	case errors.Is(err, ErrGroupNotScalable):
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"only an ephemeral group can be pinned: a persistent group is sized by its replica count, "+
				"an on-demand group by requests")
	case err != nil:
		logger.V(1).Info("could not read a group for a scale request", "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE, "the operator could not read that group just now")
	}
	if req.GetReplicas() > headroom.MaxReplicas {
		return refuse(reqID, agentpb.RequestError_REFUSED,
			fmt.Sprintf("that group's maxReplicas is %d; a pin may not lift it", headroom.MaxReplicas))
	}

	if req.GetReplicas() < headroom.MinReplicas && id.Role != agent.RoleProxy {
		return refuse(reqID, agentpb.RequestError_REFUSED,
			fmt.Sprintf("that group's minReplicas is %d; only a proxy may pin below the floor", headroom.MinReplicas))
	}

	expiresAt := s.opts.Clock().Add(duration)
	if err := s.opts.Writer.Pin(ctx, id.Namespace, req.GetGroup(), req.GetReplicas(), expiresAt); err != nil {
		if errors.Is(err, ErrNoSuchGroup) {
			return refuse(reqID, agentpb.RequestError_NOT_FOUND, "no group by that name is on this network")
		}
		logger.V(1).Info("could not create a pin", "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE, "the operator could not write that just now")
	}
	return &agentpb.CloudResponse{
		Id: reqID,
		Result: &agentpb.CloudResponse_Scale{Scale: &agentpb.ScaleResult{
			Replicas:      req.GetReplicas(),
			ExpiresAtUnix: expiresAt.Unix(),
		}},
	}
}

// answerForceStop is proxy-only: it disconnects players without a drain and
// can lose unsaved world data, so one compromised game server must not be able
// to do it to its neighbours.
func (s *Server) answerForceStop(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	reqID uint64,
	req *agentpb.ForceStopRequest,
) *agentpb.CloudResponse {
	if id.Role != agent.RoleProxy {
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"only a proxy may force-stop a server: one compromised game server must not kill its neighbours")
	}
	err := s.opts.Writer.ForceStop(ctx, id.Namespace, req.GetServer())
	switch {
	case errors.Is(err, ErrNoSuchServer):
		return refuse(reqID, agentpb.RequestError_NOT_FOUND, "no server by that name is on this network")
	case errors.Is(err, ErrNotAServer):
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"that is a proxy: /cloud retire drains a proxy, and nothing kills one from chat")
	case err != nil:
		logger.V(1).Info("could not force-stop a server", "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE, "the operator could not write that just now")
	}
	issuer := clip(req.GetIssuer(), issuerMaxLength)
	logger.Info("force-stopping a server", "server", req.GetServer(), "proxy", id.PodName, "issuer", issuer)
	s.recordOn(ctx, id.Namespace, req.GetServer(), corev1.EventTypeWarning, "ForceStopped", "ForceStop",
		fmt.Sprintf("force-stopped by %s on proxy %s", issuer, id.PodName))
	return &agentpb.CloudResponse{
		Id:     reqID,
		Result: &agentpb.CloudResponse_ForceStop{ForceStop: &agentpb.ForceStopResult{Server: req.GetServer()}},
	}
}

// recordOn reports nothing: a lost event must not fail a request that was
// carried out.
func (s *Server) recordOn(ctx context.Context, namespace, server, eventType, reason, action, note string) {
	if s.opts.Recorder == nil {
		return
	}
	var srv spawneryv1alpha1.Server
	if err := s.opts.State.Reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: server}, &srv); err != nil {
		return
	}
	s.opts.Recorder.Eventf(&srv, nil, eventType, reason, action, "%s", note)
}

// clip cuts on a character boundary.
func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
