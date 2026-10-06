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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/boost"
	"github.com/spawnery/spawnery/internal/instance"
	"github.com/spawnery/spawnery/internal/netstate"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
)

// ErrNoSuchServer is a sentinel rather than the API machinery's not-found so
// that ClusterWriter stays free of Kubernetes.
var ErrNoSuchServer = errors.New("no such server")

// ErrServerStopping is an unretire for a server that is already draining,
// terminating or finished.
var ErrServerStopping = errors.New("server is already stopping")

var ErrNotRetiring = errors.New("server is not retiring")

var ErrNoSuchGroup = errors.New("no such group")

// ErrNotAServer is a server verb aimed at a proxy.
var ErrNotAServer = errors.New("that is a proxy, not a server")

// ErrGroupNotScalable: only an ephemeral group with spec.scaling adds boosts
// to its floor; on any other group a boost would exist and change nothing.
var ErrGroupNotScalable = errors.New("that group is not sized by scaling")

// ErrGroupNotOnDemand: a Server created by hand in a counted group would be
// condemned by the group's next sizing pass.
var ErrGroupNotOnDemand = errors.New("that group is not on-demand")

var ErrWorldSyncOff = errors.New("the group keeps its worlds in an object store, and world sync is off in this operator")

// WorldDeleter is the operator's hold on a bucket of worlds. Worlds are
// named "<namespace>/<group>/<key>".
type WorldDeleter interface {
	Exists(ctx context.Context, world string) (bool, error)
	MarkDeleted(ctx context.Context, world string) error
	DeletionPending(ctx context.Context, world string) (bool, error)
}

var ErrTooManyInstances = errors.New("that group is at spec.maxInstances")

// ErrNoCeiling is an on-demand group with no spec.maxInstances. The CRD
// requires the field, so this is a group that got around that rule; nil is
// not read as unlimited.
var ErrNoCeiling = errors.New("that group has no spec.maxInstances")

// ErrNotAnInstance is a stop aimed at a server that no key names.
var ErrNotAnInstance = errors.New("that server is not an on-demand member")

// ErrInstanceStopping is a start on a key whose member is on its way out. The
// caller should retry, not be told AlreadyRunning and send a player to a
// server about to stop.
var ErrInstanceStopping = errors.New("that member is still stopping")

var ErrWorldDeleting = errors.New("that member's world is still being deleted")

// ErrForeignClaim is a delete whose claim this operator did not make for that
// group.
var ErrForeignClaim = errors.New("that claim was not made by this operator for that group")

// ErrUnkeyedWorld is a delete of a world whose claim predates the key label.
// The chart's admission policy requires the label and forbids the operator
// to add it, so such a world can only be deleted by hand.
var ErrUnkeyedWorld = errors.New("that world's claim does not carry its key")

// ErrInstancesDraining is the ceiling held by members of which at least one is
// leaving. Unlike ErrTooManyInstances it clears by itself, so the caller
// should ask again shortly.
var ErrInstancesDraining = errors.New("that group is at spec.maxInstances and a member is stopping")

// ErrNameTaken is a start whose composed name belongs to a server of another
// group: group "a" with key "b-xyz" composes the name group "a-b" gives its
// member "xyz".
var ErrNameTaken = errors.New("that name belongs to a server of another group")

// ClusterWriter is every change a plugin's request is allowed to make. It is
// deliberately narrow rather than a client.Client, so what a request from
// inside a game server can reach is readable in one screen.
type ClusterWriter interface {
	// Retire reports whether this call is the one that set the flag.
	Retire(ctx context.Context, namespace, name string) (bool, error)

	// Headroom is a read, here rather than on the network snapshot because it
	// feeds a bound and the snapshot may be a resync stale.
	Headroom(ctx context.Context, namespace, group string) (Headroom, error)

	// Boost creates a ScaleBoost on a group, owned by it. The caller has
	// already bounded the numbers.
	Boost(ctx context.Context, namespace, group string, replicas int32, expiresAt time.Time) error

	// Pin creates an Exact ScaleBoost on a group, owned by it. The caller has
	// already bounded the numbers.
	Pin(ctx context.Context, namespace, group string, replicas int32, expiresAt time.Time) error

	// ForceStop sets spec.forceStop on a server; ErrNotAServer for a proxy's
	// name, ErrNoSuchServer for anything else unknown.
	ForceStop(ctx context.Context, namespace, name string) error

	// StopBoosts deletes every boost on a group and says how many there were.
	StopBoosts(ctx context.Context, namespace, group string) (int, error)

	// StartServer creates the member of an OnDemand group that carries key, or
	// reports AlreadyRunning for one that is there.
	StartServer(ctx context.Context, namespace, group, key string) (StartedServer, error)

	// StopServer deletes one member of an OnDemand group and returns
	// ErrNotAnInstance for any other server.
	StopServer(ctx context.Context, namespace, name string) error
	// DeleteServer deletes a member of an OnDemand group for good: its server
	// if one exists, and its world claim. It returns ErrNoSuchGroup,
	// ErrGroupNotOnDemand, instance.ErrBadKey, ErrForeignClaim, ErrUnkeyedWorld, and
	// ErrNoSuchServer when neither a server nor a world is there.
	DeleteServer(ctx context.Context, namespace, group, key string) (DeletedServer, error)
	// Unretire takes a retirement back and holds the server.
	Unretire(ctx context.Context, namespace, name string) error
}

// DeletedServer is what a delete removed.
type DeletedServer struct {
	Name string
	// World is true when a world claim was deleted.
	World bool
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

// Room is how many more servers a new boost may ask for, measured against the
// floor and not the running count, which would refuse a boost exactly when
// capacity is short.
func (h Headroom) Room() int32 {
	room := h.MaxReplicas - h.MinReplicas - h.Boosted
	if room < 0 {
		return 0
	}
	return room
}

type KubeWriter struct {
	Client client.Client
	// Reader reads claims past the manager's cache, which holds only claims
	// carrying this operator's label: a claim somebody else made under a
	// member's name has to be seen to be refused. Nil means Client.
	Reader client.Reader
	// Nil means time.Now.
	Clock func() time.Time
	// Nil when the operator runs without --world-sync.
	Worlds WorldDeleter
}

func (w KubeWriter) claims() client.Reader {
	if w.Reader == nil {
		return w.Client
	}
	return w.Reader
}

func (w KubeWriter) now() time.Time {
	if w.Clock == nil {
		return time.Now()
	}
	return w.Clock()
}

// Retire sets spec.retire on one server, or retires a proxy of that name.
// The race between read and patch is accepted: losing it only tells a second
// admin they did what they merely repeated.
func (w KubeWriter) Retire(ctx context.Context, namespace, name string) (bool, error) {
	var srv spawneryv1alpha1.Server
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &srv); err != nil {
		if apierrors.IsNotFound(err) {
			return w.retireProxy(ctx, namespace, name)
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

// retireProxy asks the proxy group to drain one proxy: it is replaced, takes
// no new connections and stops once empty.
func (w KubeWriter) retireProxy(ctx context.Context, namespace, name string) (bool, error) {
	var pod corev1.Pod
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return false, ErrNoSuchServer
		}
		return false, err
	}
	if pod.Labels[podspec.LabelRole] != podspec.RoleProxy {
		return false, ErrNoSuchServer
	}
	if pod.Annotations[podspec.AnnotationRetireRequested] != "" || pod.Annotations[podspec.AnnotationProxyDrainingSince] != "" {
		return false, nil
	}
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[podspec.AnnotationRetireRequested] = w.now().UTC().Format(time.RFC3339)
	if err := w.Client.Patch(ctx, &pod, patch); err != nil {
		return false, err
	}
	return true, nil
}

func (w KubeWriter) Unretire(ctx context.Context, namespace, name string) error {
	var srv spawneryv1alpha1.Server
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &srv); err != nil {
		if apierrors.IsNotFound(err) {
			return ErrNoSuchServer
		}
		return err
	}
	switch phase.Phase(srv.Status.Phase) {
	case phase.Draining, phase.Terminating, phase.Finished, phase.Failed:
		return ErrServerStopping
	}
	if !srv.Spec.Retire && phase.Phase(srv.Status.Phase) != phase.Retiring {
		return ErrNotRetiring
	}
	patch := client.MergeFrom(srv.DeepCopy())
	srv.Spec.Retire = false
	srv.Spec.Hold = true
	return w.Client.Patch(ctx, &srv, patch)
}

// Headroom lists the boosts rather than reading status.boostedReplicas, which
// is only as fresh as the last reconcile.
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
		Boosted:     boost.Live(boost.Of(boosts.Items, &g), group, w.now()),
	}, nil
}

func (w KubeWriter) Boost(ctx context.Context, namespace, group string, replicas int32, expiresAt time.Time) error {
	return w.createBoost(ctx, namespace, group, spawneryv1alpha1.ScaleBoostAdd, replicas, expiresAt)
}

func (w KubeWriter) Pin(ctx context.Context, namespace, group string, replicas int32, expiresAt time.Time) error {
	return w.createBoost(ctx, namespace, group, spawneryv1alpha1.ScaleBoostExact, replicas, expiresAt)
}

// createBoost uses generateName so two admins boosting at once get two
// boosts, not a collision.
func (w KubeWriter) createBoost(
	ctx context.Context, namespace, group string, mode spawneryv1alpha1.ScaleBoostMode, replicas int32,
	expiresAt time.Time,
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
			Mode:      mode,
			Replicas:  replicas,
			ExpiresAt: &at,
		},
	})
}

// ForceStop does not refuse a server already set: a second force-stop is what
// shortens a grace period that began before the first.
func (w KubeWriter) ForceStop(ctx context.Context, namespace, name string) error {
	var srv spawneryv1alpha1.Server
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &srv); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		var pod corev1.Pod
		if perr := w.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &pod); perr == nil &&
			pod.Labels[podspec.LabelRole] == podspec.RoleProxy {
			return ErrNotAServer
		}
		return ErrNoSuchServer
	}
	if srv.Spec.ForceStop {
		return nil
	}
	patch := client.MergeFrom(srv.DeepCopy())
	srv.Spec.ForceStop = true
	return w.Client.Patch(ctx, &srv, patch)
}

// StopBoosts removes expired boosts too, so its count matches what an admin
// sees in `kubectl get scaleboosts`.
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
				continue
			}
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// StartServer creates the member, or reports the one that is already there.
// Terminal members do not count against the ceiling.
//
// The create decides the race between two callers for one key, so nothing
// takes a lock; on AlreadyExists, occupant reads the object in the way. The
// ceiling is counted from the list, so two simultaneous creates can put a
// group one over it.
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
	if g.UsesObjectStore() {
		// The node agent would refuse the volume until the sweep is done, and
		// the pod would wait in ContainerCreating past its startup deadline.
		if w.Worlds == nil {
			return StartedServer{}, ErrWorldSyncOff
		}
		pending, err := w.Worlds.DeletionPending(ctx, namespace+"/"+group+"/"+key)
		if err != nil {
			return StartedServer{}, err
		}
		if pending {
			return StartedServer{}, ErrWorldDeleting
		}
	} else {
		// A pod created now would mount a claim on its way out.
		var world corev1.PersistentVolumeClaim
		err = w.claims().Get(ctx, client.ObjectKey{Namespace: namespace, Name: podspec.DataClaimName(name)}, &world)
		switch {
		case err == nil && !world.DeletionTimestamp.IsZero():
			return StartedServer{}, ErrWorldDeleting
		case err != nil && !apierrors.IsNotFound(err):
			return StartedServer{}, err
		}
	}

	var members spawneryv1alpha1.ServerList
	if err := w.Client.List(ctx, &members, client.InNamespace(namespace)); err != nil {
		return StartedServer{}, err
	}
	live, going := 0, 0
	for i := range members.Items {
		m := &members.Items[i]
		if m.Spec.GroupRef.Name != group || !netstate.IsPrivateServer(m) {
			continue
		}
		// The same question occupant asks, so both paths agree.
		if m.Name == name && m.Spec.Key == key {
			if !m.DeletionTimestamp.IsZero() {
				return StartedServer{}, ErrInstanceStopping
			}
			// A terminal run of this key is replaced; its world is on the claim.
			if phase.Terminal(phase.Phase(m.Status.Phase)) {
				if err := w.Client.Delete(ctx, m); err != nil && !apierrors.IsNotFound(err) {
					return StartedServer{}, err
				}
				continue
			}
			return StartedServer{Name: name, AlreadyRunning: true}, nil
		}
		if phase.Terminal(phase.Phase(m.Status.Phase)) {
			continue
		}
		live++
		if !m.DeletionTimestamp.IsZero() {
			going++
		}
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
			return w.occupant(ctx, namespace, name, group, key)
		}
		return StartedServer{}, err
	}
	return StartedServer{Name: name}, nil
}

// occupant says what the object already holding name means for this caller.
func (w KubeWriter) occupant(
	ctx context.Context, namespace, name, group, key string,
) (StartedServer, error) {
	var existing spawneryv1alpha1.Server
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &existing); err != nil {
		if apierrors.IsNotFound(err) {
			return StartedServer{}, ErrInstanceStopping
		}
		return StartedServer{}, err
	}
	if existing.Spec.GroupRef.Name != group || existing.Spec.Key != key {
		return StartedServer{}, ErrNameTaken
	}
	if !existing.DeletionTimestamp.IsZero() || phase.Terminal(phase.Phase(existing.Status.Phase)) {
		return StartedServer{}, ErrInstanceStopping
	}
	return StartedServer{Name: name, AlreadyRunning: true}, nil
}

func (w KubeWriter) StopServer(ctx context.Context, namespace, name string) error {
	var srv spawneryv1alpha1.Server
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &srv); err != nil {
		if apierrors.IsNotFound(err) {
			return ErrNoSuchServer
		}
		return err
	}
	if !netstate.IsPrivateServer(&srv) {
		return ErrNotAnInstance
	}
	if err := w.Client.Delete(ctx, &srv); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// DeleteServer deletes a member and its world. The claim is deleted at once;
// pvc-protection holds it until the pod no longer mounts it.
func (w KubeWriter) DeleteServer(ctx context.Context, namespace, group, key string) (DeletedServer, error) {
	name, err := instance.Name(group, key)
	if err != nil {
		return DeletedServer{}, err
	}
	var g spawneryv1alpha1.ServerGroup
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: group}, &g); err != nil {
		if apierrors.IsNotFound(err) {
			return DeletedServer{}, ErrNoSuchGroup
		}
		return DeletedServer{}, err
	}
	if !g.IsOnDemand() {
		return DeletedServer{}, ErrGroupNotOnDemand
	}
	if g.UsesObjectStore() {
		return w.deleteObjectStoreWorld(ctx, namespace, group, key, name)
	}

	var claim corev1.PersistentVolumeClaim
	haveClaim := true
	if err := w.claims().Get(ctx, client.ObjectKey{Namespace: namespace, Name: podspec.DataClaimName(name)}, &claim); err != nil {
		if !apierrors.IsNotFound(err) {
			return DeletedServer{}, err
		}
		haveClaim = false
	}
	if haveClaim && (claim.Labels[podspec.LabelManagedBy] != podspec.ManagedByValue ||
		claim.Labels[podspec.LabelGroup] != group) {
		return DeletedServer{}, ErrForeignClaim
	}
	if haveClaim && claim.Labels[podspec.LabelKey] != key {
		return DeletedServer{}, ErrUnkeyedWorld
	}

	var srv spawneryv1alpha1.Server
	haveServer := true
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &srv); err != nil {
		if !apierrors.IsNotFound(err) {
			return DeletedServer{}, err
		}
		haveServer = false
	}
	if haveServer && (srv.Spec.GroupRef.Name != group || srv.Spec.Key != key) {
		haveServer = false
	}
	if !haveServer && !haveClaim {
		return DeletedServer{}, ErrNoSuchServer
	}
	if haveServer {
		if err := w.Client.Delete(ctx, &srv); err != nil && !apierrors.IsNotFound(err) {
			return DeletedServer{}, err
		}
	}
	if haveClaim {
		if err := w.Client.Delete(ctx, &claim); err != nil && !apierrors.IsNotFound(err) {
			return DeletedServer{}, err
		}
	}
	return DeletedServer{Name: name, World: haveClaim}, nil
}

func (w KubeWriter) deleteObjectStoreWorld(ctx context.Context, namespace, group, key, name string) (DeletedServer, error) {
	if w.Worlds == nil {
		return DeletedServer{}, ErrWorldSyncOff
	}
	world := namespace + "/" + group + "/" + key
	var srv spawneryv1alpha1.Server
	haveServer := true
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &srv); err != nil {
		if !apierrors.IsNotFound(err) {
			return DeletedServer{}, err
		}
		haveServer = false
	}
	if haveServer && (srv.Spec.GroupRef.Name != group || srv.Spec.Key != key) {
		haveServer = false
	}
	haveWorld, err := w.Worlds.Exists(ctx, world)
	if err != nil {
		return DeletedServer{}, err
	}
	if !haveServer && !haveWorld {
		return DeletedServer{}, ErrNoSuchServer
	}
	if haveServer {
		if err := w.Client.Delete(ctx, &srv); err != nil && !apierrors.IsNotFound(err) {
			return DeletedServer{}, err
		}
	}
	if err := w.Worlds.MarkDeleted(ctx, world); err != nil {
		return DeletedServer{}, err
	}
	return DeletedServer{Name: name, World: haveWorld}, nil
}
