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
	"sync"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/grpcauth"
	"github.com/spawnery/spawnery/internal/instance"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/worldsync"
)

var (
	// ErrNotKeepable: the caller is not a member of an Ephemeral group whose
	// group keeps worlds in the object store.
	ErrNotKeepable = errors.New("that server's group does not keep worlds")
	// ErrKeySet: the caller's world already has another key.
	ErrKeySet = errors.New("that server's world already has another key")
	// ErrKeyTaken: the key holds a world, or another server carries it.
	ErrKeyTaken = errors.New("that key is taken")
	// ErrBindingFailed: the key is on the Server, the binding is not in the
	// object store yet. The same request finishes it.
	ErrBindingFailed = errors.New("the binding could not be written")
)

// Caller is the server a request came from, as its token says.
type Caller struct {
	Namespace, Pod, PodUID string
}

// keying serialises KeyWorld: two members asking for one key at once must
// not both carry it, and the cache the check reads may not show the other's
// patch yet. The requests are rare.
var keying sync.Mutex

// KeyWorld gives the caller's world a key: spec.worldKey on its Server
// first, then the binding the node agent reads before it takes the world's
// lease. The same key again is answered as a success and writes a missing
// binding; another key is ErrKeySet.
func (w KubeWriter) KeyWorld(ctx context.Context, caller Caller, key string) (string, error) {
	keying.Lock()
	defer keying.Unlock()

	var srv spawneryv1alpha1.Server
	if err := w.claims().Get(ctx, client.ObjectKey{Namespace: caller.Namespace, Name: caller.Pod}, &srv); err != nil {
		if apierrors.IsNotFound(err) {
			return "", ErrNoSuchServer
		}
		return "", err
	}
	if srv.Status.PodUID != "" && srv.Status.PodUID != caller.PodUID {
		return "", ErrNoSuchServer
	}
	var g spawneryv1alpha1.ServerGroup
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: caller.Namespace, Name: srv.Spec.GroupRef.Name}, &g); err != nil {
		if apierrors.IsNotFound(err) {
			return "", ErrNotKeepable
		}
		return "", err
	}
	if !g.IsEphemeral() || !g.UsesObjectStore() {
		return "", ErrNotKeepable
	}
	if w.Worlds == nil {
		return "", ErrWorldSyncOff
	}
	name, err := instance.KeptName(g.Name, key)
	if err != nil {
		return "", err
	}
	world := caller.Namespace + "/" + g.Name + "/" + key
	binding := worldsync.Binding{Server: srv.Name, Pod: caller.Namespace + "/" + caller.Pod}

	switch srv.Spec.WorldKey {
	case key:
		return world, w.bind(ctx, world, binding)
	case "":
	default:
		return "", ErrKeySet
	}
	if !srv.DeletionTimestamp.IsZero() || phase.Terminal(phase.Phase(srv.Status.Phase)) {
		return "", ErrInstanceStopping
	}
	if err := w.worldNotDeleting(ctx, world); err != nil {
		return "", err
	}
	if exists, err := w.Worlds.Exists(ctx, world); err != nil {
		return "", err
	} else if exists {
		return "", ErrKeyTaken
	}
	var all spawneryv1alpha1.ServerList
	if err := w.claims().List(ctx, &all, client.InNamespace(caller.Namespace)); err != nil {
		return "", err
	}
	for i := range all.Items {
		m := &all.Items[i]
		if m.Name == name || (m.Spec.GroupRef.Name == g.Name && m.Spec.WorldKey == key) {
			return "", ErrKeyTaken
		}
	}

	patch := client.MergeFrom(srv.DeepCopy())
	srv.Spec.WorldKey = key
	if err := w.Client.Patch(ctx, &srv, patch); err != nil {
		return "", err
	}
	return world, w.bind(ctx, world, binding)
}

func (w KubeWriter) bind(ctx context.Context, world string, b worldsync.Binding) error {
	err := w.Worlds.Bind(ctx, world, b)
	switch {
	case errors.Is(err, worldsync.ErrBound):
		return ErrKeyTaken
	case err != nil:
		return errors.Join(ErrBindingFailed, err)
	}
	return nil
}

// keptMembers lists the live servers of g that carry key's world: the member
// that keyed it while it still runs, or the one resumed for it.
func (w KubeWriter) keptMembers(ctx context.Context, g *spawneryv1alpha1.ServerGroup, key string) ([]spawneryv1alpha1.Server, *spawneryv1alpha1.ServerList, error) {
	var all spawneryv1alpha1.ServerList
	if err := w.Client.List(ctx, &all, client.InNamespace(g.Namespace)); err != nil {
		return nil, nil, err
	}
	var out []spawneryv1alpha1.Server
	for _, m := range all.Items {
		if m.Spec.GroupRef.Name == g.Name && m.Spec.WorldKey == key {
			out = append(out, m)
		}
	}
	return out, &all, nil
}

// resumeKept starts "<group>-<key>" on the world a member of an Ephemeral
// group kept under key. Only resumed members count against spec.maxInstances;
// the group's own members stay sized by spec.scaling.
//
// A member that keyed the world and still runs counts as running: the answer
// names it, and nothing is created. A key without a world is ErrNoWorld, as
// an Ephemeral group starts no empty world by key. A world whose node died
// keeps its lease until it goes stale, so such a start waits in
// ContainerCreating for up to 10 minutes.
func (w KubeWriter) resumeKept(
	ctx context.Context, g *spawneryv1alpha1.ServerGroup, key string, req *agentpb.StartResources,
) (StartedServer, error) {
	name, err := instance.KeptName(g.Name, key)
	if err != nil {
		return StartedServer{}, err
	}
	world := g.Namespace + "/" + g.Name + "/" + key
	if err := w.worldNotDeleting(ctx, world); err != nil {
		return StartedServer{}, err
	}
	carriers, all, err := w.keptMembers(ctx, g, key)
	if err != nil {
		return StartedServer{}, err
	}
	for i := range carriers {
		m := &carriers[i]
		if !m.DeletionTimestamp.IsZero() {
			return StartedServer{}, ErrInstanceStopping
		}
		if !phase.Terminal(phase.Phase(m.Status.Phase)) {
			return runningMember(m), nil
		}
		if m.Name == name {
			// A terminal run of this key is replaced; its world is in the store.
			if err := w.Client.Delete(ctx, m); err != nil && !apierrors.IsNotFound(err) {
				return StartedServer{}, err
			}
		}
	}
	exists, err := w.Worlds.Exists(ctx, world)
	if err != nil {
		return StartedServer{}, err
	}
	if !exists {
		return StartedServer{}, ErrNoWorld
	}
	live, going := 0, 0
	for i := range all.Items {
		m := &all.Items[i]
		if m.Spec.GroupRef.Name != g.Name || !m.Spec.Resumed || phase.Terminal(phase.Phase(m.Status.Phase)) {
			continue
		}
		live++
		if !m.DeletionTimestamp.IsZero() {
			going++
		}
	}
	if _, _, err := w.resolveStart(ctx, g, req); err != nil {
		return StartedServer{}, err
	}
	if g.Spec.MaxInstances == nil {
		return StartedServer{}, ErrNoCeiling
	}
	if int32(live) >= *g.Spec.MaxInstances {
		if going > 0 {
			return StartedServer{}, ErrInstancesDraining
		}
		return StartedServer{}, ErrTooManyInstances
	}
	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: g.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: spawneryv1alpha1.GroupVersion.String(),
				Kind:       "ServerGroup",
				Name:       g.Name,
				UID:        g.UID,
			}},
		},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: g.Name},
			WorldKey: key,
			Resumed:  true,
		},
	}
	if err := w.Client.Create(ctx, srv); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return w.keptOccupant(ctx, g, name, key)
		}
		return StartedServer{}, err
	}
	return StartedServer{Name: name}, nil
}

func (w KubeWriter) keptOccupant(ctx context.Context, g *spawneryv1alpha1.ServerGroup, name, key string) (StartedServer, error) {
	var existing spawneryv1alpha1.Server
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: g.Namespace, Name: name}, &existing); err != nil {
		if apierrors.IsNotFound(err) {
			return StartedServer{}, ErrInstanceStopping
		}
		return StartedServer{}, err
	}
	if existing.Spec.GroupRef.Name != g.Name || existing.Spec.WorldKey != key {
		return StartedServer{}, ErrNameTaken
	}
	if !existing.DeletionTimestamp.IsZero() || phase.Terminal(phase.Phase(existing.Status.Phase)) {
		return StartedServer{}, ErrInstanceStopping
	}
	return runningMember(&existing), nil
}

// deleteKeptWorld marks the world kept under key deleted and deletes the
// live server that carries it, whether the member that keyed it or the one
// resumed for it.
func (w KubeWriter) deleteKeptWorld(ctx context.Context, g *spawneryv1alpha1.ServerGroup, key string) (DeletedServer, error) {
	name, err := instance.KeptName(g.Name, key)
	if err != nil {
		return DeletedServer{}, err
	}
	carriers, _, err := w.keptMembers(ctx, g, key)
	if err != nil {
		return DeletedServer{}, err
	}
	var carrier *spawneryv1alpha1.Server
	for i := range carriers {
		if carriers[i].DeletionTimestamp.IsZero() {
			carrier = &carriers[i]
			name = carrier.Name
			break
		}
	}
	return w.deleteObjectStoreWorld(ctx, g.Namespace+"/"+g.Name+"/"+key, name, carrier)
}

func keyedWorld(reqID uint64, world string) *agentpb.CloudResponse {
	return &agentpb.CloudResponse{
		Id:     reqID,
		Result: &agentpb.CloudResponse_KeyWorld{KeyWorld: &agentpb.KeyWorldResult{World: world}},
	}
}

// answerKeyWorld keys the calling server's own world. The server is the one
// the token names; the request names only the key.
func (s *Server) answerKeyWorld(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	reqID uint64,
	req *agentpb.KeyWorldRequest,
) *agentpb.CloudResponse {
	if id.Role != agent.RoleServer {
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"only a server has a world to key; a proxy holds none")
	}
	world, err := s.opts.Writer.KeyWorld(ctx, Caller{Namespace: id.Namespace, Pod: id.PodName, PodUID: id.PodUID}, req.GetKey())
	switch {
	case errors.Is(err, ErrNoSuchServer):
		return refuse(reqID, agentpb.RequestError_NOT_FOUND,
			"the operator knows no server for this pod")
	case errors.Is(err, ErrNotKeepable):
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"this server's group is not an Ephemeral group that keeps worlds in the object store")
	case errors.Is(err, ErrWorldSyncOff):
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"that group keeps its worlds in an object store, and this operator runs without --world-sync")
	case errors.Is(err, instance.ErrBadKey):
		return refuse(reqID, agentpb.RequestError_REFUSED, err.Error())
	case errors.Is(err, ErrKeySet):
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"this server's world already has another key; a key is set once")
	case errors.Is(err, ErrKeyTaken):
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"that key already holds a world or belongs to another server")
	case errors.Is(err, ErrInstanceStopping):
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"this server is stopping, and a world keyed now would not be saved")
	case errors.Is(err, ErrWorldDeleting):
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE,
			"a world under that key is still being deleted; ask again once it is gone")
	case errors.Is(err, ErrBindingFailed):
		logger.V(1).Info("could not write a world binding", "key", req.GetKey(), "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE,
			"the key is recorded but the object store did not take the binding; the same request finishes it")
	case err != nil:
		logger.V(1).Info("could not key a world", "key", req.GetKey(), "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE,
			"the operator could not write that just now")
	}
	logger.Info("keyed a world", "world", world, "pod", id.PodName)
	return keyedWorld(reqID, world)
}
