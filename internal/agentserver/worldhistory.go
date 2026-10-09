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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/grpcauth"
	"github.com/spawnery/spawnery/internal/instance"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/worldsync"
)

var (
	ErrNotObjectStore    = errors.New("that group keeps its worlds on claims, which keep no history")
	ErrMemberRunning     = errors.New("that member is running")
	ErrNoWorld           = errors.New("that key has no world")
	ErrNoGeneration      = errors.New("that world keeps no such generation")
	ErrCurrentGeneration = errors.New("that generation is the world's current one")
	ErrWorldBusy         = errors.New("that world is being written")
)

// WorldHistory is the operator's hold on the older generations of the
// worlds in a bucket. Worlds are named "<namespace>/<group>/<key>".
type WorldHistory interface {
	RestorePoints(ctx context.Context, world string) ([]worldsync.RestorePoint, error)
	Restore(ctx context.Context, world string, generation int64, policy worldsync.Retention) (worldsync.Restored, error)
}

// answersOffTheLoop is true for the requests that wait on the object store,
// which would hold a session's loop for as long.
func answersOffTheLoop(req *agentpb.CloudRequest) bool {
	return req.GetListRestorePoints() != nil || req.GetRestoreWorld() != nil
}

// restoreTimeout ends a restore long before the lease it holds goes stale. The
// restore runs detached from the plugin's request: once the manifest is
// committed, a dropped plugin must not turn it into a failure.
const restoreTimeout = time.Minute

func (w KubeWriter) historyGroup(ctx context.Context, namespace, group, key string) (*spawneryv1alpha1.ServerGroup, string, error) {
	name, err := instance.Name(group, key)
	if err != nil {
		return nil, "", err
	}
	var g spawneryv1alpha1.ServerGroup
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: group}, &g); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "", ErrNoSuchGroup
		}
		return nil, "", err
	}
	if !g.IsOnDemand() {
		return nil, "", ErrGroupNotOnDemand
	}
	if !g.UsesObjectStore() {
		return nil, "", ErrNotObjectStore
	}
	if w.Worlds == nil || w.History == nil {
		return nil, "", ErrWorldSyncOff
	}
	return &g, name, nil
}

func (w KubeWriter) ListRestorePoints(ctx context.Context, namespace, group, key string) ([]worldsync.RestorePoint, error) {
	if _, _, err := w.historyGroup(ctx, namespace, group, key); err != nil {
		return nil, err
	}
	points, err := w.History.RestorePoints(ctx, namespace+"/"+group+"/"+key)
	if errors.Is(err, worldsync.ErrNotFound) {
		return nil, ErrNoWorld
	}
	return points, err
}

func (w KubeWriter) RestoreWorld(ctx context.Context, namespace, group, key string, generation int64) (worldsync.Restored, error) {
	g, name, err := w.historyGroup(ctx, namespace, group, key)
	if err != nil {
		return worldsync.Restored{}, err
	}
	var srv spawneryv1alpha1.Server
	err = w.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &srv)
	switch {
	case err == nil && srv.Spec.GroupRef.Name == group && srv.Spec.Key == key:
		if !srv.DeletionTimestamp.IsZero() {
			return worldsync.Restored{}, ErrInstanceStopping
		}
		if !phase.Terminal(phase.Phase(srv.Status.Phase)) {
			return worldsync.Restored{}, ErrMemberRunning
		}
	case err != nil && !apierrors.IsNotFound(err):
		return worldsync.Restored{}, err
	}
	world := namespace + "/" + group + "/" + key
	pending, err := w.Worlds.DeletionPending(ctx, world)
	if err != nil {
		return worldsync.Restored{}, err
	}
	if pending {
		return worldsync.Restored{}, ErrWorldDeleting
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), restoreTimeout)
	defer cancel()
	restored, err := w.History.Restore(rctx, world, generation, worldsync.Retention(g.WorldRetention()))
	var held *worldsync.HeldError
	switch {
	case errors.As(err, &held), errors.Is(err, worldsync.ErrConflict):
		return worldsync.Restored{}, ErrWorldBusy
	case errors.Is(err, worldsync.ErrNotFound):
		return worldsync.Restored{}, ErrNoWorld
	case errors.Is(err, worldsync.ErrNoGeneration):
		return worldsync.Restored{}, ErrNoGeneration
	case errors.Is(err, worldsync.ErrCurrentGeneration):
		return worldsync.Restored{}, ErrCurrentGeneration
	}
	return restored, err
}

func historyRefusal(err error) (agentpb.RequestError_Reason, string, bool) {
	switch {
	case errors.Is(err, ErrNoSuchGroup):
		return agentpb.RequestError_NOT_FOUND, "no group by that name is on this network", true
	case errors.Is(err, ErrNoWorld):
		return agentpb.RequestError_NOT_FOUND, "that key has no world in the object store", true
	case errors.Is(err, ErrNoGeneration):
		return agentpb.RequestError_NOT_FOUND, "that world keeps no generation of that number", true
	case errors.Is(err, ErrGroupNotOnDemand):
		return agentpb.RequestError_REFUSED, "that group is not on-demand, so its members have no world of their own", true
	case errors.Is(err, ErrNotObjectStore):
		return agentpb.RequestError_REFUSED, "that group keeps its worlds on claims, which keep no history", true
	case errors.Is(err, instance.ErrBadKey):
		return agentpb.RequestError_REFUSED, err.Error(), true
	case errors.Is(err, ErrMemberRunning):
		return agentpb.RequestError_REFUSED, "that member is running; a world goes back only while its server is stopped", true
	case errors.Is(err, ErrWorldDeleting):
		return agentpb.RequestError_REFUSED, "that world is being deleted", true
	case errors.Is(err, ErrCurrentGeneration):
		return agentpb.RequestError_REFUSED, "that generation is already the world's current one", true
	case errors.Is(err, ErrWorldSyncOff):
		return agentpb.RequestError_REFUSED, "that group keeps its worlds in an object store, and this operator runs without --world-sync", true
	case errors.Is(err, ErrInstanceStopping):
		return agentpb.RequestError_UNAVAILABLE, "that member is still stopping; ask again once it is gone", true
	case errors.Is(err, ErrWorldBusy):
		return agentpb.RequestError_UNAVAILABLE, "that world is being written, by the upload after a stop or by another restore; ask again in a few seconds", true
	}
	return agentpb.RequestError_REASON_UNSPECIFIED, "", false
}

func (s *Server) answerListRestorePoints(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	reqID uint64,
	req *agentpb.ListRestorePointsRequest,
) *agentpb.CloudResponse {
	points, err := s.opts.Writer.ListRestorePoints(ctx, id.Namespace, req.GetGroup(), req.GetKey())
	if err != nil {
		if reason, message, known := historyRefusal(err); known {
			return refuse(reqID, reason, message)
		}
		logger.V(1).Info("could not list restore points", "group", req.GetGroup(), "key", req.GetKey(), "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE, "the operator could not read that world's history just now")
	}
	out := make([]*agentpb.RestorePoint, 0, len(points))
	for _, p := range points {
		out = append(out, &agentpb.RestorePoint{Generation: p.Generation, TakenUnixMillis: p.Taken.UnixMilli(), Current: p.Current})
	}
	return &agentpb.CloudResponse{
		Id:     reqID,
		Result: &agentpb.CloudResponse_ListRestorePoints{ListRestorePoints: &agentpb.ListRestorePointsResult{Points: out}},
	}
}

func (s *Server) answerRestoreWorld(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	reqID uint64,
	req *agentpb.RestoreWorldRequest,
) *agentpb.CloudResponse {
	restored, err := s.opts.Writer.RestoreWorld(ctx, id.Namespace, req.GetGroup(), req.GetKey(), req.GetGeneration())
	if err != nil {
		reason, message, known := historyRefusal(err)
		switch {
		case !known:
			WorldRestores.WithLabelValues("failed").Inc()
			logger.Info("could not restore a world", "group", req.GetGroup(), "key", req.GetKey(),
				"generation", req.GetGeneration(), "reason", err.Error())
			return refuse(reqID, agentpb.RequestError_UNAVAILABLE, "the operator could not restore that world just now")
		case reason == agentpb.RequestError_UNAVAILABLE:
			WorldRestores.WithLabelValues("unavailable").Inc()
		default:
			WorldRestores.WithLabelValues("refused").Inc()
		}
		return refuse(reqID, reason, message)
	}
	WorldRestores.WithLabelValues("restored").Inc()
	logger.Info("restored a world", "group", req.GetGroup(), "key", req.GetKey(),
		"generation", restored.Generation, "restoredFrom", restored.RestoredFrom, "pod", id.PodName)
	if restored.PruneErr != nil {
		logger.Error(restored.PruneErr, "the prune after a restore failed; the world's next prune catches up",
			"group", req.GetGroup(), "key", req.GetKey())
	}
	s.recordOnGroup(ctx, id.Namespace, req.GetGroup(), corev1.EventTypeNormal, "WorldRestored", "RestoreWorld",
		fmt.Sprintf("world %s restored to generation %d of %s",
			req.GetKey(), restored.RestoredFrom, restored.RestoredTaken.UTC().Format(time.RFC3339)))
	return &agentpb.CloudResponse{
		Id: reqID,
		Result: &agentpb.CloudResponse_RestoreWorld{RestoreWorld: &agentpb.RestoreWorldResult{
			Generation: restored.Generation, RestoredFrom: restored.RestoredFrom,
		}},
	}
}

// recordOnGroup reports nothing: a lost event must not fail a restore that
// was carried out.
func (s *Server) recordOnGroup(ctx context.Context, namespace, group, eventType, reason, action, note string) {
	if s.opts.Recorder == nil {
		return
	}
	var g spawneryv1alpha1.ServerGroup
	if err := s.opts.State.Reader.Get(context.WithoutCancel(ctx), client.ObjectKey{Namespace: namespace, Name: group}, &g); err != nil {
		return
	}
	s.opts.Recorder.Eventf(&g, nil, eventType, reason, action, "%s", note)
}
