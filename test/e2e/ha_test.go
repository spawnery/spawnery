//go:build e2e

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

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agentendpoint"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
)

const (
	haGate = "SPAWNERY_E2E_HA"

	haNamespace   = "minecraft-ha"
	haServerGroup = "lobby"
	haProxyGroup  = "gateway"
	haServers     = 2

	// Long enough past a takeover for a server whose agent missed
	// ReconnectGrace to be seen leaving Ready.
	haHold = phase.ReconnectGrace + 15*time.Second

	operatorMetricsPort = 8080

	haBlindPolls  = 3
	haPollTimeout = 2 * time.Second
)

var (
	agentSlice = agentendpoint.SliceName(podspec.AgentServiceName, discoveryv1.AddressTypeIPv4)

	unregisteredBackend = regexp.MustCompile(`spawnery: unregistered backend`)
	openStreamsLine     = regexp.MustCompile(`(?m)^spawnery_agent_open_streams\{role="([a-z]+)"\} (\S+)$`)
)

// The manifest of every handover case. Both groups stay off the operator
// pods' nodes, so that pausing the leader's node takes neither a server nor
// the proxy with it.
const haManifest = `apiVersion: v1
kind: Secret
metadata:
  name: velocity-forwarding-secret
  namespace: minecraft-ha
stringData:
  secret: e2e-ha-forwarding-secret
---
apiVersion: spawnery.cloud/v1alpha1
kind: Network
metadata:
  name: ha
  namespace: minecraft-ha
spec:
  forwardingSecretRef:
    name: velocity-forwarding-secret
  scheduling:
    allowedNodeSelectorKeys:
      - kubernetes.io/hostname
  defaults:
    minecraftVersion: "26.3"
    # Below one CPU: every kind node reports all of the host's CPUs. The
    # memory limit is what the image sizes its heap from.
    resources:
      requests:
        cpu: 250m
        memory: 1Gi
      limits:
        memory: 1536Mi
---
apiVersion: spawnery.cloud/v1alpha1
kind: ServerGroup
metadata:
  name: lobby
  namespace: minecraft-ha
spec:
  networkRef:
    name: ha
  type: Ephemeral
  image: %[1]s
  maxPlayers: 20
  scaling:
    minReplicas: 2
    maxReplicas: 2
    spareSlots: 0
  scheduling:
    affinity: %[3]s
---
apiVersion: spawnery.cloud/v1alpha1
kind: ProxyGroup
metadata:
  name: gateway
  namespace: minecraft-ha
spec:
  networkRef:
    name: ha
  replicas: 1
  image: %[2]s
  resources:
    requests:
      cpu: 250m
      memory: 512Mi
    limits:
      memory: 768Mi
  scheduling:
    affinity: %[3]s
  expose:
    type: ClusterIP
    clusterIP:
      address: mc.ha.e2e.test
  routing:
    fallbackGroups:
      - lobby
  config:
    onlineMode: false
`

func haGated(t *testing.T) {
	t.Helper()
	if os.Getenv(haGate) != "1" {
		t.Skipf("set %s=1 to run the standby-replica cases; hack/e2e-ha.sh does, hack/e2e.sh does "+
			"not -- it has one node, no real game image and no released chart to upgrade from", haGate)
	}
}

type haFixture struct {
	leader  *corev1.Pod
	proxy   string
	servers []string
	// restarts holds each "pod/container"'s restart count from when the
	// network came up.
	restarts map[string]int32
}

func (f *haFixture) pods() []string {
	return append([]string{f.proxy}, f.servers...)
}

// haNetworkUp waits for both operator replicas to settle, then starts the
// network away from their nodes and waits until both servers are Ready and
// registered and the proxy is Ready.
func haNetworkUp(t *testing.T) *haFixture {
	t.Helper()
	haGated(t)
	game, proxy := os.Getenv("SPAWNERY_E2E_GAME_IMAGE"), os.Getenv("SPAWNERY_E2E_PROXY_IMAGE")
	if game == "" || proxy == "" {
		t.Fatalf("%s=1 needs SPAWNERY_E2E_GAME_IMAGE and SPAWNERY_E2E_PROXY_IMAGE, the images "+
			"hack/e2e-ha.sh loads; got %q and %q", haGate, game, proxy)
	}

	leader := operatorsSettled(t)
	var avoid []string
	for _, p := range operatorPods(t, operatorNamespace) {
		if !slices.Contains(avoid, p.Spec.NodeName) {
			avoid = append(avoid, p.Spec.NodeName)
		}
	}
	slices.Sort(avoid)
	affinity, err := json.Marshal(corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpNotIn, Values: avoid,
				}},
			}},
		},
	}})
	if err != nil {
		t.Fatalf("encode the affinity: %v", err)
	}

	t.Cleanup(func() { tearDownHANetwork(t) })
	applyDocuments(t, "the HA network", []byte(fmt.Sprintf(haManifest, game, proxy, affinity)))

	f := &haFixture{leader: leader}
	eventually(t, 8*time.Minute, "both servers Ready and registered, and the proxy Ready", func() (bool, string) {
		var list spawneryv1alpha1.ServerList
		if err := k8s.List(ctx, &list, client.InNamespace(haNamespace)); err != nil {
			return false, err.Error()
		}
		var servers, states []string
		for _, s := range list.Items {
			states = append(states, fmt.Sprintf("%s %s registered=%t", s.Name, s.Status.Phase, s.Status.Registered))
			if s.Status.Phase == string(phase.Ready) && s.Status.Registered {
				servers = append(servers, s.Name)
			}
		}
		if len(list.Items) != haServers || len(servers) != haServers {
			return false, strings.Join(states, "; ")
		}
		pods := groupPods(t, podspec.RoleProxy, haProxyGroup)
		if len(pods) != 1 || !podReady(&pods[0]) || !pods[0].DeletionTimestamp.IsZero() {
			return false, fmt.Sprintf("servers ready; %d proxy pod(s), none settled and Ready", len(pods))
		}
		f.servers, f.proxy = servers, pods[0].Name
		return true, ""
	})

	var pods corev1.PodList
	if err := k8s.List(ctx, &pods, client.InNamespace(haNamespace)); err != nil {
		t.Fatalf("list pods in %s: %v", haNamespace, err)
	}
	for _, p := range pods.Items {
		if slices.Contains(avoid, p.Spec.NodeName) {
			t.Fatalf("pod %s runs on %s, which also runs an operator replica: the group's affinity "+
				"did not keep it off %v, and a lost operator node would take it along", p.Name, p.Spec.NodeName, avoid)
		}
	}
	t.Logf("operator replicas on %v, leader %s on %s; the network on the other nodes",
		avoid, leader.Name, leader.Spec.NodeName)

	addr, stop := forwardPodPort(t, operatorNamespace, leader.Name, operatorMetricsPort)
	defer stop()
	eventually(t, 2*time.Minute, "every agent connected to the leader", func() (bool, string) {
		streams, err := openStreams(addr)
		if err != nil {
			return false, err.Error()
		}
		return streams["server"] == haServers && streams["proxy"] == 1, fmt.Sprintf("open streams %v", streams)
	})
	f.restarts = containerRestarts(t, f.pods())
	return f
}

func containerRestarts(t *testing.T, pods []string) map[string]int32 {
	t.Helper()
	counts := map[string]int32{}
	for _, name := range pods {
		var p corev1.Pod
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: haNamespace, Name: name}, &p); err != nil {
			t.Fatalf("get pod %s: %v", name, err)
		}
		for _, c := range p.Status.ContainerStatuses {
			counts[name+"/"+c.Name] = c.RestartCount
		}
	}
	return counts
}

// noRestarts fails if a container of the network's pods restarted since the
// network came up, and prints the end of the log it had before.
func noRestarts(t *testing.T, f *haFixture) {
	t.Helper()
	now := containerRestarts(t, f.pods())
	keys := slices.Sorted(maps.Keys(now))
	for _, key := range keys {
		before, n := f.restarts[key], now[key]
		if n == before {
			continue
		}
		pod, container, _ := strings.Cut(key, "/")
		previous, err := readPodLog(haNamespace, pod, &corev1.PodLogOptions{
			Container: container, Previous: true, Timestamps: true, TailLines: ptr.To[int64](60),
		})
		if err != nil {
			previous = fmt.Sprintf("(no previous log: %v)", err)
		}
		t.Errorf("container %s restarted: restart count %d, %d when the network came up; the end of "+
			"its previous log:\n%s", key, n, before, previous)
	}
}

func groupPods(t *testing.T, role, group string) []corev1.Pod {
	t.Helper()
	var pods corev1.PodList
	if err := k8s.List(ctx, &pods, client.InNamespace(haNamespace),
		client.MatchingLabels{podspec.LabelRole: role, podspec.LabelGroup: group}); err != nil {
		t.Fatalf("list %s pods of %s: %v", role, group, err)
	}
	return pods.Items
}

func tearDownHANetwork(t *testing.T) {
	t.Helper()
	objs := []client.Object{
		&spawneryv1alpha1.ProxyGroup{ObjectMeta: metav1.ObjectMeta{Namespace: haNamespace, Name: haProxyGroup}},
		&spawneryv1alpha1.ServerGroup{ObjectMeta: metav1.ObjectMeta{Namespace: haNamespace, Name: haServerGroup}},
		&spawneryv1alpha1.Network{ObjectMeta: metav1.ObjectMeta{Namespace: haNamespace, Name: "ha"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: haNamespace, Name: "velocity-forwarding-secret"}},
	}
	for _, o := range objs {
		if err := k8s.Delete(ctx, o); err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete %T %s: %v", o, o.GetName(), err)
		}
	}
	stop := time.Now().Add(5 * time.Minute)
	for {
		var servers spawneryv1alpha1.ServerList
		var pods corev1.PodList
		errS := k8s.List(ctx, &servers, client.InNamespace(haNamespace))
		errP := k8s.List(ctx, &pods, client.InNamespace(haNamespace))
		if errS == nil && errP == nil && len(servers.Items) == 0 && len(pods.Items) == 0 {
			return
		}
		if time.Now().After(stop) {
			t.Errorf("the HA network is not gone after 5m: %d server(s), %d pod(s) (%v, %v); the next "+
				"case would count them as its own", len(servers.Items), len(pods.Items), errS, errP)
			return
		}
		time.Sleep(time.Second)
	}
}

// operatorsSettled waits until the Deployment has rolled out, every replica
// is Ready and none is on its way out, and the slice names the leader.
func operatorsSettled(t *testing.T) *corev1.Pod {
	t.Helper()
	var leader *corev1.Pod
	eventually(t, 5*time.Minute, "the operator replicas to settle", func() (bool, string) {
		var d appsv1.Deployment
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: operatorNamespace, Name: "spawnery-operator"}, &d); err != nil {
			return false, err.Error()
		}
		want := ptr.Deref(d.Spec.Replicas, 1)
		st := d.Status
		if st.ObservedGeneration != d.Generation || st.Replicas != want || st.UpdatedReplicas != want ||
			st.ReadyReplicas != want || st.AvailableReplicas != want {
			return false, fmt.Sprintf("deployment status %+v, want %d of each", st, want)
		}
		pods := operatorPods(t, operatorNamespace)
		if int32(len(pods)) != want {
			return false, fmt.Sprintf("%d operator pods, want %d", len(pods), want)
		}
		for i := range pods {
			if !pods[i].DeletionTimestamp.IsZero() || !podReady(&pods[i]) {
				return false, pods[i].Name + " is terminating or not Ready"
			}
		}
		pod, holder, err := leaderPod(operatorNamespace)
		if err != nil {
			return false, err.Error()
		}
		if ok, detail := sliceNames(pod); !ok {
			return false, fmt.Sprintf("leader %s: %s", holder, detail)
		}
		leader = pod
		return true, ""
	})
	return leader
}

// sliceNames reports whether the agent Service's IPv4 slice names pod, and
// only pod.
func sliceNames(pod *corev1.Pod) (bool, string) {
	var s discoveryv1.EndpointSlice
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: operatorNamespace, Name: agentSlice}, &s); err != nil {
		return false, err.Error()
	}
	if len(s.Endpoints) != 1 {
		return false, fmt.Sprintf("slice %s has %d endpoints: %s", agentSlice, len(s.Endpoints), describeSlice(&s))
	}
	ep := s.Endpoints[0]
	if ep.TargetRef == nil || ep.TargetRef.UID != pod.UID || len(ep.Addresses) != 1 || ep.Addresses[0] != pod.Status.PodIP {
		return false, fmt.Sprintf("slice %s names %s, want %s (%s, %s)", agentSlice, describeSlice(&s),
			pod.Name, pod.UID, pod.Status.PodIP)
	}
	return true, ""
}

func describeSlice(s *discoveryv1.EndpointSlice) string {
	var eps []string
	for _, ep := range s.Endpoints {
		ref := "no targetRef"
		if ep.TargetRef != nil {
			ref = fmt.Sprintf("%s (%s)", ep.TargetRef.Name, ep.TargetRef.UID)
		}
		eps = append(eps, fmt.Sprintf("%v %s", ep.Addresses, ref))
	}
	return "[" + strings.Join(eps, ", ") + "]"
}

// serversStayReady polls every Server in namespace from before during until
// hold after it returns, and fails on any phase other than Ready, on a
// server that disappears, and on a watch that could not see: more than
// haBlindPolls failed polls in a row, or none that succeeded.
func serversStayReady(t *testing.T, namespace string, hold time.Duration, during func()) {
	t.Helper()
	var list spawneryv1alpha1.ServerList
	if err := k8s.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		t.Fatalf("list servers in %s: %v", namespace, err)
	}
	if len(list.Items) == 0 {
		t.Fatalf("no server in %s to watch", namespace)
	}
	var watched []string
	for _, s := range list.Items {
		watched = append(watched, s.Name)
	}

	start := time.Now()
	var mu sync.Mutex
	var trouble []string
	seen := map[string]bool{}
	var succeeded, failedInARow int
	var blind []string
	note := func(what string) {
		mu.Lock()
		defer mu.Unlock()
		if !seen[what] {
			seen[what] = true
			trouble = append(trouble, fmt.Sprintf("+%.1fs %s", time.Since(start).Seconds(), what))
		}
	}
	observe := func() {
		var now spawneryv1alpha1.ServerList
		listCtx, cancel := context.WithTimeout(ctx, haPollTimeout)
		err := k8s.List(listCtx, &now, client.InNamespace(namespace))
		cancel()
		mu.Lock()
		if err != nil {
			failedInARow++
			if failedInARow == haBlindPolls+1 {
				blind = append(blind, fmt.Sprintf("+%.1fs %d polls in a row failed, the last with: %v",
					time.Since(start).Seconds(), failedInARow, err))
			}
			mu.Unlock()
			return
		}
		succeeded++
		failedInARow = 0
		mu.Unlock()
		phases := map[string]string{}
		for _, s := range now.Items {
			phases[s.Name] = s.Status.Phase
		}
		for _, name := range watched {
			got, ok := phases[name]
			switch {
			case !ok:
				note(name + " is gone")
			case got != string(phase.Ready):
				note(fmt.Sprintf("%s is %s", name, got))
			}
		}
	}

	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		for {
			observe()
			select {
			case <-stop:
				return
			case <-tick.C:
			}
		}
	}()
	defer func() {
		close(stop)
		<-done
		mu.Lock()
		defer mu.Unlock()
		if len(trouble) > 0 {
			t.Errorf("servers left Ready (times from before the disruption):\n%s", strings.Join(trouble, "\n"))
		}
		if len(blind) > 0 {
			t.Errorf("the watch on the servers went blind (times from before the disruption):\n%s",
				strings.Join(blind, "\n"))
		}
		if succeeded == 0 {
			t.Errorf("no poll of the servers in %s succeeded, so nothing says they stayed Ready", namespace)
		}
	}()

	during()
	time.Sleep(hold)
}

type leaseChange struct {
	at     time.Time
	holder string
	// prevRenewed is the previous holder's last renewTime.
	prevRenewed time.Time
}

type leaseWatch struct {
	mu      sync.Mutex
	changes []leaseChange
	stop    chan struct{}
	done    chan struct{}
}

func watchLease() *leaseWatch {
	w := &leaseWatch{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		last := "\x00"
		var renewed time.Time
		for {
			var lease coordinationv1.Lease
			if err := k8s.Get(ctx, client.ObjectKey{Namespace: operatorNamespace, Name: operatorLease}, &lease); err == nil {
				holder := ptr.Deref(lease.Spec.HolderIdentity, "")
				if holder != last {
					w.mu.Lock()
					w.changes = append(w.changes, leaseChange{at: time.Now(), holder: holder, prevRenewed: renewed})
					w.mu.Unlock()
					last = holder
				}
				if lease.Spec.RenewTime != nil {
					renewed = lease.Spec.RenewTime.Time
				}
			}
			select {
			case <-w.stop:
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}()
	return w
}

func (w *leaseWatch) snapshot() []leaseChange {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.changes)
}

func (w *leaseWatch) close() []leaseChange {
	close(w.stop)
	<-w.done
	return w.snapshot()
}

func secondsSince(start, at time.Time) string {
	return fmt.Sprintf("%+.1fs", at.Sub(start).Seconds())
}

type agentReturn struct {
	first, servers, proxy time.Duration
}

type handoverResult struct {
	next     *corev1.Pod
	takeover time.Duration
	slice    time.Duration
	agents   agentReturn
}

// handover runs act with every server watched, then waits for another pod to
// lead, for the slice to name it and for every agent to reconnect to it
// within ReconnectGrace of the last lease change. Afterwards no proxy may
// have dropped a backend, and the slice still names the new leader.
func handover(t *testing.T, f *haFixture, act func()) handoverResult {
	t.Helper()
	var r handoverResult
	start := time.Now()
	lease := watchLease()
	defer func() {
		for _, c := range lease.close() {
			prev := "never"
			if !c.prevRenewed.IsZero() {
				prev = secondsSince(start, c.prevRenewed)
			}
			t.Logf("lease: %s held by %q (the previous holder last renewed at %s)",
				secondsSince(start, c.at), c.holder, prev)
		}
	}()

	defer logAgentLines(t, f, start)

	serversStayReady(t, haNamespace, haHold, func() {
		act()
		r.next, r.takeover = awaitNewLeader(t, f.leader, start, time.Now(), 2*time.Minute)
		t.Logf("%s leads, %.1fs after the disruption began", r.next.Name, r.takeover.Seconds())
		r.slice = awaitSlice(t, r.next, start, start.Add(r.takeover), time.Minute)
		t.Logf("the slice names %s, %.1fs after the disruption began", r.next.Name, r.slice.Seconds())

		led := start.Add(r.takeover)
		if changes := lease.snapshot(); len(changes) > 0 && changes[len(changes)-1].at.After(led) {
			led = changes[len(changes)-1].at
		}
		r.agents = awaitAgentsBack(t, f, r.next, start, led.Add(phase.ReconnectGrace))
		t.Logf("agents back on %s: first stream %.1fs, both servers %.1fs, the proxy %.1fs after the "+
			"disruption began (the lease last changed at %s; ReconnectGrace is %s)",
			r.next.Name, r.agents.first.Seconds(), r.agents.servers.Seconds(), r.agents.proxy.Seconds(),
			secondsSince(start, led), phase.ReconnectGrace)
	})

	noRestarts(t, f)
	log, err := readPodLog(haNamespace, f.proxy, &corev1.PodLogOptions{Container: podspec.ProxyContainerName})
	if err != nil {
		t.Errorf("read the proxy's log: %v", err)
	} else if unregisteredBackend.MatchString(log) {
		t.Errorf("the proxy dropped a backend during the handover:\n%s",
			strings.Join(linesMatching(log, unregisteredBackend), "\n"))
	}

	now, _, err := leaderPod(operatorNamespace)
	if err != nil {
		t.Errorf("after the hold: %v", err)
	} else if now.UID != r.next.UID {
		t.Errorf("after the hold %s leads, not %s, which took over", now.Name, r.next.Name)
	}
	if ok, detail := sliceNames(r.next); !ok {
		t.Errorf("after the hold: %s", detail)
	}
	return r
}

func linesMatching(log string, pattern *regexp.Regexp) []string {
	var found []string
	for _, line := range strings.Split(log, "\n") {
		if pattern.MatchString(line) {
			found = append(found, line)
		}
	}
	return found
}

// awaitNewLeader and awaitSlice report times since start but count their
// budget from from, because the rollout case's act returns only after
// rollout status does.
func awaitNewLeader(t *testing.T, old *corev1.Pod, start, from time.Time, within time.Duration) (*corev1.Pod, time.Duration) {
	t.Helper()
	for {
		pod, holder, err := leaderPod(operatorNamespace)
		var last string
		switch {
		case err != nil:
			last = err.Error()
		case pod.UID == old.UID:
			last = "still held by " + holder
		case !pod.DeletionTimestamp.IsZero():
			// A rollout's old pods can lead for a while once rollout status
			// has already returned.
			last = "held by " + holder + ", which is terminating"
		default:
			return pod, time.Since(start)
		}
		if time.Since(from) >= within {
			t.Fatalf("no pod other than %s took the lease within %s (%.1fs after the disruption began): %s",
				old.Name, within, time.Since(start).Seconds(), last)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func awaitSlice(t *testing.T, pod *corev1.Pod, start, from time.Time, within time.Duration) time.Duration {
	t.Helper()
	for {
		ok, detail := sliceNames(pod)
		if ok {
			return time.Since(start)
		}
		if time.Since(from) >= within {
			t.Fatalf("the slice did not name the new leader %s within %s of the takeover (%.1fs after "+
				"the disruption began): %s", pod.Name, within, time.Since(start).Seconds(), detail)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// awaitAgentsBack fails if not every agent is back on leader by deadline, but
// first keeps watching for two more minutes, so that the failure says when
// they did come back, if at all.
func awaitAgentsBack(t *testing.T, f *haFixture, leader *corev1.Pod, start, deadline time.Time) agentReturn {
	t.Helper()
	addr, stop := forwardPodPort(t, operatorNamespace, leader.Name, operatorMetricsPort)
	defer stop()
	var r agentReturn
	last := "nothing observed yet"
	late := false
	for {
		streams, err := openStreams(addr)
		if err != nil {
			last = err.Error()
		} else {
			last = fmt.Sprintf("open streams %v", streams)
			if r.first == 0 && streams["server"]+streams["proxy"] > 0 {
				r.first = time.Since(start)
			}
			if r.servers == 0 && streams["server"] >= haServers {
				r.servers = time.Since(start)
			}
			if r.proxy == 0 && streams["proxy"] >= 1 {
				r.proxy = time.Since(start)
			}
			if r.servers != 0 && r.proxy != 0 {
				break
			}
		}
		if !late && time.Now().After(deadline) {
			late = true
			t.Errorf("not every agent reconnected to %s within ReconnectGrace (%s after the last lease "+
				"change, %.1fs after the disruption began): %s", leader.Name, phase.ReconnectGrace,
				deadline.Sub(start).Seconds(), last)
			logAgentConnections(t, f)
		}
		if time.Now().After(deadline.Add(2 * time.Minute)) {
			t.Fatalf("still not every agent back on %s %.1fs after the disruption began: %s",
				leader.Name, time.Since(start).Seconds(), last)
		}
		time.Sleep(250 * time.Millisecond)
	}
	if late {
		t.Errorf("the agents came back late: first stream %.1fs, both servers %.1fs, the proxy %.1fs "+
			"after the disruption began", r.first.Seconds(), r.servers.Seconds(), r.proxy.Seconds())
	}
	return r
}

// logAgentConnections prints the agent Service's connection-tracking entries
// on the nodes of the network's pods. A pod's socket names the Service's
// ClusterIP, so only the node's conntrack table shows which operator pod a
// stream, or a connect attempt, actually went to.
func logAgentConnections(t *testing.T, f *haFixture) {
	t.Helper()
	var svc corev1.Service
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: operatorNamespace, Name: podspec.AgentServiceName}, &svc); err != nil {
		t.Logf("conntrack: get Service %s: %v", podspec.AgentServiceName, err)
		return
	}
	var pods []string
	for _, p := range operatorPods(t, operatorNamespace) {
		pods = append(pods, fmt.Sprintf("%s=%s", p.Name, p.Status.PodIP))
	}
	t.Logf("conntrack: Service %s is %s; operator pods %v", svc.Name, svc.Spec.ClusterIP, pods)
	nodes := map[string]bool{}
	for _, name := range f.pods() {
		var p corev1.Pod
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: haNamespace, Name: name}, &p); err == nil {
			nodes[p.Spec.NodeName] = true
		}
	}
	for node := range nodes {
		out, err := exec.Command(containerRuntime(), "exec", node,
			"conntrack", "-L", "-p", "tcp", "--orig-dst", svc.Spec.ClusterIP).CombinedOutput()
		t.Logf("conntrack on %s (%v):\n%s", node, err, out)
	}
}

func containerRuntime() string {
	if os.Getenv("KIND_EXPERIMENTAL_PROVIDER") == "podman" {
		return "podman"
	}
	return "docker"
}

// logAgentLines prints what each agent said about its operator stream since
// start, with the kubelet's timestamps: when it noticed, and every redial.
func logAgentLines(t *testing.T, f *haFixture, start time.Time) {
	t.Helper()
	since := metav1.NewTime(start)
	pods := map[string]string{f.proxy: podspec.ProxyContainerName}
	for _, s := range f.servers {
		pods[s] = podspec.ContainerName
	}
	for pod, container := range pods {
		log, err := readPodLog(haNamespace, pod, &corev1.PodLogOptions{
			Container: container, Timestamps: true, SinceTime: &since,
		})
		if err != nil {
			t.Logf("%s: no log (%v)", pod, err)
			continue
		}
		for _, line := range strings.Split(log, "\n") {
			if !strings.Contains(strings.ToLower(line), "spawnery") && !strings.Contains(line, "operator") {
				continue
			}
			stamp, rest, _ := strings.Cut(line, " ")
			if trimmed := strings.TrimSpace(rest); strings.HasPrefix(trimmed, "at ") || strings.HasPrefix(trimmed, "... ") {
				continue
			}
			if at, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
				stamp = secondsSince(start, at)
			}
			t.Logf("%s %s: %s", stamp, pod, rest)
		}
	}
}

func scrape(addr string) (string, error) {
	c := http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get("http://" + addr + "/metrics")
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET /metrics: %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

func openStreams(addr string) (map[string]int, error) {
	body, err := scrape(addr)
	if err != nil {
		return nil, err
	}
	streams := map[string]int{}
	for _, m := range openStreamsLine.FindAllStringSubmatch(body, -1) {
		v, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			return nil, fmt.Errorf("spawnery_agent_open_streams{role=%q}: %w", m[1], err)
		}
		streams[m[1]] = int(v)
	}
	return streams, nil
}

// TestHAUpgradeFromASelectorServiceLeavesOnlyTheLeadersSlice reads the state
// hack/e2e-ha.sh's upgrade from the released chart left behind: the slices
// the EndpointSlice controller wrote while the Service had a selector are
// gone, so is the Endpoints object, and the leader's slice is the only one.
// It must run before any other case moves the leader.
func TestHAUpgradeFromASelectorServiceLeavesOnlyTheLeadersSlice(t *testing.T) {
	haGated(t)
	at, err := strconv.ParseInt(os.Getenv("SPAWNERY_E2E_HA_UPGRADED_AT"), 10, 64)
	if err != nil {
		t.Fatalf("SPAWNERY_E2E_HA_UPGRADED_AT: %v; hack/e2e-ha.sh sets it to when helm upgrade returned", err)
	}
	stale := strings.Fields(os.Getenv("SPAWNERY_E2E_HA_STALE_SLICES"))
	if len(stale) == 0 {
		t.Fatalf("SPAWNERY_E2E_HA_STALE_SLICES is empty: without the slices the released chart's " +
			"selector produced, this case has nothing to see removed")
	}
	upgraded := time.Unix(at, 0)
	// One assert period (30 s) and the takeover after the old leader's lease.
	deadline := upgraded.Add(40 * time.Second)
	if time.Now().After(deadline) {
		t.Fatalf("this case started %s after the upgrade returned, past the 40 s it checks; "+
			"whatever it saw now would say nothing about how soon the stale slices went",
			time.Since(upgraded).Round(time.Second))
	}

	last := "nothing observed yet"
	for time.Now().Before(deadline) {
		ok, detail := onlyTheLeadersSlice(stale)
		if ok {
			t.Logf("only the leader's slice and no Endpoints object, %.0fs after helm upgrade returned",
				time.Since(upgraded).Seconds())
			return
		}
		last = detail
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("40 s after the upgrade from the released chart: %s", last)
}

func onlyTheLeadersSlice(stale []string) (bool, string) {
	leader, _, err := leaderPod(operatorNamespace)
	if err != nil {
		return false, err.Error()
	}
	var svc corev1.Service
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: operatorNamespace, Name: podspec.AgentServiceName}, &svc); err != nil {
		return false, err.Error()
	}
	if len(svc.Spec.Selector) > 0 {
		return false, fmt.Sprintf("Service %s still has the selector %v", svc.Name, svc.Spec.Selector)
	}
	var list discoveryv1.EndpointSliceList
	if err := k8s.List(ctx, &list, client.InNamespace(operatorNamespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: podspec.AgentServiceName}); err != nil {
		return false, err.Error()
	}
	var names []string
	for _, s := range list.Items {
		names = append(names, fmt.Sprintf("%s (managed by %q) %s", s.Name, s.Labels[discoveryv1.LabelManagedBy], describeSlice(&s)))
		if slices.Contains(stale, s.Name) {
			return false, fmt.Sprintf("the released chart's slice %s is still there: %v", s.Name, names)
		}
	}
	if len(list.Items) != 1 || list.Items[0].Name != agentSlice ||
		list.Items[0].Labels[discoveryv1.LabelManagedBy] != agentendpoint.ManagedBy {
		return false, fmt.Sprintf("slices of %s: %v, want %s alone", podspec.AgentServiceName, names, agentSlice)
	}
	if ok, detail := sliceNames(leader); !ok {
		return false, detail
	}
	ep := &metav1.PartialObjectMetadata{}
	ep.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Endpoints"))
	err = k8s.Get(ctx, client.ObjectKey{Namespace: operatorNamespace, Name: podspec.AgentServiceName}, ep)
	switch {
	case apierrors.IsNotFound(err):
		return true, ""
	case err != nil:
		return false, err.Error()
	default:
		return false, fmt.Sprintf("Endpoints/%s still exists, so the mirroring controller would "+
			"write it back as a slice", podspec.AgentServiceName)
	}
}

// TestHAStandbyExportsNoLeaderOnlySeries scrapes both replicas. The leader's
// certificate series are the control: without them the standby's silence
// would prove nothing.
func TestHAStandbyExportsNoLeaderOnlySeries(t *testing.T) {
	haGated(t)
	leader := operatorsSettled(t)
	var standby *corev1.Pod
	pods := operatorPods(t, operatorNamespace)
	for i := range pods {
		if pods[i].UID != leader.UID {
			standby = &pods[i]
		}
	}
	if standby == nil {
		t.Fatalf("no standby beside the leader %s", leader.Name)
	}

	leaderOnly := []string{"spawnery_ca_expiry_timestamp_seconds", "spawnery_serving_cert_expiry_timestamp_seconds"}
	status := func(v string) *regexp.Regexp {
		return regexp.MustCompile(`(?m)^leader_election_master_status\{name="` +
			regexp.QuoteMeta(operatorLease) + `"\} ` + v + `$`)
	}

	read := func(pod *corev1.Pod) string {
		addr, stop := forwardPodPort(t, operatorNamespace, pod.Name, operatorMetricsPort)
		defer stop()
		body, err := scrape(addr)
		if err != nil {
			t.Fatalf("scrape %s: %v", pod.Name, err)
		}
		return body
	}

	body := read(leader)
	for _, name := range leaderOnly {
		if !strings.Contains(body, name) {
			t.Fatalf("the leader %s exports no %s, so its absence on the standby would prove nothing", leader.Name, name)
		}
	}
	if !status("1").MatchString(body) {
		t.Errorf("the leader %s does not report leader_election_master_status 1", leader.Name)
	}

	body = read(standby)
	if !status("0").MatchString(body) {
		t.Errorf("the standby %s does not report leader_election_master_status 0; lines: %v", standby.Name,
			linesMatching(body, regexp.MustCompile(`leader_election_master_status`)))
	}
	for _, name := range leaderOnly {
		if strings.Contains(body, name) {
			t.Errorf("the standby %s exports %s:\n%s", standby.Name, name,
				strings.Join(linesMatching(body, regexp.MustCompile(regexp.QuoteMeta(name))), "\n"))
		}
	}
}

func TestHAPlannedHandoverKeepsServersReady(t *testing.T) {
	f := haNetworkUp(t)
	r := handover(t, f, func() {
		kubectl(t, "-n", operatorNamespace, "delete", "pod", f.leader.Name, "--wait=false")
	})
	// Unreleased, the lease would pass only after its 15 s duration from
	// the last renewal, which went on through the graceful shutdown.
	if r.takeover >= 15*time.Second {
		t.Errorf("the planned handover took %.1fs: the old leader's lease release did not land "+
			"before SIGKILL", r.takeover.Seconds())
	}
}

func TestHACrashedLeaderKeepsServersReady(t *testing.T) {
	f := haNetworkUp(t)
	handover(t, f, func() {
		kubectl(t, "-n", operatorNamespace, "delete", "pod", f.leader.Name, "--grace-period=0", "--force", "--wait=false")
	})
}

func TestHARolloutKeepsServersReady(t *testing.T) {
	f := haNetworkUp(t)
	handover(t, f, func() {
		kubectl(t, "-n", operatorNamespace, "rollout", "restart", "deployment/spawnery-operator")
		kubectl(t, "-n", operatorNamespace, "rollout", "status", "deployment/spawnery-operator", "--timeout=5m")
		out, err := exec.Command("helm", "upgrade", "spawnery", repoRoot+"/charts/spawnery",
			"--namespace", operatorNamespace, "--reuse-values", "--wait", "--timeout", "5m").CombinedOutput()
		if err != nil {
			t.Fatalf("helm upgrade --wait with two replicas: %v\n%s", err, out)
		}
	})
}

func TestHALostLeaderNodeKeepsServersReady(t *testing.T) {
	f := haNetworkUp(t)
	node := f.leader.Spec.NodeName
	for _, p := range operatorPods(t, operatorNamespace) {
		if p.UID != f.leader.UID && p.Spec.NodeName == node {
			t.Fatalf("the standby %s runs on the leader's node %s: the chart's spread did not "+
				"separate them, and losing that node loses both", p.Name, node)
		}
	}

	runtime := containerRuntime()
	paused := false
	unpause := func() error {
		out, err := exec.Command(runtime, "unpause", node).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s unpause %s: %v\n%s", runtime, node, err, out)
		}
		paused = false
		return nil
	}
	t.Cleanup(func() {
		if paused {
			if err := unpause(); err != nil {
				t.Error(err)
			}
		}
	})

	r := handover(t, f, func() {
		pod, holder, err := leaderPod(operatorNamespace)
		switch {
		case err != nil:
			t.Fatalf("before pausing %s: %v", node, err)
		case pod.UID != f.leader.UID:
			t.Fatalf("before pausing %s: %q holds the lease, not %s on that node, so the pause "+
				"would freeze a standby and leave the leader running", node, holder, f.leader.Name)
		}
		if out, err := exec.Command(runtime, "pause", node).CombinedOutput(); err != nil {
			t.Fatalf("%s pause %s: %v\n%s", runtime, node, err, out)
		}
		paused = true
	})
	t.Logf("node %s paused: %s led after %.1fs, the first agent was back after %.1fs, the last after %.1fs",
		node, r.next.Name, r.takeover.Seconds(), r.agents.first.Seconds(),
		max(r.agents.servers, r.agents.proxy).Seconds())

	// The old leader resumes believing it still leads, until its next renewal
	// fails: neither the slice nor any server may notice.
	serversStayReady(t, haNamespace, 0, func() {
		if err := unpause(); err != nil {
			t.Fatal(err)
		}
		resumed := time.Now()
		for time.Since(resumed) < time.Minute {
			if ok, detail := sliceNames(r.next); !ok {
				t.Fatalf("%.1fs after %s came back, the slice no longer names the leader %s: %s",
					time.Since(resumed).Seconds(), node, r.next.Name, detail)
			}
			time.Sleep(250 * time.Millisecond)
		}
	})
	noRestarts(t, f)
}

func kubectl(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("kubectl", args...).CombinedOutput(); err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
