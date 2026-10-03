//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
)

const (
	tutorialNamespace   = "spawnery-tutorial"
	tutorialManifest    = "docs/tutorial/network.yaml"
	tutorialServerGroup = "lobby"
	tutorialProxyGroup  = "gateway"

	// tutorialJoinPort is docs/tutorial/network.yaml's fixed NodePort, mapped
	// to the host by docs/tutorial/kind-config.yaml.
	tutorialJoinPort = 30001

	// tutorialOperatorNamespace is the chart's default, as README.md installs
	// it; waits in this file pass it to eventuallyIn.
	tutorialOperatorNamespace = "spawnery-system"
)

// TestTutorialPath drives the tutorial's own path once, join included: apply
// docs/tutorial/network.yaml, wait for a Ready backend and an addressable
// proxy, then join with cmd/spawnery-join --hold so the proxy's
// status.connectedPlayers is non-zero when the assertion reads it.
//
// It needs real images, so it runs under hack/e2e-tutorial.sh
// (SPAWNERY_E2E_TUTORIAL=1, nightly), not hack/e2e.sh.
func TestTutorialPath(t *testing.T) {
	if os.Getenv("SPAWNERY_E2E_TUTORIAL") != "1" {
		t.Skip("set SPAWNERY_E2E_TUTORIAL=1 to run the tutorial's own path; " +
			"hack/e2e-tutorial.sh does this nightly, hack/e2e.sh does not")
	}

	applyManifest(t, tutorialManifest)

	eventuallyIn(t, tutorialOperatorNamespace, 3*time.Minute, "the lobby ServerGroup to report a Ready backend", func() (bool, string) {
		var group spawneryv1alpha1.ServerGroup
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: tutorialNamespace, Name: tutorialServerGroup}, &group); err != nil {
			return false, err.Error()
		}
		return group.Status.ReadyReplicas >= 1, fmt.Sprintf("readyReplicas=%d", group.Status.ReadyReplicas)
	})

	eventuallyIn(t, tutorialOperatorNamespace, 2*time.Minute, "the gateway ProxyGroup to be addressable", func() (bool, string) {
		var group spawneryv1alpha1.ProxyGroup
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: tutorialNamespace, Name: tutorialProxyGroup}, &group); err != nil {
			return false, err.Error()
		}
		return group.Status.Address != "", fmt.Sprintf("status.address=%q", group.Status.Address)
	})

	joinPath, err := exec.LookPath("spawnery-join")
	if err != nil {
		t.Fatalf("spawnery-join not on PATH (%v); the dev shell carries it, run this through nix develop", err)
	}

	// The wait below must land inside the hold. connectedPlayers is refreshed
	// only on reconcile, so hold clears at least two 5 s resyncs with room
	// for a slow cluster.
	const hold = 25 * time.Second

	// Only as far above hold as spawnery-join's "--hold must fit inside
	// --timeout" check needs.
	timeout := hold + 5*time.Second
	cmd := exec.Command(joinPath,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(tutorialJoinPort),
		"--timeout", timeout.String(),
		"--hold", hold.String(),
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start spawnery-join: %v", err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	// Not eventuallyIn: racing the process exit reports a failed join in its
	// own words rather than as a generic timeout.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	joinDeadline := hold - 3*time.Second
	deadline := time.After(joinDeadline)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	last := "nothing observed yet"
	for {
		select {
		case err := <-done:
			t.Fatalf("spawnery-join exited before the gateway ProxyGroup reported the joined player (%v):\n%s", err, out.String())
		case <-deadline:
			t.Fatalf("timed out after %s waiting for the gateway ProxyGroup to report the joined player; last seen: %s%s",
				joinDeadline, last, denialHint(t, tutorialOperatorNamespace))
		case <-ticker.C:
			var group spawneryv1alpha1.ProxyGroup
			if err := k8s.Get(ctx, client.ObjectKey{Namespace: tutorialNamespace, Name: tutorialProxyGroup}, &group); err != nil {
				last = err.Error()
				continue
			}
			last = fmt.Sprintf("connectedPlayers=%d", group.Status.ConnectedPlayers)
			if group.Status.ConnectedPlayers >= 1 {
				if err := <-done; err != nil {
					t.Fatalf("spawnery-join: %v\n%s", err, out.String())
				}
				t.Logf("spawnery-join: %s", out.String())
				return
			}
		}
	}
}

// TestTutorialPlayableSlots runs after TestTutorialPath on the same network:
// with one playable seat per server and three kept spare, the group grows to
// its maxReplicas of three, where two twenty-seat servers had room to spare.
//
// No join: spawnery-join stops in the configuration state, and a backend
// counts a player only once it reaches play, so a held join never shows up
// in a server's report.
func TestTutorialPlayableSlots(t *testing.T) {
	if os.Getenv("SPAWNERY_E2E_TUTORIAL") != "1" {
		t.Skip("set SPAWNERY_E2E_TUTORIAL=1; hack/e2e-tutorial.sh does this nightly")
	}

	key := client.ObjectKey{Namespace: tutorialNamespace, Name: tutorialServerGroup}
	var g spawneryv1alpha1.ServerGroup
	if err := k8s.Get(ctx, key, &g); err != nil {
		t.Fatalf("get ServerGroup: %v", err)
	}
	restore := g.DeepCopy()
	patch := client.MergeFrom(g.DeepCopy())
	g.Spec.PlayableSlots = ptr.To[int32](1)
	g.Spec.Scaling.SpareSlots = 3
	if err := k8s.Patch(ctx, &g, patch); err != nil {
		t.Fatalf("patch ServerGroup: %v", err)
	}
	t.Cleanup(func() {
		var now spawneryv1alpha1.ServerGroup
		if err := k8s.Get(ctx, key, &now); err != nil {
			return
		}
		back := client.MergeFrom(now.DeepCopy())
		now.Spec.PlayableSlots = nil
		now.Spec.Scaling.SpareSlots = restore.Spec.Scaling.SpareSlots
		_ = k8s.Patch(ctx, &now, back)
	})

	eventuallyIn(t, tutorialOperatorNamespace, 3*time.Minute, "three Ready servers with one playable seat each", func() (bool, string) {
		var list spawneryv1alpha1.ServerList
		if err := k8s.List(ctx, &list, client.InNamespace(tutorialNamespace)); err != nil {
			return false, err.Error()
		}
		ready, seen := 0, ""
		for _, s := range list.Items {
			if s.Spec.GroupRef.Name != tutorialServerGroup || s.Status.Phase == string(phase.Failed) {
				continue
			}
			seen += fmt.Sprintf(" %s=%s %d/%d playable %d;", s.Name, s.Status.Phase,
				s.Status.Players, s.Status.Slots, s.Status.PlayableSlots)
			if s.Status.Phase == string(phase.Ready) && s.Status.PlayableSlots == 1 && s.Status.Slots > 1 {
				ready++
			}
		}
		return ready == 3, "servers:" + seen
	})

	eventuallyIn(t, tutorialOperatorNamespace, time.Minute, "status.freeSlots to count playable seats", func() (bool, string) {
		var now spawneryv1alpha1.ServerGroup
		if err := k8s.Get(ctx, key, &now); err != nil {
			return false, err.Error()
		}
		return now.Status.FreeSlots == 3, fmt.Sprintf("status.freeSlots=%d", now.Status.FreeSlots)
	})
}

// TestTutorialChangeoverStages puts the gateway a stage ahead of the lobby
// and changes both: the lobby's first new server may only come once the
// gateway's new proxy stands.
//
// It starts from one lobby server: at its ceiling the lobby could not surge,
// and would hold back for that reason rather than for the gateway.
//
// The lobby is changed only after the gateway reports its changeover: a
// sibling's status.changeover is one status write behind its reconcile, and
// the design accepts that a later stage which saw it before that write begins
// at once.
func TestTutorialChangeoverStages(t *testing.T) {
	if os.Getenv("SPAWNERY_E2E_TUTORIAL") != "1" {
		t.Skip("set SPAWNERY_E2E_TUTORIAL=1; hack/e2e-tutorial.sh does this nightly")
	}

	applyManifest(t, tutorialManifest)

	lobbyKey := client.ObjectKey{Namespace: tutorialNamespace, Name: tutorialServerGroup}
	gatewayKey := client.ObjectKey{Namespace: tutorialNamespace, Name: tutorialProxyGroup}

	eventuallyIn(t, tutorialOperatorNamespace, 8*time.Minute, "one lobby server, room under its ceiling, and no changeover under way", func() (bool, string) {
		var lobby spawneryv1alpha1.ServerGroup
		if err := k8s.Get(ctx, lobbyKey, &lobby); err != nil {
			return false, err.Error()
		}
		var gateway spawneryv1alpha1.ProxyGroup
		if err := k8s.Get(ctx, gatewayKey, &gateway); err != nil {
			return false, err.Error()
		}
		return lobby.Status.Replicas == 1 && lobby.Status.ReadyReplicas == 1 && gateway.Status.ReadyReplicas >= 1 &&
				lobby.Status.Changeover == "" && gateway.Status.Changeover == "",
			fmt.Sprintf("lobby replicas=%d ready=%d changeover=%q; gateway ready=%d changeover=%q",
				lobby.Status.Replicas, lobby.Status.ReadyReplicas, lobby.Status.Changeover,
				gateway.Status.ReadyReplicas, gateway.Status.Changeover)
	})

	oldHashes := map[string]bool{}
	var servers spawneryv1alpha1.ServerList
	if err := k8s.List(ctx, &servers, client.InNamespace(tutorialNamespace)); err != nil {
		t.Fatalf("list servers: %v", err)
	}
	for _, s := range servers.Items {
		if s.Spec.GroupRef.Name == tutorialServerGroup {
			oldHashes[s.Spec.PodHash] = true
		}
	}

	probe := corev1.EnvVar{Name: "STAGE_PROBE", Value: strconv.FormatInt(time.Now().UnixNano(), 10)}
	start := time.Now()
	since := func(at time.Time) string { return at.Sub(start).Round(time.Second).String() }

	var gateway spawneryv1alpha1.ProxyGroup
	if err := k8s.Get(ctx, gatewayKey, &gateway); err != nil {
		t.Fatalf("get ProxyGroup: %v", err)
	}
	gatewayPatch := client.MergeFrom(gateway.DeepCopy())
	gateway.Spec.ChangeoverStage = -10
	gateway.Spec.Env = append(gateway.Spec.Env, probe)
	if err := k8s.Patch(ctx, &gateway, gatewayPatch); err != nil {
		t.Fatalf("patch ProxyGroup: %v", err)
	}
	t.Logf("+%s gateway patched: changeoverStage -10, %s=%s", since(time.Now()), probe.Name, probe.Value)

	eventuallyIn(t, tutorialOperatorNamespace, time.Minute, "the gateway to report its changeover", func() (bool, string) {
		var now spawneryv1alpha1.ProxyGroup
		if err := k8s.Get(ctx, gatewayKey, &now); err != nil {
			return false, err.Error()
		}
		c := now.Status.Changeover
		return c == spawneryv1alpha1.ChangeoverWaiting || c == spawneryv1alpha1.ChangeoverBegun,
			fmt.Sprintf("status.changeover=%q", c)
	})
	t.Logf("+%s gateway in flight", since(time.Now()))

	var lobby spawneryv1alpha1.ServerGroup
	if err := k8s.Get(ctx, lobbyKey, &lobby); err != nil {
		t.Fatalf("get ServerGroup: %v", err)
	}
	lobbyPatch := client.MergeFrom(lobby.DeepCopy())
	lobby.Spec.Env = append(lobby.Spec.Env, probe)
	if err := k8s.Patch(ctx, &lobby, lobbyPatch); err != nil {
		t.Fatalf("patch ServerGroup: %v", err)
	}
	lobbyGeneration := lobby.Generation
	t.Logf("+%s lobby patched: %s=%s (generation %d)", since(time.Now()), probe.Name, probe.Value, lobbyGeneration)

	var proxyStood, lobbyBegan, firstGated time.Time
	var lobbyBeganName, lastSeen string
	var violations []string
	deadline := time.Now().Add(10 * time.Minute)
	for ; time.Now().Before(deadline); time.Sleep(time.Second) {
		var g spawneryv1alpha1.ProxyGroup
		if err := k8s.Get(ctx, gatewayKey, &g); err != nil {
			continue
		}
		var l spawneryv1alpha1.ServerGroup
		if err := k8s.Get(ctx, lobbyKey, &l); err != nil {
			continue
		}
		var list spawneryv1alpha1.ServerList
		if err := k8s.List(ctx, &list, client.InNamespace(tutorialNamespace)); err != nil {
			continue
		}

		if proxyStood.IsZero() && (g.Status.Changeover == spawneryv1alpha1.ChangeoverDeferred || g.Status.Changeover == spawneryv1alpha1.ChangeoverNone) {
			proxyStood = time.Now()
			t.Logf("+%s gateway stands: status.changeover=%q readyReplicas=%d", since(proxyStood), g.Status.Changeover, g.Status.ReadyReplicas)
		}

		if lobbyBegan.IsZero() {
			for _, s := range list.Items {
				if s.Spec.GroupRef.Name != tutorialServerGroup || oldHashes[s.Spec.PodHash] || s.CreationTimestamp.Time.Before(start.Truncate(time.Second)) {
					continue
				}
				if lobbyBegan.IsZero() || s.CreationTimestamp.Time.Before(lobbyBegan) {
					lobbyBegan, lobbyBeganName = s.CreationTimestamp.Time, s.Name
				}
			}
			if !lobbyBegan.IsZero() {
				t.Logf("+%s lobby's first new server %s (created +%s)", since(time.Now()), lobbyBeganName, since(lobbyBegan))
			}
		}

		progressing := meta.FindStatusCondition(l.Status.Conditions, spawneryv1alpha1.ConditionProgressing)
		reason, message := "", ""
		if progressing != nil {
			reason, message = progressing.Reason, progressing.Message
		}
		if seen := fmt.Sprintf("gateway %q ready=%d; lobby %q replicas=%d ready=%d, Progressing %s: %s",
			g.Status.Changeover, g.Status.ReadyReplicas, l.Status.Changeover,
			l.Status.Replicas, l.Status.ReadyReplicas, reason, message); seen != lastSeen {
			lastSeen = seen
			t.Logf("+%s %s", since(time.Now()), seen)
		}
		gatewayInFlight := g.Status.Changeover == spawneryv1alpha1.ChangeoverWaiting || g.Status.Changeover == spawneryv1alpha1.ChangeoverBegun
		if lobbyBegan.IsZero() && gatewayInFlight && l.Status.ObservedGeneration >= lobbyGeneration {
			if reason != spawneryv1alpha1.ReasonWaitingForEarlierStage {
				violations = append(violations, fmt.Sprintf("+%s gateway %q, lobby Progressing reason %q (%s)",
					since(time.Now()), g.Status.Changeover, reason, message))
			} else if firstGated.IsZero() {
				firstGated = time.Now()
				t.Logf("+%s lobby gated: changeover=%q Progressing %s: %s", since(firstGated), l.Status.Changeover, reason, message)
				if want := "waiting for stage -10: " + tutorialProxyGroup; message != want {
					violations = append(violations, fmt.Sprintf("lobby Progressing message %q, want %q", message, want))
				}
			}
		}

		if !proxyStood.IsZero() && !lobbyBegan.IsZero() &&
			g.Status.Changeover == spawneryv1alpha1.ChangeoverNone && l.Status.Changeover == spawneryv1alpha1.ChangeoverNone {
			t.Logf("+%s both done: gateway ready=%d, lobby ready=%d", since(time.Now()), g.Status.ReadyReplicas, l.Status.ReadyReplicas)
			break
		}
	}

	for _, v := range violations {
		t.Errorf("lobby not held by the stage gate: %s", v)
	}
	switch {
	case proxyStood.IsZero():
		t.Fatalf("the gateway never stood within the deadline%s", denialHint(t, tutorialOperatorNamespace))
	case lobbyBegan.IsZero():
		t.Fatalf("the lobby never created a server under its new spec%s", denialHint(t, tutorialOperatorNamespace))
	}
	if firstGated.IsZero() {
		t.Errorf("never saw the lobby report %s while the gateway was in flight", spawneryv1alpha1.ReasonWaitingForEarlierStage)
	}
	if lobbyBegan.Before(proxyStood.Add(-3 * time.Second)) {
		t.Errorf("lobby's first new server %s was created at +%s, before the gateway stood at +%s",
			lobbyBeganName, since(lobbyBegan), since(proxyStood))
	}

	var g spawneryv1alpha1.ProxyGroup
	var l spawneryv1alpha1.ServerGroup
	if err := k8s.Get(ctx, gatewayKey, &g); err != nil {
		t.Fatalf("get ProxyGroup: %v", err)
	}
	if err := k8s.Get(ctx, lobbyKey, &l); err != nil {
		t.Fatalf("get ServerGroup: %v", err)
	}
	if g.Status.Changeover != "" || l.Status.Changeover != "" {
		t.Errorf("changeovers not finished: gateway %q, lobby %q", g.Status.Changeover, l.Status.Changeover)
	}
}

// TestTutorialTransferOnDrain rolls a two-proxy gateway under a held player
// and expects the leaving proxy to transfer them to the other one, back onto
// the backend they were on.
//
// The player is placed behind the lobby server a fresh join avoids: a filler
// takes the first server on one proxy, the player joins until it lands on the
// same proxy and so on the second, and the filler leaves before the roll. A
// proxy with no players sends a fresh join to the first server by name, so
// only the transfer cookie can bring the player back to the second.
//
// Which proxy pod and backend a player is on is read from the Velocity log of
// each proxy pod: "[server connection] <player> -> <server> has connected".
func TestTutorialTransferOnDrain(t *testing.T) {
	if os.Getenv("SPAWNERY_E2E_TUTORIAL") != "1" {
		t.Skip("set SPAWNERY_E2E_TUTORIAL=1; hack/e2e-tutorial.sh does this nightly")
	}
	joinPath, err := exec.LookPath("spawnery-join")
	if err != nil {
		t.Fatalf("spawnery-join not on PATH (%v); the dev shell carries it, run this through nix develop", err)
	}

	applyManifest(t, tutorialManifest)

	gatewayKey := client.ObjectKey{Namespace: tutorialNamespace, Name: tutorialProxyGroup}
	lobbyKey := client.ObjectKey{Namespace: tutorialNamespace, Name: tutorialServerGroup}

	var gateway spawneryv1alpha1.ProxyGroup
	if err := k8s.Get(ctx, gatewayKey, &gateway); err != nil {
		t.Fatalf("get ProxyGroup: %v", err)
	}
	gatewayBefore := gateway.DeepCopy()
	gatewayPatch := client.MergeFrom(gateway.DeepCopy())
	gateway.Spec.Replicas = 2
	update := spawneryv1alpha1.ProxyUpdateSpec{}
	if gateway.Spec.Update != nil {
		update = *gateway.Spec.Update.DeepCopy()
	}
	update.Transfer = &spawneryv1alpha1.ProxyTransferSpec{ForceAfterSeconds: ptr.To[int32](0)}
	gateway.Spec.Update = &update
	// A blue/green roll of two replicas runs four proxies at once; at the
	// tutorial's 500m each they do not fit beside the lobby on a 4-CPU runner.
	resources := corev1.ResourceRequirements{}
	if gateway.Spec.Resources != nil {
		resources = *gateway.Spec.Resources.DeepCopy()
	}
	if resources.Requests == nil {
		resources.Requests = corev1.ResourceList{}
	}
	resources.Requests[corev1.ResourceCPU] = resource.MustParse("100m")
	gateway.Spec.Resources = &resources
	if err := k8s.Patch(ctx, &gateway, gatewayPatch); err != nil {
		t.Fatalf("patch ProxyGroup: %v", err)
	}
	t.Cleanup(func() {
		var now spawneryv1alpha1.ProxyGroup
		if err := k8s.Get(ctx, gatewayKey, &now); err != nil {
			return
		}
		back := client.MergeFrom(now.DeepCopy())
		now.Spec.Replicas = gatewayBefore.Spec.Replicas
		now.Spec.Update = gatewayBefore.Spec.Update
		now.Spec.Env = gatewayBefore.Spec.Env
		now.Spec.Resources = gatewayBefore.Spec.Resources
		_ = k8s.Patch(ctx, &now, back)
	})

	var lobby spawneryv1alpha1.ServerGroup
	if err := k8s.Get(ctx, lobbyKey, &lobby); err != nil {
		t.Fatalf("get ServerGroup: %v", err)
	}
	minBefore := lobby.Spec.Scaling.MinReplicas
	lobbyPatch := client.MergeFrom(lobby.DeepCopy())
	lobby.Spec.Scaling.MinReplicas = 2
	if err := k8s.Patch(ctx, &lobby, lobbyPatch); err != nil {
		t.Fatalf("patch ServerGroup: %v", err)
	}
	t.Cleanup(func() {
		var now spawneryv1alpha1.ServerGroup
		if err := k8s.Get(ctx, lobbyKey, &now); err != nil {
			return
		}
		back := client.MergeFrom(now.DeepCopy())
		now.Spec.Scaling.MinReplicas = minBefore
		_ = k8s.Patch(ctx, &now, back)
	})

	proxyPods := func() ([]corev1.Pod, error) {
		var list corev1.PodList
		err := k8s.List(ctx, &list, client.InNamespace(tutorialNamespace),
			client.MatchingLabels{podspec.LabelGroup: tutorialProxyGroup, podspec.LabelRole: podspec.RoleProxy})
		return list.Items, err
	}

	eventuallyIn(t, tutorialOperatorNamespace, 5*time.Minute, "two transferring proxies and two Ready lobby servers", func() (bool, string) {
		var g spawneryv1alpha1.ProxyGroup
		if err := k8s.Get(ctx, gatewayKey, &g); err != nil {
			return false, err.Error()
		}
		pods, err := proxyPods()
		if err != nil {
			return false, err.Error()
		}
		steady := 0
		for _, p := range pods {
			if p.DeletionTimestamp == nil && podReady(&p) && p.Annotations[podspec.AnnotationProxyDrainingSince] == "" &&
				hasEnv(&p, podspec.ProxyContainerName, "SPAWNERY_TRANSFER_FORCE_AFTER_SECONDS") {
				steady++
			}
		}
		ready := len(readyLobbyServers())
		return g.Status.Changeover == "" && g.Status.ObservedGeneration == g.Generation && g.Status.ReadyReplicas == 2 &&
				len(pods) == 2 && steady == 2 && ready >= 2,
			fmt.Sprintf("gateway changeover=%q ready=%d pods=%d steady=%d; lobby ready=%d",
				g.Status.Changeover, g.Status.ReadyReplicas, len(pods), steady, ready)
	})
	t.Logf("lobby servers: %v", readyLobbyServers())

	base := fmt.Sprintf("tf%04d", time.Now().UnixNano()%10000)
	var joins []*heldJoin
	t.Cleanup(func() {
		for _, j := range joins {
			j.stop()
		}
	})
	// Velocity's login-ratelimit default: one login per address every 3000 ms,
	// and every join here reaches the proxies from the same node address.
	var lastJoin time.Time
	join := func(username string, hold time.Duration) *heldJoin {
		time.Sleep(time.Until(lastJoin.Add(3500 * time.Millisecond)))
		lastJoin = time.Now()
		j := startHeldJoin(t, joinPath, username, hold)
		joins = append(joins, j)
		return j
	}

	filler := join(base+"f", 10*time.Minute)
	fillerPod, fillerServer := whereIs(t, filler, proxyPods)
	t.Logf("filler %s: proxy %s, backend %s", filler.username, fillerPod, fillerServer)

	leave := func(j *heldJoin, pod string) {
		j.stop()
		eventuallyIn(t, tutorialOperatorNamespace, 30*time.Second, j.username+" to leave "+pod, func() (bool, string) {
			log, err := readPodLog(tutorialNamespace, pod, &corev1.PodLogOptions{Container: podspec.ProxyContainerName})
			if err != nil {
				return false, err.Error()
			}
			left := regexp.MustCompile(`\[connected player\] ` + regexp.QuoteMeta(j.username) + `\b[^\n]* has disconnected`)
			return left.MatchString(log), "no disconnect line for " + j.username
		})
	}

	const hold = 2 * time.Minute
	var player *heldJoin
	var playerPod, playerServer string
	var holdEnd time.Time
	for attempt := 1; attempt <= 12 && player == nil; attempt++ {
		j := join(fmt.Sprintf("%sp%d", base, attempt), hold)
		started := time.Now()
		pod, server := whereIs(t, j, proxyPods)
		t.Logf("attempt %d, %s: proxy %s, backend %s", attempt, j.username, pod, server)
		if pod == fillerPod && server != fillerServer {
			player, playerPod, playerServer, holdEnd = j, pod, server, started.Add(hold)
			continue
		}
		leave(j, pod)
	}
	if player == nil {
		t.Fatalf("twelve joins never shared the filler's proxy %s; the NodePort is not spreading connections", fillerPod)
	}
	leave(filler, fillerPod)

	var original corev1.Pod
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: tutorialNamespace, Name: playerPod}, &original); err != nil {
		t.Fatalf("get the player's proxy pod: %v", err)
	}
	transferred := followTransferLines(t, playerPod, player.username)

	if err := k8s.Get(ctx, gatewayKey, &gateway); err != nil {
		t.Fatalf("get ProxyGroup: %v", err)
	}
	rollPatch := client.MergeFrom(gateway.DeepCopy())
	gateway.Spec.Env = append(gateway.Spec.Env, corev1.EnvVar{Name: "TRANSFER_PROBE", Value: strconv.FormatInt(time.Now().UnixNano(), 10)})
	if err := k8s.Patch(ctx, &gateway, rollPatch); err != nil {
		t.Fatalf("patch ProxyGroup: %v", err)
	}
	rolled := time.Now()
	t.Logf("gateway rolled with %s on %s behind %s", player.username, playerPod, playerServer)

	arrivalDeadline := rolled.Add(75 * time.Second)
	if latest := holdEnd.Add(-10 * time.Second); latest.Before(arrivalDeadline) {
		arrivalDeadline = latest
	}
	var arrivedPod, arrivedServer string
	var originalGone bool
	for ; time.Now().Before(arrivalDeadline); time.Sleep(time.Second) {
		select {
		case err := <-player.done:
			player.done <- err
			t.Fatalf("%s's join ended %s after the roll, before it was seen on another proxy (%v); transfer lines on %s: %q\n%s",
				player.username, time.Since(rolled).Round(time.Second), err, playerPod, transferred(), player.out.String())
		default:
		}
		if arrivedPod == "" {
			if pods, err := proxyPods(); err == nil {
				for _, p := range pods {
					if p.Name == playerPod {
						continue
					}
					if server := lastServerConnection(p.Name, player.username); server != "" {
						arrivedPod, arrivedServer = p.Name, server
						t.Logf("+%s %s arrived on %s behind %s", time.Since(rolled).Round(time.Second), player.username, arrivedPod, arrivedServer)
					}
				}
			}
		}
		if !originalGone {
			var now corev1.Pod
			err := k8s.Get(ctx, client.ObjectKey{Namespace: tutorialNamespace, Name: playerPod}, &now)
			originalGone = apierrors.IsNotFound(err) || (err == nil && now.UID != original.UID)
			if originalGone {
				t.Logf("+%s %s is gone", time.Since(rolled).Round(time.Second), playerPod)
			}
		}
		if arrivedPod != "" && originalGone {
			break
		}
	}
	if arrivedPod == "" {
		t.Fatalf("%s never arrived on another proxy within %s of the roll; transfer lines on %s: %q%s",
			player.username, arrivalDeadline.Sub(rolled).Round(time.Second), playerPod, transferred(), denialHint(t, tutorialOperatorNamespace))
	}
	if !originalGone {
		t.Fatalf("%s arrived on %s, but its original proxy %s never went away", player.username, arrivedPod, playerPod)
	}
	for _, line := range transferred() {
		t.Logf("%s: %s", playerPod, line)
	}
	if len(transferred()) == 0 {
		t.Errorf("%s never logged a transfer of %s", playerPod, player.username)
	}
	if arrivedServer != playerServer {
		t.Errorf("%s arrived behind %s, want %s, the server it was on before the roll", player.username, arrivedServer, playerServer)
	}
	arrivedLog, err := readPodLog(tutorialNamespace, arrivedPod, &corev1.PodLogOptions{Container: podspec.ProxyContainerName})
	if err != nil {
		t.Fatalf("read %s's log: %v", arrivedPod, err)
	}
	if refused := regexp.MustCompile(`spawnery: transfer cookie from '` + regexp.QuoteMeta(player.username) + `' refused[^\n]*`).FindString(arrivedLog); refused != "" {
		t.Errorf("%s: %s", arrivedPod, refused)
	}

	statusWait := 45 * time.Second
	if left := time.Until(holdEnd.Add(-5 * time.Second)); left < statusWait {
		statusWait = left
	}
	eventuallyIn(t, tutorialOperatorNamespace, statusWait, "the operator to count the player on "+playerServer, func() (bool, string) {
		players := lobbyServerPlayers()
		if _, ok := players[playerServer]; !ok {
			return false, fmt.Sprintf("%s is not among the Ready lobby servers %v", playerServer, players)
		}
		seen := ""
		ok := true
		for name, players := range players {
			seen += fmt.Sprintf(" %s=%d", name, players)
			want := int32(0)
			if name == playerServer {
				want = 1
			}
			ok = ok && players == want
		}
		return ok, "status.players:" + seen
	})

	// spawnery-join reconnects only where a Transfer points, so a process
	// still running after its username arrived on another pod followed one.
	select {
	case err := <-player.done:
		player.done <- err
		t.Fatalf("%s's join ended before the checks were done (%v):\n%s", player.username, err, player.out.String())
	default:
	}
	player.stop()

	t.Run("a closed door shields the player", func(t *testing.T) {
		t.Skip("no command or tool closes a backend's door from outside the server: AcceptJoins is " +
			"reachable only through the plugin API; the agent's TransferPolicy tests cover the rule")
	})
}

// TestTutorialJoinPermission puts a vip group ahead of the lobby in the
// gateway's fallback list. The test images carry no LuckPerms, so nobody
// holds a node: with Required the player lands in the lobby without the vip
// server ever refusing them, and with DenyOnly they land in vip.
func TestTutorialJoinPermission(t *testing.T) {
	if os.Getenv("SPAWNERY_E2E_TUTORIAL") != "1" {
		t.Skip("set SPAWNERY_E2E_TUTORIAL=1; hack/e2e-tutorial.sh does this nightly")
	}
	joinPath, err := exec.LookPath("spawnery-join")
	if err != nil {
		t.Fatalf("spawnery-join not on PATH (%v); the dev shell carries it, run this through nix develop", err)
	}

	applyManifest(t, tutorialManifest)

	const vipGroup = "vip"
	gatewayKey := client.ObjectKey{Namespace: tutorialNamespace, Name: tutorialProxyGroup}
	vipKey := client.ObjectKey{Namespace: tutorialNamespace, Name: vipGroup}

	var lobby spawneryv1alpha1.ServerGroup
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: tutorialNamespace, Name: tutorialServerGroup}, &lobby); err != nil {
		t.Fatalf("get ServerGroup: %v", err)
	}
	vip := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: tutorialNamespace, Name: vipGroup},
		Spec:       *lobby.Spec.DeepCopy(),
	}
	vip.Spec.Scaling = &spawneryv1alpha1.ScalingSpec{MinReplicas: 1, MaxReplicas: 1, SpareSlots: 0}
	// The nightly runner has 4 vCPUs; a second group at the Network's 500m does not fit.
	vip.Spec.Resources = &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("2Gi")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
	}
	vip.Spec.JoinPermission = &spawneryv1alpha1.JoinPermission{Mode: spawneryv1alpha1.JoinPermissionDenyOnly}
	if err := k8s.Create(ctx, vip); err != nil {
		t.Fatalf("create ServerGroup %s: %v", vipGroup, err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, &spawneryv1alpha1.ServerGroup{ObjectMeta: vip.ObjectMeta}) })

	var gateway spawneryv1alpha1.ProxyGroup
	if err := k8s.Get(ctx, gatewayKey, &gateway); err != nil {
		t.Fatalf("get ProxyGroup: %v", err)
	}
	fallbackBefore := append([]string(nil), gateway.Spec.Routing.FallbackGroups...)
	gatewayPatch := client.MergeFrom(gateway.DeepCopy())
	gateway.Spec.Routing.FallbackGroups = []string{vipGroup, tutorialServerGroup}
	if err := k8s.Patch(ctx, &gateway, gatewayPatch); err != nil {
		t.Fatalf("patch ProxyGroup: %v", err)
	}
	t.Cleanup(func() {
		var now spawneryv1alpha1.ProxyGroup
		if err := k8s.Get(ctx, gatewayKey, &now); err != nil {
			return
		}
		back := client.MergeFrom(now.DeepCopy())
		now.Spec.Routing.FallbackGroups = fallbackBefore
		_ = k8s.Patch(ctx, &now, back)
	})

	gatewayPods := func() ([]corev1.Pod, error) {
		var list corev1.PodList
		err := k8s.List(ctx, &list, client.InNamespace(tutorialNamespace),
			client.MatchingLabels{podspec.LabelGroup: tutorialProxyGroup, podspec.LabelRole: podspec.RoleProxy})
		return list.Items, err
	}
	groupOf := func(server string) string {
		var s spawneryv1alpha1.Server
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: tutorialNamespace, Name: server}, &s); err != nil {
			return ""
		}
		return s.Spec.GroupRef.Name
	}

	wantFallback := strings.Join(gateway.Spec.Routing.FallbackGroups, ",")
	var vipServer string
	eventuallyIn(t, tutorialOperatorNamespace, 5*time.Minute, "a Ready vip server and every gateway pod on "+wantFallback, func() (bool, string) {
		vipServer = ""
		var servers spawneryv1alpha1.ServerList
		if err := k8s.List(ctx, &servers, client.InNamespace(tutorialNamespace)); err != nil {
			return false, err.Error()
		}
		for _, s := range servers.Items {
			if s.Spec.GroupRef.Name == vipGroup && s.Status.Phase == string(phase.Ready) {
				vipServer = s.Name
			}
		}
		pods, err := gatewayPods()
		if err != nil {
			return false, err.Error()
		}
		current := 0
		for _, p := range pods {
			if p.DeletionTimestamp == nil && podReady(&p) && envValue(&p, podspec.ProxyContainerName, podspec.EnvFallbackGroups) == wantFallback {
				current++
			}
		}
		return vipServer != "" && len(pods) > 0 && current == len(pods),
			fmt.Sprintf("vip server %q; gateway pods %d, on %s %d", vipServer, len(pods), wantFallback, current)
	})

	// No observable says an agent has applied a sync; the change push normally lands in well under a second.
	time.Sleep(15 * time.Second)

	// DenyOnly admits everyone, so this join lands in vip only if the proxy knows vip.
	j := startHeldJoin(t, joinPath, "denyonly", 20*time.Second)
	_, server := whereIs(t, j, gatewayPods)
	j.stop()
	if g := groupOf(server); g != vipGroup {
		t.Errorf("denyonly landed on %s of group %q, want group %q", server, g, vipGroup)
	}

	if err := k8s.Get(ctx, vipKey, vip); err != nil {
		t.Fatalf("get ServerGroup %s: %v", vipGroup, err)
	}
	vipPatch := client.MergeFrom(vip.DeepCopy())
	vip.Spec.JoinPermission.Mode = spawneryv1alpha1.JoinPermissionRequired
	if err := k8s.Patch(ctx, vip, vipPatch); err != nil {
		t.Fatalf("patch ServerGroup %s: %v", vipGroup, err)
	}

	// No observable says an agent has applied a sync; the change push normally lands in well under a second.
	time.Sleep(15 * time.Second)

	j = startHeldJoin(t, joinPath, "required", 20*time.Second)
	_, server = whereIs(t, j, gatewayPods)
	j.stop()
	if g := groupOf(server); g != tutorialServerGroup {
		t.Errorf("required landed on %s of group %q, want group %q", server, g, tutorialServerGroup)
	}
	vipLog, err := readPodLog(tutorialNamespace, vipServer, &corev1.PodLogOptions{Container: podspec.ContainerName})
	if err != nil {
		t.Fatalf("read %s's log: %v", vipServer, err)
	}
	if refused := regexp.MustCompile(`spawnery: refused 'required'[^\n]*`).FindString(vipLog); refused != "" {
		t.Errorf("the proxy sent required into a refusal instead of around %s: %s", vipGroup, refused)
	}
}

type heldJoin struct {
	username string
	cmd      *exec.Cmd
	out      *bytes.Buffer
	done     chan error
}

func startHeldJoin(t *testing.T, joinPath, username string, hold time.Duration) *heldJoin {
	t.Helper()
	j := &heldJoin{username: username, out: &bytes.Buffer{}, done: make(chan error, 1)}
	j.cmd = exec.Command(joinPath,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(tutorialJoinPort),
		"--username", username,
		"--timeout", (hold + 30*time.Second).String(),
		"--hold", hold.String(),
		"--follow-transfers",
	)
	j.cmd.Stdout = j.out
	j.cmd.Stderr = j.out
	if err := j.cmd.Start(); err != nil {
		t.Fatalf("start spawnery-join: %v", err)
	}
	go func() { j.done <- j.cmd.Wait() }()
	return j
}

func (j *heldJoin) stop() {
	_ = j.cmd.Process.Kill()
	err := <-j.done
	j.done <- err
}

func whereIs(t *testing.T, j *heldJoin, pods func() ([]corev1.Pod, error)) (string, string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for ; time.Now().Before(deadline); time.Sleep(500 * time.Millisecond) {
		select {
		case err := <-j.done:
			j.done <- err
			t.Fatalf("spawnery-join as %s ended before it reached a backend (%v):\n%s", j.username, err, j.out.String())
		default:
		}
		list, err := pods()
		if err != nil {
			continue
		}
		for _, p := range list {
			if server := lastServerConnection(p.Name, j.username); server != "" {
				return p.Name, server
			}
		}
	}
	t.Fatalf("%s was not seen on any proxy within 30s", j.username)
	return "", ""
}

func lastServerConnection(pod, username string) string {
	log, err := readPodLog(tutorialNamespace, pod, &corev1.PodLogOptions{Container: podspec.ProxyContainerName})
	if err != nil {
		return ""
	}
	connected := regexp.MustCompile(`\[server connection\] ` + regexp.QuoteMeta(username) + ` -> (\S+) has connected`)
	matches := connected.FindAllStringSubmatch(log, -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1][1]
}

// followTransferLines collects the agent's transfer lines for username from
// pod's log until the container ends: the pod is deleted once it is empty,
// and a poll could miss its last lines.
func followTransferLines(t *testing.T, pod, username string) func() []string {
	t.Helper()
	stream, err := clientset.CoreV1().Pods(tutorialNamespace).
		GetLogs(pod, &corev1.PodLogOptions{Container: podspec.ProxyContainerName, Follow: true}).Stream(ctx)
	if err != nil {
		t.Fatalf("follow %s's log: %v", pod, err)
	}
	t.Cleanup(func() { _ = stream.Close() })
	wanted := regexp.MustCompile(`spawnery: (transferred|could not transfer) '` + regexp.QuoteMeta(username) + `'.*`)
	var mu sync.Mutex
	var lines []string
	go func() {
		scanner := bufio.NewScanner(stream)
		for scanner.Scan() {
			if m := wanted.FindString(scanner.Text()); m != "" {
				mu.Lock()
				lines = append(lines, m)
				mu.Unlock()
			}
		}
	}()
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), lines...)
	}
}

func readyLobbyServers() []string {
	var list spawneryv1alpha1.ServerList
	if err := k8s.List(ctx, &list, client.InNamespace(tutorialNamespace)); err != nil {
		return nil
	}
	var names []string
	for _, s := range list.Items {
		if s.Spec.GroupRef.Name == tutorialServerGroup && s.Status.Phase == string(phase.Ready) {
			names = append(names, s.Name)
		}
	}
	return names
}

func lobbyServerPlayers() map[string]int32 {
	var list spawneryv1alpha1.ServerList
	if err := k8s.List(ctx, &list, client.InNamespace(tutorialNamespace)); err != nil {
		return nil
	}
	players := map[string]int32{}
	for _, s := range list.Items {
		if s.Spec.GroupRef.Name == tutorialServerGroup && s.Status.Phase == string(phase.Ready) {
			players[s.Name] = s.Status.Players
		}
	}
	return players
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func hasEnv(p *corev1.Pod, container, name string) bool {
	for _, c := range p.Spec.Containers {
		if c.Name != container {
			continue
		}
		for _, e := range c.Env {
			if e.Name == name {
				return true
			}
		}
	}
	return false
}

func envValue(p *corev1.Pod, container, name string) string {
	for _, c := range p.Spec.Containers {
		if c.Name != container {
			continue
		}
		for _, e := range c.Env {
			if e.Name == name {
				return e.Value
			}
		}
	}
	return ""
}
