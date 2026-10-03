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

package netstatus

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/netstate"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
)

// ErrUnknownTarget is a target that names nothing this audience may see.
var ErrUnknownTarget = errors.New("no group, server or proxy by that name")

type Source struct {
	Reader  client.Reader
	Agents  *agent.Registry
	Metrics MetricsReader
	Clock   func() time.Time
	// MetricsTimeout bounds the live metrics call, which runs on the asking
	// agent's session loop. Zero means DefaultMetricsTimeout.
	MetricsTimeout time.Duration
	Log            logr.Logger
}

// DefaultMetricsTimeout is well under the agent's ten-second request deadline.
const DefaultMetricsTimeout = 3 * time.Second

type view struct {
	serverGroups []spawneryv1alpha1.ServerGroup
	proxyGroups  []spawneryv1alpha1.ProxyGroup
	servers      []spawneryv1alpha1.Server
	pods         map[string]*corev1.Pod
	usage        map[string]Usage
	available    bool
	now          time.Time
}

func (s Source) Status(ctx context.Context, namespace string, audience netstate.Audience, target string) (*agentpb.StatusResult, error) {
	v, err := s.read(ctx, namespace, audience)
	if err != nil {
		return nil, err
	}
	if target == "" {
		return v.network(s.Agents), nil
	}
	for i := range v.serverGroups {
		if g := &v.serverGroups[i]; g.Name == target {
			return v.serverGroup(s.Agents, g), nil
		}
	}
	for i := range v.proxyGroups {
		if g := &v.proxyGroups[i]; g.Name == target {
			return v.proxyGroup(s.Agents, g), nil
		}
	}
	for i := range v.servers {
		if srv := &v.servers[i]; srv.Name == target {
			in := v.server(s.Agents, srv)
			return &agentpb.StatusResult{Total: in.Usage, Instances: []*agentpb.InstanceStatus{in},
				MetricsAvailable: v.available}, nil
		}
	}
	if p, ok := v.pods[target]; ok && p.Labels[podspec.LabelRole] == podspec.RoleProxy && p.DeletionTimestamp.IsZero() {
		in := v.proxy(s.Agents, p)
		return &agentpb.StatusResult{Total: in.Usage, Instances: []*agentpb.InstanceStatus{in},
			MetricsAvailable: v.available}, nil
	}
	return nil, ErrUnknownTarget
}

// read never drops pods: hiding a name must not hide its usage from the total.
func (s Source) read(ctx context.Context, namespace string, audience netstate.Audience) (*view, error) {
	v := &view{pods: map[string]*corev1.Pod{}, now: s.Clock()}
	var sgs spawneryv1alpha1.ServerGroupList
	if err := s.Reader.List(ctx, &sgs, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list server groups in %s: %w", namespace, err)
	}
	for _, g := range sgs.Items {
		if audience == netstate.ForServers && g.IsOnDemand() {
			continue
		}
		v.serverGroups = append(v.serverGroups, g)
	}
	var pgs spawneryv1alpha1.ProxyGroupList
	if err := s.Reader.List(ctx, &pgs, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list proxy groups in %s: %w", namespace, err)
	}
	v.proxyGroups = pgs.Items
	var servers spawneryv1alpha1.ServerList
	if err := s.Reader.List(ctx, &servers, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list servers in %s: %w", namespace, err)
	}
	for _, srv := range servers.Items {
		if audience == netstate.ForServers && netstate.IsPrivateServer(&srv) {
			continue
		}
		v.servers = append(v.servers, srv)
	}
	var pods corev1.PodList
	if err := s.Reader.List(ctx, &pods, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("list pods in %s: %w", namespace, err)
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodPending || p.Status.Phase == corev1.PodRunning {
			v.pods[p.Name] = p
		}
	}
	timeout := s.MetricsTimeout
	if timeout == 0 {
		timeout = DefaultMetricsTimeout
	}
	metricsCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	usage, err := s.Metrics.PodUsage(metricsCtx, namespace)
	if err != nil {
		s.Log.Info("pod metrics unavailable for /cloud status", "namespace", namespace, "reason", err.Error())
	}
	v.usage, v.available = usage, err == nil
	sort.Slice(v.serverGroups, func(i, j int) bool { return v.serverGroups[i].Name < v.serverGroups[j].Name })
	sort.Slice(v.proxyGroups, func(i, j int) bool { return v.proxyGroups[i].Name < v.proxyGroups[j].Name })
	sort.Slice(v.servers, func(i, j int) bool { return v.servers[i].Name < v.servers[j].Name })
	return v, nil
}

func (v *view) add(u *agentpb.ResourceUsage, p *corev1.Pod) {
	u.Pods++
	for _, c := range p.Spec.Containers {
		u.CpuRequestedMillicores += c.Resources.Requests.Cpu().MilliValue()
		u.MemoryRequestedBytes += c.Resources.Requests.Memory().Value()
		if q, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
			u.CpuLimitMillicores += q.MilliValue()
		} else {
			u.CpuUnlimited = true
		}
		if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
			u.MemoryLimitBytes += q.Value()
		} else {
			u.MemoryUnlimited = true
		}
	}
	if m, ok := v.usage[p.Name]; ok {
		u.PodsMeasured++
		u.CpuUsedMillicores += m.CPUMilli
		u.MemoryUsedBytes += m.MemoryBytes
	}
}

func (v *view) groupPods(role, group string) []*corev1.Pod {
	var out []*corev1.Pod
	for _, p := range v.pods {
		if p.Labels[podspec.LabelRole] == role && p.Labels[podspec.LabelGroup] == group {
			out = append(out, p)
		}
	}
	return out
}

// ticks returns zeros once the report is stale.
func ticks(agents *agent.Registry, podUID string) (float64, float64) {
	if podUID == "" {
		return 0, 0
	}
	snap := agents.Lookup(podUID)
	if !snap.Known || snap.PlayersStale {
		return 0, 0
	}
	return snap.TPS, snap.MSPT
}

func (v *view) server(agents *agent.Registry, srv *spawneryv1alpha1.Server) *agentpb.InstanceStatus {
	tps, mspt := ticks(agents, srv.Status.PodUID)
	in := &agentpb.InstanceStatus{
		Name: srv.Name, Group: srv.Spec.GroupRef.Name, Phase: srv.Status.Phase,
		Ready:   srv.Status.Phase == string(phase.Ready),
		Players: srv.Status.Players, Slots: srv.Status.Slots, PlayableSlots: srv.Status.PlayableSlots,
		Tps: tps, Mspt: mspt,
		Retiring: srv.Spec.Retire || srv.Status.Phase == string(phase.Retiring),
		Held:     srv.Spec.Hold,
		Draining: srv.Status.Phase == string(phase.Draining),
		Usage:    &agentpb.ResourceUsage{},
	}
	started := srv.CreationTimestamp.Time
	if p, ok := v.pods[srv.Name]; ok {
		v.add(in.Usage, p)
		in.Node = p.Spec.NodeName
		started = p.CreationTimestamp.Time
	}
	in.AgeSeconds = int64(v.now.Sub(started) / time.Second)
	return in
}

func (v *view) proxy(agents *agent.Registry, p *corev1.Pod) *agentpb.InstanceStatus {
	snap := agents.Lookup(string(p.UID))
	in := &agentpb.InstanceStatus{
		Name: p.Name, Group: p.Labels[podspec.LabelGroup], Proxy: true,
		Ready:   podReady(p),
		Players: snap.Players, Slots: snap.Slots, PlayableSlots: snap.Slots,
		Retiring:   p.Annotations[podspec.AnnotationRetireRequested] != "",
		Draining:   p.Annotations[podspec.AnnotationProxyDrainingSince] != "",
		AgeSeconds: int64(v.now.Sub(p.CreationTimestamp.Time) / time.Second),
		Usage:      &agentpb.ResourceUsage{},
		Node:       p.Spec.NodeName,
	}
	v.add(in.Usage, p)
	return in
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (v *view) serverGroupLine(agents *agent.Registry, g *spawneryv1alpha1.ServerGroup) (*agentpb.GroupStatus, []*agentpb.InstanceStatus) {
	line := &agentpb.GroupStatus{
		Name: g.Name, Kind: kindOf(g), Phase: g.Status.Phase,
		Replicas: g.Status.Replicas, ReadyReplicas: g.Status.ReadyReplicas, Players: g.Status.OnlinePlayers,
		Usage: &agentpb.ResourceUsage{},
	}
	var members []*agentpb.InstanceStatus
	for i := range v.servers {
		if srv := &v.servers[i]; srv.Spec.GroupRef.Name == g.Name {
			in := v.server(agents, srv)
			members = append(members, in)
			if in.Tps > 0 && (line.LowestTps == 0 || in.Tps < line.LowestTps) {
				line.LowestTps = in.Tps
			}
		}
	}
	for _, p := range v.groupPods(podspec.RoleServer, g.Name) {
		v.add(line.Usage, p)
	}
	return line, members
}

func (v *view) proxyGroupLine(agents *agent.Registry, g *spawneryv1alpha1.ProxyGroup) (*agentpb.GroupStatus, []*agentpb.InstanceStatus) {
	line := &agentpb.GroupStatus{
		Name: g.Name, Kind: agentpb.GroupState_PROXY, Phase: g.Status.Phase,
		Replicas: g.Spec.Replicas, ReadyReplicas: g.Status.ReadyReplicas, Players: g.Status.ConnectedPlayers,
		Usage: &agentpb.ResourceUsage{},
	}
	var members []*agentpb.InstanceStatus
	for _, p := range v.groupPods(podspec.RoleProxy, g.Name) {
		v.add(line.Usage, p)
		if p.DeletionTimestamp.IsZero() {
			members = append(members, v.proxy(agents, p))
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
	return line, members
}

func (v *view) serverGroup(agents *agent.Registry, g *spawneryv1alpha1.ServerGroup) *agentpb.StatusResult {
	line, members := v.serverGroupLine(agents, g)
	return &agentpb.StatusResult{Total: line.Usage, Groups: []*agentpb.GroupStatus{line},
		Instances: members, MetricsAvailable: v.available}
}

func (v *view) proxyGroup(agents *agent.Registry, g *spawneryv1alpha1.ProxyGroup) *agentpb.StatusResult {
	line, members := v.proxyGroupLine(agents, g)
	return &agentpb.StatusResult{Total: line.Usage, Groups: []*agentpb.GroupStatus{line},
		Instances: members, MetricsAvailable: v.available}
}

func (v *view) network(agents *agent.Registry) *agentpb.StatusResult {
	res := &agentpb.StatusResult{Total: &agentpb.ResourceUsage{}, Other: &agentpb.ResourceUsage{},
		MetricsAvailable: v.available, Servers: int32(len(v.servers))}
	listed := map[string]bool{}
	for i := range v.serverGroups {
		line, _ := v.serverGroupLine(agents, &v.serverGroups[i])
		res.Groups = append(res.Groups, line)
		listed[podspec.RoleServer+"/"+line.Name] = true
	}
	for i := range v.proxyGroups {
		line, members := v.proxyGroupLine(agents, &v.proxyGroups[i])
		res.Groups = append(res.Groups, line)
		res.Players += line.Players
		res.Proxies += int32(len(members))
		listed[podspec.RoleProxy+"/"+line.Name] = true
	}
	for _, p := range v.pods {
		v.add(res.Total, p)
		if !listed[p.Labels[podspec.LabelRole]+"/"+p.Labels[podspec.LabelGroup]] {
			v.add(res.Other, p)
		}
	}
	return res
}

func kindOf(g *spawneryv1alpha1.ServerGroup) agentpb.GroupState_Kind {
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
