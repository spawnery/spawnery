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

package controller

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/podspec"
)

var (
	groupLabels  = []string{"namespace", "network", "group", "type"}
	serverLabels = []string{"namespace", "network", "group", "server", "node"}
	proxyLabels  = []string{"namespace", "network", "group", "proxy", "node"}

	descNetworkPlayers = prometheus.NewDesc("spawnery_network_players",
		"Players on the network's proxies.", []string{"namespace", "network"}, nil)
	descGroupServers = prometheus.NewDesc("spawnery_group_servers",
		"Servers of the group.", groupLabels, nil)
	descGroupServersReady = prometheus.NewDesc("spawnery_group_servers_ready",
		"Ready servers of the group.", groupLabels, nil)
	descGroupPlayers = prometheus.NewDesc("spawnery_group_players",
		"Players on the group's servers.", groupLabels, nil)
	descGroupFreeSlots = prometheus.NewDesc("spawnery_group_free_slots",
		"Free playable seats of the group.", groupLabels, nil)
	descServerPlayers = prometheus.NewDesc("spawnery_server_players",
		"Players on the server, as its agent last reported.", serverLabels, nil)
	descServerSlots = prometheus.NewDesc("spawnery_server_slots",
		"The server's slots, as its agent last reported.", serverLabels, nil)
	descServerPlayable = prometheus.NewDesc("spawnery_server_playable_slots",
		"The server's effective playable slots.", serverLabels, nil)
	descServerTPS = prometheus.NewDesc("spawnery_server_tps",
		"The server's one-minute ticks per second.", serverLabels, nil)
	descServerMSPT = prometheus.NewDesc("spawnery_server_mspt",
		"The server's mean tick time in milliseconds.", serverLabels, nil)
	descServerHeapUsed = prometheus.NewDesc("spawnery_server_heap_used_bytes",
		"JVM heap in use on the server.", serverLabels, nil)
	descServerHeapMax = prometheus.NewDesc("spawnery_server_heap_max_bytes",
		"The server's JVM max heap.", serverLabels, nil)
	descServerPhase = prometheus.NewDesc("spawnery_server_phase",
		"1 for the server's current phase.", append(append([]string{}, serverLabels...), "phase"), nil)
	descProxyPlayers = prometheus.NewDesc("spawnery_proxy_players",
		"Players on the proxy.", proxyLabels, nil)
	descProxyHeapUsed = prometheus.NewDesc("spawnery_proxy_heap_used_bytes",
		"JVM heap in use on the proxy.", proxyLabels, nil)
	descProxyHeapMax = prometheus.NewDesc("spawnery_proxy_heap_max_bytes",
		"The proxy's JVM max heap.", proxyLabels, nil)
)

// NetworkMetrics is registered at init so the metrics reference lists its
// series; SetupAll binds it.
var NetworkMetrics = &NetworkCollector{}

func init() { metrics.Registry.MustRegister(NetworkMetrics) }

// NetworkCollector keeps nothing between scrapes, so a server that is gone
// leaves no series behind.
type NetworkCollector struct {
	mu     sync.RWMutex
	reader client.Reader
	agents *agent.Registry
}

// Bind: an unbound collector emits nothing.
func (c *NetworkCollector) Bind(reader client.Reader, agents *agent.Registry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reader, c.agents = reader, agents
}

func (c *NetworkCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{
		descNetworkPlayers, descGroupServers, descGroupServersReady, descGroupPlayers, descGroupFreeSlots,
		descServerPlayers, descServerSlots, descServerPlayable, descServerTPS, descServerMSPT,
		descServerHeapUsed, descServerHeapMax, descServerPhase,
		descProxyPlayers, descProxyHeapUsed, descProxyHeapMax,
	} {
		ch <- d
	}
}

func (c *NetworkCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	reader, agents := c.reader, c.agents
	c.mu.RUnlock()
	if reader == nil || agents == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	logger := log.FromContext(ctx).WithName("network-metrics")
	gauge := func(d *prometheus.Desc, v float64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, labels...)
	}

	var groups spawneryv1alpha1.ServerGroupList
	if err := reader.List(ctx, &groups); err != nil {
		logger.Error(err, "list server groups")
		return
	}
	byGroup := map[[2]string]*spawneryv1alpha1.ServerGroup{}
	for i := range groups.Items {
		g := &groups.Items[i]
		byGroup[[2]string{g.Namespace, g.Name}] = g
		l := []string{g.Namespace, g.Spec.NetworkRef.Name, g.Name, string(g.Spec.Type)}
		gauge(descGroupServers, float64(g.Status.Replicas), l...)
		gauge(descGroupServersReady, float64(g.Status.ReadyReplicas), l...)
		gauge(descGroupPlayers, float64(g.Status.OnlinePlayers), l...)
		gauge(descGroupFreeSlots, float64(g.Status.FreeSlots), l...)
	}

	var serverPods corev1.PodList
	if err := reader.List(ctx, &serverPods, client.MatchingLabels{podspec.LabelRole: podspec.RoleServer}); err != nil {
		logger.Error(err, "list server pods")
	}
	nodeOf := map[[2]string]string{}
	for i := range serverPods.Items {
		p := &serverPods.Items[i]
		nodeOf[[2]string{p.Namespace, p.Name}] = p.Spec.NodeName
	}

	var servers spawneryv1alpha1.ServerList
	if err := reader.List(ctx, &servers); err != nil {
		logger.Error(err, "list servers")
	}
	for i := range servers.Items {
		s := &servers.Items[i]
		g := byGroup[[2]string{s.Namespace, s.Spec.GroupRef.Name}]
		network := ""
		var specPlayable *int32
		if g != nil {
			network, specPlayable = g.Spec.NetworkRef.Name, g.Spec.PlayableSlots
		}
		l := []string{s.Namespace, network, s.Spec.GroupRef.Name, s.Name, nodeOf[[2]string{s.Namespace, s.Status.PodName}]}
		if s.Status.Phase != "" {
			gauge(descServerPhase, 1, append(append([]string{}, l...), s.Status.Phase)...)
		}
		snap := agents.Lookup(s.Status.PodUID)
		if s.Status.PodUID == "" || !reporting(snap) {
			continue
		}
		gauge(descServerPlayers, float64(snap.Players), l...)
		gauge(descServerSlots, float64(snap.Slots), l...)
		gauge(descServerPlayable, float64(playableSeats(snap.PlayableSlots, specPlayable, snap.Slots)), l...)
		if snap.TPS > 0 {
			gauge(descServerTPS, snap.TPS, l...)
		}
		if snap.MSPT > 0 {
			gauge(descServerMSPT, snap.MSPT, l...)
		}
		if snap.HeapMax > 0 {
			gauge(descServerHeapUsed, float64(snap.HeapUsed), l...)
			gauge(descServerHeapMax, float64(snap.HeapMax), l...)
		}
	}

	var proxies corev1.PodList
	if err := reader.List(ctx, &proxies, client.MatchingLabels{podspec.LabelRole: podspec.RoleProxy}); err != nil {
		logger.Error(err, "list proxy pods")
		return
	}
	networkPlayers := map[[2]string]int32{}
	for i := range proxies.Items {
		p := &proxies.Items[i]
		snap := agents.Lookup(string(p.UID))
		if !reporting(snap) {
			continue
		}
		network := p.Labels[podspec.LabelNetwork]
		l := []string{p.Namespace, network, p.Labels[podspec.LabelGroup], p.Name, p.Spec.NodeName}
		gauge(descProxyPlayers, float64(snap.Players), l...)
		if snap.HeapMax > 0 {
			gauge(descProxyHeapUsed, float64(snap.HeapUsed), l...)
			gauge(descProxyHeapMax, float64(snap.HeapMax), l...)
		}
		networkPlayers[[2]string{p.Namespace, network}] += snap.Players
	}
	for k, n := range networkPlayers {
		gauge(descNetworkPlayers, float64(n), k[0], k[1])
	}
}

// reporting: an entry outlives its stream until it is forgotten, so a crashed
// server would otherwise keep its last TPS and heap as a flat line.
func reporting(snap agent.Snapshot) bool {
	return snap.Known && snap.Connected && !snap.PlayersStale && snap.Slots > 0
}
