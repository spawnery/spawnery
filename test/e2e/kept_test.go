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
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/instance"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/prune"
)

const (
	keptManifest = "test/e2e/manifests/kept-rounds.yaml"
	keptGroup    = "rounds"
	keptKey      = "finale"
)

// TestAKeptRoundComesBackOnAnotherNode runs a member of an Ephemeral group
// with storage, checks that its world stays out of the object store until it
// is keyed, keys it over the member's own session, deletes the member's pod,
// resumes the key on another node and finds the world there. Then it deletes
// the kept world and waits for its prefix to empty.
//
// No game plugin runs: the test asks keyWorld over a session bound to the
// member's pod and then writes the control files the Paper agent would.
func TestAKeptRoundComesBackOnAnotherNode(t *testing.T) {
	if os.Getenv(worldSyncGate) != "1" {
		t.Skipf("set %s=1 to run the object-store path; hack/e2e-worldsync.sh does", worldSyncGate)
	}

	applyManifest(t, worldSyncManifest)
	applyManifest(t, keptManifest)

	groupPrefix := fmt.Sprintf("%s/%s/", worldSyncNamespace, keptGroup)
	keyPrefix := groupPrefix + keptKey + "/"
	resumed, err := instance.KeptName(keptGroup, keptKey)
	if err != nil {
		t.Fatalf("compose the resumed member's name: %v", err)
	}

	member := aReadyMemberOf(t, worldSyncNamespace, keptGroup)
	assertWorldGroupWritable(t, getPod(t, worldSyncNamespace, member))

	// An unkeyed member is in standby: its world is a plain directory on the node.
	eventuallyStable(t, 2*time.Minute, 30*time.Second, "the unkeyed member to keep the store empty",
		storeEmpty(groupPrefix))

	marker := fmt.Sprintf("kept-e2e %d", time.Now().UnixNano())
	writeMarker(t, worldSyncNamespace, member, marker)

	world := keyWorldAs(t, worldSyncNamespace, member, keptKey)
	if want := worldSyncNamespace + "/" + keptGroup + "/" + keptKey; world != want {
		t.Fatalf("keyWorld answered world %q, want %q", world, want)
	}
	var srv spawneryv1alpha1.Server
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: worldSyncNamespace, Name: member}, &srv); err != nil {
		t.Fatalf("get %s: %v", member, err)
	}
	if srv.Spec.WorldKey != keptKey {
		t.Fatalf("%s carries worldKey %q after keyWorld answered, want %q", member, srv.Spec.WorldKey, keptKey)
	}

	// What the Paper agent does once keyWorld answers: ask the node agent to
	// take the key, then for a snapshot.
	controlFile := func(name string) string { return "/data/" + prune.ControlDir + "/" + name }
	out, err := kubectlExec(t, worldSyncNamespace, member, fmt.Sprintf(
		"set -e; rm -f %[1]s; printf %%s %[2]s > %[3]s.tmp; mv %[3]s.tmp %[3]s",
		controlFile("key.done"), keptKey, controlFile("key.request")))
	if err != nil {
		t.Fatalf("ask the node agent to take the key in %s: %v\n%s", member, err, out)
	}
	eventually(t, 2*time.Minute, "the node agent to take the key", func() (bool, string) {
		out, err := kubectlExec(t, worldSyncNamespace, member, "cat "+controlFile("key.done"))
		got := strings.TrimSpace(out)
		if err == nil && got == keptKey {
			return true, ""
		}
		if strings.HasPrefix(got, "failed ") {
			t.Fatalf("the node agent refused the key: %s", got)
		}
		return false, got
	})
	deadline := time.Now().Add(2 * time.Minute).UnixMilli()
	out, err = kubectlExec(t, worldSyncNamespace, member, fmt.Sprintf(
		"set -e; printf '1 %[1]d' > %[2]s.tmp; mv %[2]s.tmp %[2]s; printf 1 > %[3]s.tmp; mv %[3]s.tmp %[3]s",
		deadline, controlFile("snapshot.deadline"), controlFile("snapshot.request")))
	if err != nil {
		t.Fatalf("ask for a snapshot in %s: %v\n%s", member, err, out)
	}
	eventually(t, 2*time.Minute, "the keyed world's first snapshot", func() (bool, string) {
		out, _ := kubectlExec(t, worldSyncNamespace, member, "cat "+controlFile("snapshot.done"))
		got := strings.TrimSpace(out)
		if strings.HasPrefix(got, "failed ") {
			t.Fatalf("the node agent failed the snapshot: %s", got)
		}
		return got == "1", got
	})
	listing, err := listStore(keyPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listing, "manifest.json") {
		t.Fatalf("%s holds no manifest.json after the keyed world's snapshot: %q", keyPrefix, listing)
	}

	pod := getPod(t, worldSyncNamespace, member)
	node := pod.Spec.NodeName
	if err := k8s.Delete(ctx, pod); err != nil {
		t.Fatalf("delete pod %s: %v", member, err)
	}
	eventually(t, 5*time.Minute, "the keyed member to end with its pod", func() (bool, string) {
		var s spawneryv1alpha1.Server
		err := k8s.Get(ctx, client.ObjectKey{Namespace: worldSyncNamespace, Name: member}, &s)
		if apierrors.IsNotFound(err) {
			return true, ""
		}
		if err != nil {
			return false, err.Error()
		}
		return phase.Terminal(phase.Phase(s.Status.Phase)), "phase " + s.Status.Phase
	})

	if out, err := exec.Command("kubectl", "cordon", node).CombinedOutput(); err != nil {
		t.Fatalf("cordon %s: %v\n%s", node, err, out)
	}
	t.Cleanup(func() {
		if out, err := exec.Command("kubectl", "uncordon", node).CombinedOutput(); err != nil {
			t.Logf("uncordon %s: %v\n%s", node, err, out)
		}
	})

	proxy := aProxyPodOf(t, worldSyncNamespace, worldSyncProxyGroup)
	var started *agentpb.StartServerResult
	eventually(t, 3*time.Minute, "startServer to resume the key", func() (bool, string) {
		resp := askToStart(t, proxy, keptGroup, keptKey, nil)
		started = resp.GetStartServer()
		return started != nil, fmt.Sprint(resp.GetError())
	})
	if started.GetServer() != resumed {
		t.Fatalf("startServer resumed the key as %q, want %q", started.GetServer(), resumed)
	}
	if started.GetAlreadyRunning() {
		t.Fatalf("startServer answered already_running for %s, yet the keyed member had ended", resumed)
	}

	waitReady(t, worldSyncNamespace, resumed, "the resume")
	if got := getPod(t, worldSyncNamespace, resumed).Spec.NodeName; got == node {
		t.Fatalf("%s ran on the cordoned %s", resumed, node)
	}
	if got := readMarker(t, worldSyncNamespace, resumed); got != marker {
		t.Fatalf("the kept world came back with %q, want %q", got, marker)
	}

	deleted := deleteServer(t, proxy, keptGroup, keptKey)
	if deleted.GetServer() != resumed {
		t.Fatalf("deleting key %q removed %q, want %q", keptKey, deleted.GetServer(), resumed)
	}
	eventually(t, 5*time.Minute, "the kept world to leave the object store", storeEmpty(keyPrefix))
	eventually(t, 5*time.Minute, "every carrier of the key to be gone", func() (bool, string) {
		var all spawneryv1alpha1.ServerList
		if err := k8s.List(ctx, &all, client.InNamespace(worldSyncNamespace)); err != nil {
			return false, err.Error()
		}
		var left []string
		for _, m := range all.Items {
			if m.Spec.GroupRef.Name == keptGroup && m.Spec.WorldKey == keptKey {
				left = append(left, m.Name+" "+m.Status.Phase)
			}
		}
		return len(left) == 0, strings.Join(left, ", ")
	})
}

func storeEmpty(prefix string) func() (bool, string) {
	return func() (bool, string) {
		listing, err := listStore(prefix)
		if err != nil {
			return false, err.Error()
		}
		if listing == "" {
			return true, ""
		}
		return false, "stored: " + strings.ReplaceAll(listing, "\n", "; ")
	}
}

// aReadyMemberOf waits for a Ready, unkeyed member of group.
func aReadyMemberOf(t *testing.T, namespace, group string) string {
	t.Helper()
	var name string
	eventually(t, 6*time.Minute, "a Ready member of "+group, func() (bool, string) {
		var all spawneryv1alpha1.ServerList
		if err := k8s.List(ctx, &all, client.InNamespace(namespace)); err != nil {
			return false, err.Error()
		}
		var seen []string
		for _, m := range all.Items {
			if m.Spec.GroupRef.Name != group || m.Spec.WorldKey != "" {
				continue
			}
			if m.Status.Phase == string(phase.Ready) {
				name = m.Name
				return true, ""
			}
			seen = append(seen, m.Name+" "+m.Status.Phase+" "+podTrouble(t, namespace, m.Name))
		}
		return false, strings.Join(seen, "; ")
	})
	return name
}

// keyWorldAs asks keyWorld on a server session bound to the member's pod. The
// member's own agent holds a session too and takes it back within seconds,
// and the answer goes to whichever stream is current, so the request is
// repeated until one arrives here; the operator answers the same key again
// as a success.
func keyWorldAs(t *testing.T, namespace, server, key string) string {
	t.Helper()
	pod := getPod(t, namespace, server)
	var world string
	eventually(t, 2*time.Minute, "keyWorld to be answered", func() (bool, string) {
		conn, closeConn := dialAgentPort(t)
		defer closeConn()
		streamCtx := metadata.AppendToOutgoingContext(ctx, "authorization",
			"Bearer "+agentToken(t, pod, podspec.ServerServiceAccountName))
		stream, err := agentpb.NewAgentServiceClient(conn).ServerSession(streamCtx)
		if err != nil {
			return false, err.Error()
		}
		id := uint64(time.Now().UnixNano())
		if err := stream.Send(&agentpb.ServerMessage{Message: &agentpb.ServerMessage_CloudRequest{
			CloudRequest: &agentpb.CloudRequest{Id: id, Request: &agentpb.CloudRequest_KeyWorld{
				KeyWorld: &agentpb.KeyWorldRequest{Key: key},
			}},
		}}); err != nil {
			return false, "send: " + err.Error()
		}
		type received struct {
			msg *agentpb.OperatorToServer
			err error
		}
		ch := make(chan received, 1)
		go func() {
			for {
				msg, err := stream.Recv()
				if err != nil || msg.GetCloudResponse().GetId() == id {
					ch <- received{msg, err}
					return
				}
			}
		}()
		select {
		case r := <-ch:
			if r.err != nil {
				return false, "session ended: " + r.err.Error()
			}
			resp := r.msg.GetCloudResponse()
			if resp.GetKeyWorld() == nil {
				t.Fatalf("keyWorld %q for %s was answered %v", key, server, resp.GetError())
			}
			world = resp.GetKeyWorld().GetWorld()
			return true, ""
		case <-time.After(20 * time.Second):
			return false, "no answer within 20s"
		}
	})
	return world
}
