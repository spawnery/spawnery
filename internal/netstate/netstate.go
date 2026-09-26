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
// receive.
//
// It exists so that there is exactly one builder. The two channels have two
// fan-outs -- internal/proxyreg for proxies, internal/serverreg for backends,
// and internal/serverreg's own comment argues why they are two -- but the
// question "what does this network look like right now" has to have one
// answer. The plugin API's whole premise is that the same call returns the
// same thing on either side of the proxy, and two builders would eventually
// make that false in a way no test on either side could see. The one place
// the two pictures differ is named, and is an Audience.
package netstate

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/podspec"
)

// Audience is which kind of agent a picture is for.
//
// A network with three hundred private servers running would hand every lobby
// three hundred entries, and a fresh picture on every start and stop, for
// servers no lobby will ever send anyone to. Proxies need them -- that is
// where routing is, and where the consumer's plugin runs -- and backends need
// neither the members nor their group.
//
// The split is by audience and not by a flag on the group. A backend that
// could opt in would be a backend whose plugins start reading these entries,
// and taking them out again would then be a breaking change to somebody's
// code; narrower now is the only direction that can still be widened later.
type Audience int

const (
	// ForProxies is the whole namespace.
	ForProxies Audience = iota
	// ForServers leaves out on-demand groups and their members, and blanks the
	// server a roster entry names when it is one of those members.
	ForServers
)

// IsPrivateServer reports whether a server is a member of an on-demand group.
//
// The one place that says so, for everything that treats such a server
// differently: the filter in Build, and the answer a backend is given when it
// names one. The marker is the member's own key rather than its group's type,
// because a Server carries it so that nothing has to fetch the group to know
// (see ServerSpec.Key) -- and it is a marker that has moved once already.
func IsPrivateServer(srv *spawneryv1alpha1.Server) bool {
	return srv.Spec.Key != ""
}

// AudienceOf is the picture an agent in this role is sent, and the one a
// request from it is resolved against.
//
// Anything but a proxy gets the narrower picture, so a role added later is
// shown too little until somebody decides otherwise rather than too much.
func AudienceOf(role agent.Role) Audience {
	if role == agent.RoleProxy {
		return ForProxies
	}
	return ForServers
}

// Source is what a NetworkState is built from: the objects, and the players.
//
// The split is not incidental. Groups and servers are custom resources and
// come from the manager's cache, so reading them is an indexer lookup rather
// than an API round trip. Players are in memory in the registry and are in no
// object at all -- see docs/explanation/network-boundaries.md for why they stay there.
type Source struct {
	// Reader is the manager's cached client.
	Reader client.Reader
	// Agents holds the rosters the proxies report.
	Agents *agent.Registry
}

// Build describes one namespace to one kind of agent.
//
// Every slice it returns is sorted. A message assembled from map iteration
// differs between two identical states, and every consumer here wants the
// opposite: a test that asserts a list, a reader comparing two resyncs, and
// anything that might later skip a resend because nothing changed.
func (s Source) Build(ctx context.Context, namespace string, audience Audience) (*agentpb.NetworkState, error) {
	state := &agentpb.NetworkState{}

	// The chat feed's format, from whichever Network this namespace holds.
	//
	// A failed read is a blank format and not an error: the agent reads blank
	// as "use my own default", so a Network that cannot be listed costs a
	// styling choice rather than the whole picture -- and the picture is what
	// a plugin and the proxies' routing depend on.
	//
	// The first Network wins if a namespace somehow holds two. That is not a
	// state this operator allows: the Network controller refuses a duplicate
	// with ReasonDuplicateNetwork, so a second one is already not Accepted and
	// owns nothing here.
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
			Name:          g.Name,
			Kind:          serverGroupKind(g),
			Replicas:      g.Status.Replicas,
			ReadyReplicas: g.Status.ReadyReplicas,
			OnlinePlayers: g.Status.OnlinePlayers,
			FreeSlots:     g.Status.FreeSlots,
			// From the spec and not the status: nobody derived this, somebody
			// wrote it down.
			Attributes:  g.Spec.Attributes,
			DisplayName: g.Spec.DisplayName,
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
			// spec.replicas and not a status field, because ProxyGroupStatus
			// publishes none -- and that is a real difference from a
			// ServerGroup, whose Replicas here is *observed*. For a proxy
			// group this number is what was asked for, so during a rollout it
			// can exceed what exists while ReadyReplicas below tells the
			// truth about what is serving. A plugin comparing the two across
			// group kinds is comparing two different questions, which is why
			// this comment is here rather than only in the CRD.
			Replicas:      g.Spec.Replicas,
			ReadyReplicas: g.Status.ReadyReplicas,
			OnlinePlayers: g.Status.ConnectedPlayers,
			Attributes:    g.Spec.Attributes,
			DisplayName:   g.Spec.DisplayName,
			// No free-slot figure: capacity is a backend's property, and
			// inventing a number here would answer a question nobody asked of
			// a proxy.
		})
	}

	// What each server says about itself. In no object either, and for a
	// different reason than the roster: a roster is somebody else's personal
	// data, while this is a game's own word about itself and stays in memory
	// because the operator never acts on it. A status field would mean an etcd
	// write every time a round changed what it was doing, CRD validation over
	// text nothing here reads, and a description outliving the pod that meant
	// it.
	//
	// A server that has announced nothing is absent from this map and gets the
	// zero values below, which is the same picture as a server whose agent
	// predates the verb.
	announcements := s.Agents.Announcements(namespace)

	var servers spawneryv1alpha1.ServerList
	if err := s.Reader.List(ctx, &servers, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list servers in %s: %w", namespace, err)
	}
	// Collected here rather than derived below, because this is the loop that
	// drops them: the roster is built from a different source and would
	// otherwise name servers this picture does not contain.
	private := map[string]bool{}
	for i := range servers.Items {
		srv := &servers.Items[i]
		if IsPrivateServer(srv) {
			private[srv.Name] = true
		}
		if audience == ForServers && IsPrivateServer(srv) {
			continue
		}
		announced := announcements[srv.Name]
		state.Servers = append(state.Servers, &agentpb.ServerState{
			Name:  srv.Name,
			Group: srv.Spec.GroupRef.Name,
			// The operator's own spelling, unmapped. See the proto's comment:
			// an agent older than a phase has to be able to read it as
			// something it does not know.
			Phase:      srv.Status.Phase,
			Players:    srv.Status.Players,
			Slots:      srv.Status.Slots,
			Registered: srv.Status.Registered,
			State:      announced.State,
			Attributes: announced.Attributes,
			// Which run of this server this is. From the status, because it is
			// the operator's own record of the pod it made.
			Incarnation: srv.Status.PodUID,
			// From the spec and not the status: the group decided this when it
			// created the server, and nothing observes it afterwards.
			Number: srv.Spec.Number,
			Held:   srv.Spec.Hold,
		})
	}

	// Players are not in any object. An empty roster is a state and not an
	// error -- a namespace whose proxies have not reported recently has no
	// player list -- so the staleness flag is deliberately not consulted here:
	// Roster already returns nothing from a stale proxy, and a namespace with
	// no fresh proxy and a namespace with nobody online are the same picture
	// from a plugin's side.
	roster, _ := s.Agents.Roster(namespace)
	for _, p := range roster {
		server := p.Server
		// The player stays, the address goes. A backend learns that they are
		// online and not where, which is what a lobby's count and its
		// "somebody is on" both want; filtering the entry out instead would
		// take a player off players() while they are still on the network.
		//
		// The name is what the audience split is for: it is a server this
		// picture does not list, and stopServer and retire each act on a name
		// a backend supplies.
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
			Name:     pod.Name,
			Group:    pod.Labels[podspec.LabelGroup],
			Ready:    podReady(pod),
			Draining: pod.Annotations[podspec.AnnotationProxyDrainingSince] != "",
			Players:  s.Agents.Lookup(string(pod.UID)).Players,
		})
	}

	sort.Slice(state.Groups, func(i, j int) bool { return state.Groups[i].Name < state.Groups[j].Name })
	sort.Slice(state.Proxies, func(i, j int) bool { return state.Proxies[i].Name < state.Proxies[j].Name })
	sort.Slice(state.Servers, func(i, j int) bool { return state.Servers[i].Name < state.Servers[j].Name })
	sort.Slice(state.Players, func(i, j int) bool { return state.Players[i].Uuid < state.Players[j].Uuid })
	return state, nil
}

// serverGroupKind maps a ServerGroup's own type onto the wire enum.
//
// A type this build does not recognise becomes KIND_UNSPECIFIED rather than a
// guess. The CRD's validation makes a fourth value impossible today; the point
// is that adding one later reaches an old agent as "unknown" rather than as
// whichever kind happened to be the default.
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

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
