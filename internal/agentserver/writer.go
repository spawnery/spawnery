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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/boost"
	"github.com/spawnery/spawnery/internal/instance"
	"github.com/spawnery/spawnery/internal/phase"
)

// ErrNoSuchServer is what a ClusterWriter reports for a server that is not
// there.
//
// A sentinel rather than the API machinery's own not-found, so that the
// interface below stays free of Kubernetes: a reader of ClusterWriter should
// be able to see the whole of what a request can do without knowing what
// backs it.
var ErrNoSuchServer = errors.New("no such server")

// ErrNoSuchGroup is the same for a group.
var ErrNoSuchGroup = errors.New("no such group")

// ErrGroupNotScalable is what a boost gets for a group no boost can move.
//
// Only an ephemeral group with a spec.scaling reaches the rule that adds
// boosts to a floor; a persistent group is sized by spec.replicas and a boost
// on one would be created, counted in status.boostedReplicas, and change
// nothing. That is worse than a refusal: the object exists, the status agrees
// it exists, and the group is the size it always was.
var ErrGroupNotScalable = errors.New("that group is not sized by scaling")

// ErrGroupNotOnDemand is what a start gets for a group whose members are
// counted rather than named. Creating a Server in one by hand would make the
// group's own sizing pass condemn it on the next reconcile, which reads from
// outside as a server that started and vanished.
var ErrGroupNotOnDemand = errors.New("that group is not on-demand")

// ErrTooManyInstances is the group's own ceiling, reached.
var ErrTooManyInstances = errors.New("that group is at spec.maxInstances")

// ErrNotAnInstance is a stop aimed at a server that no key names.
var ErrNotAnInstance = errors.New("that server is not an on-demand member")

// ErrInstanceStopping is a start on a key whose member is on its way out.
//
// Distinct from "already running" and from every refusal, because it is the
// one state a caller should wait through rather than act on: the member is
// draining its players and the same request starts a fresh one once it is
// gone. Answering AlreadyRunning here would send the next player to a server
// that is about to stop.
var ErrInstanceStopping = errors.New("that member is still stopping")

// ClusterWriter is every change a plugin's request is allowed to make.
//
// Deliberately one method wide, in the shape ProxyFleet uses and for the same
// reason: this endpoint is the one place in the operator where an instruction
// arrives from inside a game server, and the list of things such an
// instruction can reach should be readable in one screen. A client.Client
// here would instead put every object in the cluster one line away from a
// request handler, and nobody auditing this later could bound it by reading.
type ClusterWriter interface {
	// Retire asks one server to stop taking joins and empty out.
	//
	// It reports whether this call is the one that set the flag. False means
	// somebody had already asked, which the caller answers as REFUSED rather
	// than patching an identical value a second time.
	//
	// It returns ErrNoSuchServer when the server is gone, which is ordinary:
	// the caller resolved the name against a snapshot that is allowed to be a
	// moment stale.
	Retire(ctx context.Context, namespace, name string) (bool, error)

	// Headroom reports how much more capacity a group could be asked for.
	//
	// A read on an interface named for writing, and it earns its place here
	// rather than on the network snapshot: it is the input to a bound, and a
	// bound computed from a picture that is allowed to be a resync stale is a
	// bound that can be wrong in the direction that matters. It returns
	// ErrNoSuchGroup for a group this namespace does not have.
	Headroom(ctx context.Context, namespace, group string) (Headroom, error)

	// Boost creates a ScaleBoost on a group, owned by it.
	//
	// The caller has already bounded the numbers; the only thing this can
	// report is ErrNoSuchGroup, from the same race Retire has.
	Boost(ctx context.Context, namespace, group string, replicas int32, expiresAt time.Time) error

	// StopBoosts deletes every boost on a group and says how many there were.
	//
	// Every one and not the newest: a partial reduction across boosts with
	// different expiries is arithmetic nobody asked for. Zero is an ordinary
	// answer.
	StopBoosts(ctx context.Context, namespace, group string) (int, error)

	// StartServer creates the member of an OnDemand group that carries key.
	//
	// Reports AlreadyRunning for a member that was already there, which is a
	// success: what the caller asked for is the case. It returns
	// ErrNoSuchGroup, ErrGroupNotOnDemand, ErrTooManyInstances,
	// instance.ErrBadKey for a key no name can be built from, and
	// ErrInstanceStopping for a key whose member is still going.
	StartServer(ctx context.Context, namespace, group, key string) (StartedServer, error)

	// StopServer deletes one member of an OnDemand group.
	//
	// It returns ErrNoSuchServer for a name this namespace does not have and
	// ErrNotAnInstance for a server that is not a member of such a group --
	// the refusal that keeps a mistyped name from deleting a lobby.
	StopServer(ctx context.Context, namespace, name string) error
}

// StartedServer is the member a start request produced.
type StartedServer struct {
	Name           string
	AlreadyRunning bool
}

// Headroom is what a group's own spec leaves for a boost to ask for.
type Headroom struct {
	MinReplicas int32
	MaxReplicas int32
	// Boosted is what this group's live boosts already add to the floor.
	Boosted int32
}

// Room is how many more servers a new boost may ask for.
//
// Measured against the *floor* and not against how many servers are running:
// a boost raises what the group tries for, and a group sitting below its floor
// because nodes are full has exactly as much room for a boost as one sitting
// on it. Sizing this by the live count would refuse a boost precisely when
// somebody is asking for capacity because capacity is short.
//
// Never negative. A group whose floor already exceeds its ceiling is
// misconfigured, and answering "minus two" would put that arithmetic into a
// chat line rather than a refusal.
func (h Headroom) Room() int32 {
	room := h.MaxReplicas - h.MinReplicas - h.Boosted
	if room < 0 {
		return 0
	}
	return room
}

// KubeWriter is the ClusterWriter the operator runs with.
type KubeWriter struct {
	// Client must be able to patch servers and create boosts. The manager's
	// client is cached for reads, which is what makes the already-retiring
	// check below cheap.
	Client client.Client
	// Clock decides which boosts are still live. Nil means time.Now.
	Clock func() time.Time
}

func (w KubeWriter) now() time.Time {
	if w.Clock == nil {
		return time.Now()
	}
	return w.Clock()
}

// Retire sets spec.retire on one server.
//
// The read before the patch is not an optimisation. spec.retire is a bool, so
// a blind patch would succeed identically whether or not it changed anything,
// and this verb has to be able to tell the two apart: the operator's answer is
// the only way the person who typed the command learns whether they were the
// one who retired the server or the second person to ask.
//
// The gap between the read and the patch is a race two admins could lose
// together, and losing it costs nothing: the second patch writes the value the
// first one already wrote, and the second admin is told they did something
// they merely repeated. That is a strictly better failure than serialising
// every retire through a conflict-retry loop for a flag that only ever goes
// one way.
func (w KubeWriter) Retire(ctx context.Context, namespace, name string) (bool, error) {
	var srv spawneryv1alpha1.Server
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &srv); err != nil {
		if apierrors.IsNotFound(err) {
			return false, ErrNoSuchServer
		}
		return false, err
	}
	if srv.Spec.Retire {
		return false, nil
	}
	patch := client.MergeFrom(srv.DeepCopy())
	srv.Spec.Retire = true
	if err := w.Client.Patch(ctx, &srv, patch); err != nil {
		return false, err
	}
	return true, nil
}

// Headroom reads the group and its boosts.
//
// Two reads and not one: minReplicas and maxReplicas come from the group's own
// spec, and what is already boosted comes from the boosts themselves. The
// group's status carries a BoostedReplicas figure that would have saved a
// List, and using it would have been wrong -- it is what the last reconcile
// observed, so two people typing the command in the same second would each be
// told there was room for both.
func (w KubeWriter) Headroom(ctx context.Context, namespace, group string) (Headroom, error) {
	var g spawneryv1alpha1.ServerGroup
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: group}, &g); err != nil {
		if apierrors.IsNotFound(err) {
			return Headroom{}, ErrNoSuchGroup
		}
		return Headroom{}, err
	}
	if !g.IsEphemeral() || g.Spec.Scaling == nil {
		return Headroom{}, ErrGroupNotScalable
	}
	var boosts spawneryv1alpha1.ScaleBoostList
	if err := w.Client.List(ctx, &boosts, client.InNamespace(namespace)); err != nil {
		return Headroom{}, err
	}
	return Headroom{
		MinReplicas: g.Spec.Scaling.MinReplicas,
		MaxReplicas: g.Spec.Scaling.MaxReplicas,
		Boosted:     boost.Live(boosts.Items, group, w.now()),
	}, nil
}

// Boost creates the object.
//
// generateName rather than a name built from the group and a timestamp: two
// admins typing at once must both get a boost, and a name either could compute
// would make the second one collide with the first. Boosts add, so two is the
// correct outcome and a collision would silently make it one.
//
// The owner reference is what makes a deleted group take its boosts with it.
// Without it a boost would outlive the group it names, count for nothing, and
// sit in the namespace until somebody wondered what it was.
func (w KubeWriter) Boost(
	ctx context.Context, namespace, group string, replicas int32, expiresAt time.Time,
) error {
	var g spawneryv1alpha1.ServerGroup
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: group}, &g); err != nil {
		if apierrors.IsNotFound(err) {
			return ErrNoSuchGroup
		}
		return err
	}
	at := metav1.NewTime(expiresAt)
	return w.Client.Create(ctx, &spawneryv1alpha1.ScaleBoost{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: group + "-",
			Namespace:    namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: spawneryv1alpha1.GroupVersion.String(),
				Kind:       "ServerGroup",
				Name:       g.Name,
				UID:        g.UID,
			}},
		},
		Spec: spawneryv1alpha1.ScaleBoostSpec{
			GroupRef:  spawneryv1alpha1.ObjectRef{Name: group},
			Replicas:  replicas,
			ExpiresAt: &at,
		},
	})
}

// StopBoosts removes every boost on the group.
//
// Expired ones included, and deliberately: they count for nothing already, so
// deleting them changes no group's size, and leaving them would mean the count
// this reports disagrees with what an admin then sees in `kubectl get
// scaleboosts`. The orphan sweep would have taken them anyway.
func (w KubeWriter) StopBoosts(ctx context.Context, namespace, group string) (int, error) {
	var boosts spawneryv1alpha1.ScaleBoostList
	if err := w.Client.List(ctx, &boosts, client.InNamespace(namespace)); err != nil {
		return 0, err
	}
	removed := 0
	for i := range boosts.Items {
		b := &boosts.Items[i]
		if b.Spec.GroupRef.Name != group {
			continue
		}
		if err := w.Client.Delete(ctx, b); err != nil {
			if apierrors.IsNotFound(err) {
				// The sweep got there first. Not counted, because the admin
				// did not remove it and the number is meant to tell them what
				// their command did.
				continue
			}
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// StartServer creates the member, or reports the one that is already there.
//
// The count that bounds it is of members that are not terminal. A failed
// world is kept for diagnosis and a finished one is swept within a pass, and
// counting either against the ceiling would make a group drift closed as its
// players' servers ended.
//
// The create is what decides the race between two callers asking for the same
// key: AlreadyExists comes back to exactly one of them, and it is the answer
// rather than an error -- which is why nothing here takes a lock and why the
// read above it is an optimisation and not the bound. The one AlreadyExists
// that is not that race is the one after a terminal member was deleted: there
// the object that is in the way is the corpse, held by its own drain
// finalizer, and the caller is told to ask again rather than told their
// server is up.
func (w KubeWriter) StartServer(
	ctx context.Context, namespace, group, key string,
) (StartedServer, error) {
	name, err := instance.Name(group, key)
	if err != nil {
		return StartedServer{}, err
	}

	var g spawneryv1alpha1.ServerGroup
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: group}, &g); err != nil {
		if apierrors.IsNotFound(err) {
			return StartedServer{}, ErrNoSuchGroup
		}
		return StartedServer{}, err
	}
	if !g.IsOnDemand() {
		return StartedServer{}, ErrGroupNotOnDemand
	}

	var members spawneryv1alpha1.ServerList
	if err := w.Client.List(ctx, &members, client.InNamespace(namespace)); err != nil {
		return StartedServer{}, err
	}
	live, replacing := 0, false
	for i := range members.Items {
		m := &members.Items[i]
		if m.Spec.GroupRef.Name != group || m.Spec.Key == "" {
			continue
		}
		if m.Name == name {
			if !m.DeletionTimestamp.IsZero() {
				return StartedServer{}, ErrInstanceStopping
			}
			// A terminal run of this very key is replaced rather than
			// reported: its world is on the claim, the object is a corpse,
			// and refusing here would leave the owner waiting out a
			// retention they cannot see.
			if isTerminal(m.Status.Phase) {
				if err := w.Client.Delete(ctx, m); err != nil && !apierrors.IsNotFound(err) {
					return StartedServer{}, err
				}
				replacing = true
				continue
			}
			return StartedServer{Name: name, AlreadyRunning: true}, nil
		}
		if !isTerminal(m.Status.Phase) {
			live++
		}
	}
	if g.Spec.MaxInstances != nil && int32(live) >= *g.Spec.MaxInstances {
		return StartedServer{}, ErrTooManyInstances
	}

	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: spawneryv1alpha1.GroupVersion.String(),
				Kind:       "ServerGroup",
				Name:       g.Name,
				UID:        g.UID,
			}},
		},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: group},
			Key:      key,
		},
	}
	if err := w.Client.Create(ctx, srv); err != nil {
		if apierrors.IsAlreadyExists(err) {
			if replacing {
				return StartedServer{}, ErrInstanceStopping
			}
			return StartedServer{Name: name, AlreadyRunning: true}, nil
		}
		return StartedServer{}, err
	}
	return StartedServer{Name: name}, nil
}

// StopServer deletes one member.
//
// The key check is the bound and not a courtesy: this is the only verb on
// this channel that deletes a server outright, and a name that belongs to a
// lobby has to fail rather than work.
func (w KubeWriter) StopServer(ctx context.Context, namespace, name string) error {
	var srv spawneryv1alpha1.Server
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &srv); err != nil {
		if apierrors.IsNotFound(err) {
			return ErrNoSuchServer
		}
		return err
	}
	if srv.Spec.Key == "" {
		return ErrNotAnInstance
	}
	if err := w.Client.Delete(ctx, &srv); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// isTerminal is whether a member's run is over: its object may be replaced
// and it counts against no ceiling.
func isTerminal(p string) bool {
	return p == string(phase.Failed) || p == string(phase.Finished)
}
