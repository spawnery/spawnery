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
