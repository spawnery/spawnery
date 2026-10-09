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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/instance"
)

const worldHistoryKey = "5ca1ab1e"

func TestAnObjectStoreWorldGoesBackToAnEarlierGeneration(t *testing.T) {
	if os.Getenv(worldSyncGate) != "1" {
		t.Skipf("set %s=1 to run the object-store path; hack/e2e-worldsync.sh does", worldSyncGate)
	}
	applyManifest(t, worldSyncManifest)
	server, err := instance.Name(worldSyncGroup, worldHistoryKey)
	if err != nil {
		t.Fatalf("compose the member name: %v", err)
	}
	proxy := aProxyPodOf(t, worldSyncNamespace, worldSyncProxyGroup)

	runMember(t, proxy, server, "the first run")
	writeWorldFile(t, server, "spawnery-e2e-first", "first")
	stopAndWaitGone(t, proxy, server)

	runMember(t, proxy, server, "the second run")
	if got := readWorldFile(t, server, "spawnery-e2e-first"); got != "first" {
		t.Fatalf("the second run found %q in the first run's file, want \"first\"", got)
	}
	found := restorePoints(t, proxy)[0]
	if !found.GetCurrent() {
		t.Fatalf("the first restore point %v is not the current one", found)
	}
	writeWorldFile(t, server, "spawnery-e2e-second", "second")
	stopAndWaitGone(t, proxy, server)

	back := restoreWhenFree(t, proxy, found.GetGeneration())
	if back.GetRestoredFrom() != found.GetGeneration() {
		t.Fatalf("restored from %d, asked for %d", back.GetRestoredFrom(), found.GetGeneration())
	}
	points := restorePoints(t, proxy)
	if points[0].GetGeneration() != back.GetGeneration() || !points[0].GetCurrent() {
		t.Fatalf("restore points %v do not start with the restore's generation %d", points, back.GetGeneration())
	}
	if !hasGeneration(points, back.GetGeneration()-1) || !hasGeneration(points, found.GetGeneration()) {
		t.Fatalf("restore points %v lack the generation the restore replaced or the one it restored", points)
	}

	runMember(t, proxy, server, "the run after the restore")
	if got := readWorldFile(t, server, "spawnery-e2e-first"); got != "first" {
		t.Fatalf("after the restore the first run's file holds %q, want \"first\"", got)
	}
	if worldFileExists(t, server, "spawnery-e2e-second") {
		t.Fatalf("after restoring generation %d the second run's file is still there: the start did not get the restored world", found.GetGeneration())
	}
	stopAndWaitGone(t, proxy, server)

	forward := restoreWhenFree(t, proxy, back.GetGeneration()-1)
	t.Logf("restored generation %d as %d", forward.GetRestoredFrom(), forward.GetGeneration())
	runMember(t, proxy, server, "the run after the second restore")
	if got := readWorldFile(t, server, "spawnery-e2e-second"); got != "second" {
		t.Fatalf("after undoing the restore the second run's file holds %q, want \"second\"", got)
	}
	if got := readWorldFile(t, server, "spawnery-e2e-first"); got != "first" {
		t.Fatalf("after undoing the restore the first run's file holds %q, want \"first\"", got)
	}

	deleteServer(t, proxy, worldSyncGroup, worldHistoryKey)
}

func runMember(t *testing.T, proxy *corev1.Pod, server, which string) {
	t.Helper()
	started := startServer(t, proxy, worldSyncGroup, worldHistoryKey)
	if started.GetServer() != server || started.GetAlreadyRunning() {
		t.Fatalf("%s: the start answered %v, want a new %s", which, started, server)
	}
	waitReady(t, worldSyncNamespace, server, which)
}

func stopAndWaitGone(t *testing.T, proxy *corev1.Pod, server string) {
	t.Helper()
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
}

func askOnce(t *testing.T, proxy *corev1.Pod, req *agentpb.CloudRequest) *agentpb.CloudResponse {
	t.Helper()
	s := openProxySession(t, proxy)
	defer s.close()
	return s.ask(t, req)
}

func restorePoints(t *testing.T, proxy *corev1.Pod) []*agentpb.RestorePoint {
	t.Helper()
	resp := askOnce(t, proxy, &agentpb.CloudRequest{Request: &agentpb.CloudRequest_ListRestorePoints{
		ListRestorePoints: &agentpb.ListRestorePointsRequest{Group: worldSyncGroup, Key: worldHistoryKey},
	}})
	points := resp.GetListRestorePoints().GetPoints()
	if len(points) == 0 {
		t.Fatalf("listing the restore points was answered %v", resp.GetError())
	}
	return points
}

// restoreWhenFree asks again while the answer is UNAVAILABLE: right after a
// stop the node still uploads the world under its lease.
func restoreWhenFree(t *testing.T, proxy *corev1.Pod, generation int64) *agentpb.RestoreWorldResult {
	t.Helper()
	var result *agentpb.RestoreWorldResult
	eventually(t, 3*time.Minute, fmt.Sprintf("a restore of generation %d", generation), func() (bool, string) {
		resp := askOnce(t, proxy, &agentpb.CloudRequest{Request: &agentpb.CloudRequest_RestoreWorld{
			RestoreWorld: &agentpb.RestoreWorldRequest{Group: worldSyncGroup, Key: worldHistoryKey, Generation: generation},
		}})
		if r := resp.GetRestoreWorld(); r != nil {
			result = r
			return true, ""
		}
		if resp.GetError().GetReason() == agentpb.RequestError_UNAVAILABLE {
			return false, resp.GetError().GetMessage()
		}
		t.Fatalf("restoring generation %d was answered %v", generation, resp.GetError())
		return false, ""
	})
	return result
}

func hasGeneration(points []*agentpb.RestorePoint, generation int64) bool {
	for _, p := range points {
		if p.GetGeneration() == generation {
			return true
		}
	}
	return false
}

func writeWorldFile(t *testing.T, server, name, content string) {
	t.Helper()
	out, err := kubectlExec(t, worldSyncNamespace, server, fmt.Sprintf("set -e; test -d /data/world; printf %%s %q > /data/world/%s", content, name))
	if err != nil {
		t.Fatalf("write %s into %s's world: %v\n%s", name, server, err, out)
	}
}

func readWorldFile(t *testing.T, server, name string) string {
	t.Helper()
	out, err := kubectlExec(t, worldSyncNamespace, server, "cat /data/world/"+name)
	if err != nil {
		t.Fatalf("read %s out of %s's world: %v\n%s", name, server, err, out)
	}
	return strings.TrimSpace(out)
}

func worldFileExists(t *testing.T, server, name string) bool {
	t.Helper()
	out, err := kubectlExec(t, worldSyncNamespace, server, "if test -e /data/world/"+name+"; then echo yes; else echo no; fi")
	if err != nil {
		t.Fatalf("look for %s in %s's world: %v\n%s", name, server, err, out)
	}
	return strings.TrimSpace(out) == "yes"
}
