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

// Package netstate builds the picture of a namespace that both agent kinds
// receive. It is the only builder, so the plugin API returns the same thing on
// either side of the proxy; the one difference is an Audience.
package netstate

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
)

// Audience is which kind of agent a picture is for. Backends get no on-demand
// groups or members: a lobby would otherwise carry hundreds of private servers
// it never routes to.
//
// The split is by audience, not by an opt-in flag on the group: once a
// backend's plugins read these entries, taking them out again would break
// somebody's code.
type Audience int

const (
	ForProxies Audience = iota
	// ForServers leaves out on-demand groups and their members, and blanks the
	// server a roster entry names when it is one of those members.
	ForServers
)

// IsPrivateServer reports whether a server is a member of an on-demand group.
// It reads the member's own key so nothing has to fetch the group.
func IsPrivateServer(srv *spawneryv1alpha1.Server) bool {
	return srv.Spec.Key != ""
}

// AudienceOf is the picture an agent in this role is sent, and the one a
// request from it is resolved against. Anything but a proxy gets the narrower
// one, so a role added later is shown too little rather than too much.
func AudienceOf(role agent.Role) Audience {
	if role == agent.RoleProxy {
		return ForProxies
	}
	return ForServers
}

// Source is what a NetworkState is built from: groups and servers from the
// manager's cache, players from the in-memory registry (see
// docs/explanation/network-boundaries.md).
type Source struct {
	Reader client.Reader
	Agents *agent.Registry
}

// Build describes one namespace to one kind of agent. Every slice it returns
// is sorted, so two identical states produce identical messages.
func (s Source) Build(ctx context.Context, namespace string, audience Audience) (*agentpb.NetworkState, error) {
	state := &agentpb.NetworkState{}

	// A failed read is a blank format, not an error: the agent reads blank as its
	// own default. The Network controller refuses a second Network per namespace,
	// so taking the first is safe.
	var networks spawneryv1alpha1.NetworkList
	if err := s.Reader.List(ctx, &networks, client.InNamespace(namespace)); err == nil {
		for i := range networks.Items {
			if d := networks.Items[i].Spec.Defaults; d != nil && d.FeedFormat != "" {
				state.FeedFormat = d.FeedFormat
				break
			}
		}
	}

	var serverGroups spawneryv1alpha1.ServerGroupList
	if err := s.Reader.List(ctx, &serverGroups, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list server groups in %s: %w", namespace, err)
	}
	for i := range serverGroups.Items {
		g := &serverGroups.Items[i]
		if audience == ForServers && g.IsOnDemand() {
			continue
		}
		state.Groups = append(state.Groups, &agentpb.GroupState{
			Name:                   g.Name,
			Kind:                   serverGroupKind(g),
			RoutingStrategy:        routingStrategy(g),
			Replicas:               g.Status.Replicas,
			ReadyReplicas:          g.Status.ReadyReplicas,
			OnlinePlayers:          g.Status.OnlinePlayers,
			FreeSlots:              g.Status.FreeSlots,
			Attributes:             g.Spec.Attributes,
			DisplayName:            g.Spec.DisplayName,
			PlayableSlots:          ptr.Deref(g.Spec.PlayableSlots, 0),
			EnforcePlayableSlots:   g.Spec.EnforcePlayableSlots,
			JoinPermission:         g.Spec.JoinPermission.ResolvedNode(g.Name),
			JoinPermissionDenyOnly: g.Spec.JoinPermission.DenyOnly(),
			Pinned:                 g.Status.PinnedReplicas != nil,
			PinnedReplicas:         ptr.Deref(g.Status.PinnedReplicas, 0),
			PinnedUntilUnix:        unixOrZero(g.Status.PinnedUntil),
		})
	}

	var proxyGroups spawneryv1alpha1.ProxyGroupList
	if err := s.Reader.List(ctx, &proxyGroups, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list proxy groups in %s: %w", namespace, err)
	}
	for i := range proxyGroups.Items {
		g := &proxyGroups.Items[i]
		state.Groups = append(state.Groups, &agentpb.GroupState{
			Name: g.Name,
			Kind: agentpb.GroupState_PROXY,
			// spec.replicas: ProxyGroupStatus publishes no observed count, so unlike
			// a ServerGroup's this is what was asked for and can exceed what
			// exists during a rollout.
			Replicas:      g.Spec.Replicas,
			ReadyReplicas: g.Status.ReadyReplicas,
			OnlinePlayers: g.Status.ConnectedPlayers,
			Attributes:    g.Spec.Attributes,
			DisplayName:   g.Spec.DisplayName,
			// No free-slot figure: capacity is a backend's property.
		})
	}

	// Announcements stay in memory because the operator never acts on them. A
	// server that announced nothing is absent and gets the zero values.
	announcements := s.Agents.Announcements(namespace)
	closedDoors := s.Agents.ClosedDoors(namespace)

	var servers spawneryv1alpha1.ServerList
	if err := s.Reader.List(ctx, &servers, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list servers in %s: %w", namespace, err)
	}
	var serverPods corev1.PodList
	if err := s.Reader.List(ctx, &serverPods, client.InNamespace(namespace),
		client.MatchingLabels{podspec.LabelRole: podspec.RoleServer}); err != nil {
		return nil, fmt.Errorf("list server pods in %s: %w", namespace, err)
	}
	nodeOf := make(map[string]string, len(serverPods.Items))
	for i := range serverPods.Items {
		nodeOf[serverPods.Items[i].Name] = serverPods.Items[i].Spec.NodeName
	}
	// Collected before the audience filter drops them: the roster comes from a
	// different source and would otherwise name servers this picture omits.
	private := map[string]bool{}
	for i := range servers.Items {
		srv := &servers.Items[i]
		if IsPrivateServer(srv) {
			private[srv.Name] = true
		}
		if audience == ForServers && IsPrivateServer(srv) {
			continue
		}
		state.Servers = append(state.Servers, serverState(
			srv, nodeOf[srv.Status.PodName], announcements[srv.Name], closedDoors[srv.Status.PodUID],
			s.Agents.DesiredServerHash(namespace, srv.Spec.GroupRef.Name)))
	}

	// The staleness flag is not consulted: a namespace with no fresh proxy and one
	// with nobody online are the same picture to a plugin.
	roster, _ := s.Agents.Roster(namespace)
	for _, p := range roster {
		server := p.Server
		// The player stays, the private server's name goes: a backend still counts
		// them as online, but cannot stop or retire a server it is not shown.
		if audience == ForServers && private[server] {
			server = ""
		}
		state.Players = append(state.Players, &agentpb.RosterEntry{
			Uuid: p.UUID, Name: p.Name, Server: server,
		})
	}

	var proxies corev1.PodList
	if err := s.Reader.List(ctx, &proxies, client.InNamespace(namespace),
		client.MatchingLabels{podspec.LabelRole: podspec.RoleProxy}); err != nil {
		return nil, fmt.Errorf("list proxies in %s: %w", namespace, err)
	}
	for i := range proxies.Items {
		pod := &proxies.Items[i]
		if !pod.DeletionTimestamp.IsZero() {
			continue
		}
		state.Proxies = append(state.Proxies, &agentpb.ProxyState{
			Name:             pod.Name,
			Group:            pod.Labels[podspec.LabelGroup],
			Ready:            podReady(pod),
			Draining:         pod.Annotations[podspec.AnnotationProxyDrainingSince] != "",
			Players:          s.Agents.Lookup(string(pod.UID)).Players,
			Node:             pod.Spec.NodeName,
			AcceptsTransfers: acceptsTransfers(pod),
		})
	}

	sort.Slice(state.Groups, func(i, j int) bool { return state.Groups[i].Name < state.Groups[j].Name })
	sort.Slice(state.Proxies, func(i, j int) bool { return state.Proxies[i].Name < state.Proxies[j].Name })
	sort.Slice(state.Servers, func(i, j int) bool { return state.Servers[i].Name < state.Servers[j].Name })
	sort.Slice(state.Players, func(i, j int) bool { return state.Players[i].Uuid < state.Players[j].Uuid })
	return state, nil
}

// Unschedulable reports whether the Kubernetes scheduler has found no node for
// the server's pod: the server is Pending and its Ready condition carries
// PodUnschedulable. It reads the status the Server controller wrote, so the
// agents, the state machine and the group's Progressing condition agree.
func Unschedulable(srv *spawneryv1alpha1.Server) bool {
	if srv.Status.Phase != string(phase.Pending) {
		return false
	}
	c := meta.FindStatusCondition(srv.Status.Conditions, spawneryv1alpha1.ConditionReady)
	return c != nil && c.Reason == phase.ReasonPodUnschedulable
}

// serverState is one server's entry in a picture. announced and joinsClosed
// are what the agents reported, which no CR carries. want is the spec hash the
// group's servers should carry, empty when the operator has not computed one.
func serverState(srv *spawneryv1alpha1.Server, node string, announced agent.Announcement, joinsClosed bool, want string) *agentpb.ServerState {
	return &agentpb.ServerState{
		Name:  srv.Name,
		Group: srv.Spec.GroupRef.Name,
		// Unmapped, so an agent older than a phase reads it as unknown.
		Phase:         srv.Status.Phase,
		Players:       srv.Status.Players,
		Slots:         srv.Status.Slots,
		PlayableSlots: srv.Status.PlayableSlots,
		Registered:    srv.Status.Registered,
		State:         announced.State,
		Attributes:    announced.Attributes,
		Incarnation:   srv.Status.PodUID,
		Number:        srv.Spec.Number,
		Held:          srv.Spec.Hold,
		Node:          node,
		Resources:     ToStartResources(srv.Spec.Resources, srv.Spec.Heap),
		// Cleared by a node: the status can lag the bind by one reconcile, and
		// the two fields never both say yes.
		Unschedulable: node == "" && Unschedulable(srv),
		// Keyed by pod UID: a persistent server's name outlives the pod that
		// closed this door.
		JoinsClosed: joinsClosed,
		Stale:       want != "" && srv.Spec.PodHash != "" && srv.Spec.PodHash != want,
	}
}

// Visible is the part of a server's entry that a lifecycle change moves: its
// phase, its pod, where it runs and whether it can be placed. What a report
// moves (players, slots, announcements, the door) is left blank, so comparing
// two of these tells whether a proxy needs a fresh picture now or can wait for
// the next resync. want is the group's desired spec hash, so that Stale agrees
// with what Build reports.
func Visible(srv *spawneryv1alpha1.Server, node, want string) *agentpb.ServerState {
	state := serverState(srv, node, agent.Announcement{}, false, want)
	state.Players = 0
	state.Slots = 0
	state.PlayableSlots = 0
	return state
}

func routingStrategy(g *spawneryv1alpha1.ServerGroup) agentpb.GroupState_RoutingStrategy {
	if g.RoutingStrategy() == spawneryv1alpha1.RoutingFill {
		return agentpb.GroupState_ROUTING_FILL
	}
	return agentpb.GroupState_ROUTING_SPREAD
}

// serverGroupKind maps a ServerGroup's type onto the wire enum. An unknown type
// becomes KIND_UNSPECIFIED so a future value reaches an old agent as unknown
// rather than as a guess.
func serverGroupKind(g *spawneryv1alpha1.ServerGroup) agentpb.GroupState_Kind {
	switch g.Spec.Type {
	case spawneryv1alpha1.ServerGroupEphemeral:
		return agentpb.GroupState_EPHEMERAL
	case spawneryv1alpha1.ServerGroupPersistent:
		return agentpb.GroupState_PERSISTENT
	case spawneryv1alpha1.ServerGroupOnDemand:
		return agentpb.GroupState_ON_DEMAND
	default:
		return agentpb.GroupState_KIND_UNSPECIFIED
	}
}

// acceptsTransfers reads the pod and not its group: during a roll the two
// disagree, and the pod is what Velocity was started with.
func acceptsTransfers(pod *corev1.Pod) bool {
	for _, c := range pod.Spec.Containers {
		if c.Name != podspec.ProxyContainerName {
			continue
		}
		for _, e := range c.Env {
			if e.Name == podspec.EnvTransferForceAfterSeconds {
				return true
			}
		}
	}
	return false
}

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func unixOrZero(t *metav1.Time) int64 {
	if t == nil {
		return 0
	}
	return t.Unix()
}
