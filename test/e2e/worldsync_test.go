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
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/instance"
)

const (
	worldSyncGate = "SPAWNERY_E2E_WORLDSYNC"

	worldSyncManifest   = "test/e2e/manifests/worldsync.yaml"
	worldSyncNamespace  = "minecraft-ws"
	worldSyncGroup      = "private-servers"
	worldSyncProxyGroup = "gateway"
	worldSyncKey        = "c0ffee"

	storeNamespace = "worldsync-e2e"
	storeBucket    = "worlds"
)

// TestAnObjectStoreWorldTravelsBetweenNodes starts a member whose world lives
// in the object store, writes a marker into it, stops it, cordons the node it
// ran on, starts it again and reads the marker on the other node. Then it
// deletes the member and waits for the world's prefix to empty.
//
// The game image is the real Purpur image, whose agent waits in the Paper
// bootstrapper for the world the node agent hands it; this run is the first
// to show that wait ending in a server that is Ready.
func TestAnObjectStoreWorldTravelsBetweenNodes(t *testing.T) {
	if os.Getenv(worldSyncGate) != "1" {
		t.Skipf("set %s=1 to run the object-store path; hack/e2e-worldsync.sh does, "+
			"hack/e2e.sh does not -- it has neither a store nor a real game image",
			worldSyncGate)
	}

	applyManifest(t, worldSyncManifest)

	server, err := instance.Name(worldSyncGroup, worldSyncKey)
	if err != nil {
		t.Fatalf("compose the member name: %v", err)
	}
	prefix := fmt.Sprintf("%s/%s/%s/", worldSyncNamespace, worldSyncGroup, worldSyncKey)

	proxy := aProxyPodOf(t, worldSyncNamespace, worldSyncProxyGroup)

	first := startServer(t, proxy, worldSyncGroup, worldSyncKey)
	if first.GetServer() != server {
		t.Fatalf("the operator started %q, want %q: the name a plugin gets back is the one it "+
			"routes the player to", first.GetServer(), server)
	}
	if first.GetAlreadyRunning() {
		t.Fatalf("the first start of %s answered already_running: nothing had asked for this key "+
			"before, so everything below would be measuring a member this test did not start", server)
	}

	waitReady(t, worldSyncNamespace, server, "the first start")

	firstPod := getPod(t, worldSyncNamespace, server)
	node := firstPod.Spec.NodeName
	if node == "" {
		t.Fatalf("pod %s is Ready but names no node", server)
	}

	assertWorldGroupWritable(t, firstPod)

	// Unique per run, for clusters kept with E2E_KEEP=1.
	marker := fmt.Sprintf("worldsync-e2e %d", time.Now().UnixNano())
	writeMarker(t, worldSyncNamespace, server, marker)

	stopServer(t, proxy, server)

	eventually(t, 5*time.Minute, "the stopped server to be gone", func() (bool, string) {
		var srv spawneryv1alpha1.Server
		err := k8s.Get(ctx, client.ObjectKey{Namespace: worldSyncNamespace, Name: server}, &srv)
		if apierrors.IsNotFound(err) {
			return true, ""
		}
		if err != nil {
			return false, err.Error()
		}
		return false, "phase " + srv.Status.Phase
	})
	eventually(t, 2*time.Minute, "the stopped server's pod to be gone", func() (bool, string) {
		var pod corev1.Pod
		err := k8s.Get(ctx, client.ObjectKey{Namespace: worldSyncNamespace, Name: server}, &pod)
		if apierrors.IsNotFound(err) {
			return true, ""
		}
		if err != nil {
			return false, err.Error()
		}
		return false, "phase " + string(pod.Status.Phase)
	})

	if out, err := exec.Command("kubectl", "cordon", node).CombinedOutput(); err != nil {
		t.Fatalf("cordon %s: %v\n%s", node, err, out)
	}
	t.Cleanup(func() {
		if out, err := exec.Command("kubectl", "uncordon", node).CombinedOutput(); err != nil {
			t.Logf("uncordon %s: %v\n%s", node, err, out)
		}
	})

	second := startServer(t, proxy, worldSyncGroup, worldSyncKey)
	if second.GetServer() != server {
		t.Fatalf("the second start of key %q made %q, want the same %q: a player who comes back "+
			"under a new name is a player who comes back to an empty world",
			worldSyncKey, second.GetServer(), server)
	}
	if second.GetAlreadyRunning() {
		t.Fatalf("the second start answered already_running, yet the first server and its pod " +
			"were both observed gone above")
	}

	waitReady(t, worldSyncNamespace, server, "the second start")

	secondPod := getPod(t, worldSyncNamespace, server)
	if secondPod.Spec.NodeName == node {
		t.Fatalf("the second start of %s ran on %s again, though that node was cordoned: this "+
			"run proves nothing about a world travelling between nodes", server, node)
	}

	got := readMarker(t, worldSyncNamespace, server)
	if got != marker {
		t.Fatalf("the world of %s came back on %s with %q, want %q. The server is gone and made "+
			"again on another node, and the player's world is not the one they left",
			server, secondPod.Spec.NodeName, got, marker)
	}

	deleted := deleteServer(t, proxy, worldSyncGroup, worldSyncKey)
	if deleted.GetServer() != server {
		t.Fatalf("deleting key %q removed %q, want %q", worldSyncKey, deleted.GetServer(), server)
	}

	eventually(t, 5*time.Minute, "the deleted member's world to leave the object store", func() (bool, string) {
		listing, err := listStore(prefix)
		if err != nil {
			return false, err.Error()
		}
		if listing == "" {
			return true, ""
		}
		return false, "still stored: " + strings.ReplaceAll(strings.TrimSpace(listing), "\n", "; ")
	})
}

func getPod(t *testing.T, namespace, name string) *corev1.Pod {
	t.Helper()
	var pod corev1.Pod
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &pod); err != nil {
		t.Fatalf("get pod %s/%s: %v", namespace, name, err)
	}
	return &pod
}

// assertWorldGroupWritable checks that /data belongs to the pod's fsGroup and
// that the group may write there: the server runs as a non-root user, and the
// node agent, which runs as root, has to hand it a world it can write.
func assertWorldGroupWritable(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	sc := pod.Spec.SecurityContext
	if sc == nil || sc.FSGroup == nil {
		t.Fatalf("pod %s carries no fsGroup, so a non-root server has no group to own its world", pod.Name)
	}
	out, err := kubectlExec(t, pod.Namespace, pod.Name, "stat -c '%g %a' /data")
	if err != nil {
		t.Fatalf("stat /data in %s: %v\n%s", pod.Name, err, out)
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		t.Fatalf("stat /data in %s printed %q, want a group and a mode", pod.Name, out)
	}
	gid, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		t.Fatalf("stat /data in %s: group %q is not a number", pod.Name, fields[0])
	}
	if gid != *sc.FSGroup {
		t.Fatalf("/data in %s belongs to group %d, want the pod's fsGroup %d: the server could not "+
			"write its world, and would fail at startup", pod.Name, gid, *sc.FSGroup)
	}
	mode, err := strconv.ParseUint(fields[1], 8, 32)
	if err != nil {
		t.Fatalf("stat /data in %s: mode %q is not octal", pod.Name, fields[1])
	}
	if mode&0o020 == 0 {
		t.Fatalf("/data in %s has mode %s, which the group cannot write: the server could not "+
			"write its world, and would fail at startup", pod.Name, fields[1])
	}
}

// listStore returns the objects under prefix in the world bucket, one per
// line; empty when there are none. mc is in the store's own pod.
func listStore(prefix string) (string, error) {
	cmd := exec.Command("kubectl", "-n", storeNamespace, "exec", "deploy/minio", "--",
		"mc", "ls", "--recursive", "local/"+storeBucket+"/"+prefix)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "does not exist") {
			return "", nil
		}
		return "", fmt.Errorf("list %s: %v: %s", prefix, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
