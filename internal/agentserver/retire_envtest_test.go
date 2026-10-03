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

package agentserver_test

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/agentserver"
	"github.com/spawnery/spawnery/internal/podspec"
)

// Every test reads spec.retire back: an answer is not evidence that anything was patched.

func makeServer(t *testing.T, f *serverFixture, name string) {
	t.Helper()
	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"},
		},
	}
	if err := f.c.Create(f.ctx, srv); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
}

func retireOverTheWire(
	t *testing.T, f *serverFixture, pod *corev1.Pod, server string,
) *agentpb.CloudResponse {
	t.Helper()
	return askAsServerOverTheWire(t, f, pod, &agentpb.CloudRequest{
		Request: &agentpb.CloudRequest_Retire{Retire: &agentpb.RetireRequest{Server: server}},
	})
}

func unretireOverTheWire(
	t *testing.T, f *serverFixture, pod *corev1.Pod, server string,
) *agentpb.CloudResponse {
	t.Helper()
	return askAsServerOverTheWire(t, f, pod, &agentpb.CloudRequest{
		Request: &agentpb.CloudRequest_Unretire{Unretire: &agentpb.UnretireRequest{Server: server}},
	})
}

func askAsServerOverTheWire(
	t *testing.T, f *serverFixture, pod *corev1.Pod, req *agentpb.CloudRequest,
) *agentpb.CloudResponse {
	t.Helper()
	req.Id = 11
	stream, done := dialAgent(t, f.ctx, f.addr, f.ca,
		f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer done()
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("the opening message never arrived: %v", err)
	}
	if err := stream.Send(&agentpb.ServerMessage{
		Message: &agentpb.ServerMessage_CloudRequest{CloudRequest: req},
	}); err != nil {
		t.Fatalf("send the request: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		msg, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if resp := msg.GetCloudResponse(); resp != nil {
			if resp.GetId() != 11 {
				t.Fatalf("answered id %d, want the 11 the agent asked with", resp.GetId())
			}
			return resp
		}
		if time.Now().After(deadline) {
			t.Fatal("no CloudResponse arrived within ten seconds")
		}
	}
}

func retiring(t *testing.T, f *serverFixture, namespace, name string) bool {
	t.Helper()
	var srv spawneryv1alpha1.Server
	if err := f.c.Get(f.ctx, client.ObjectKey{Namespace: namespace, Name: name}, &srv); err != nil {
		t.Fatalf("get %s/%s: %v", namespace, name, err)
	}
	return srv.Spec.Retire
}

func TestRetireSetsTheFlagAndSaysSo(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-aaaa")
	makeServer(t, f, "lobby-aaaa")

	resp := retireOverTheWire(t, f, pod, "lobby-aaaa")
	if resp.GetRetire().GetServer() != "lobby-aaaa" {
		t.Fatalf("answer = %+v, want a RetireResult naming lobby-aaaa", resp.GetResult())
	}
	if !retiring(t, f, f.ns, "lobby-aaaa") {
		t.Error("the operator answered yes and spec.retire is still false")
	}
}

// Same name in two namespaces: with distinct names a cross-namespace write
// would fail to resolve and the test would pass for the wrong reason.
func TestRetireCannotReachAnotherNetworksServerOfTheSameName(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-aaaa")
	makeServer(t, f, "lobby-aaaa")

	other := "somewhere-else-" + f.ns
	if err := f.c.Create(f.ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: other},
	}); err != nil {
		t.Fatalf("create the other namespace: %v", err)
	}
	if err := f.c.Create(f.ctx, &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-aaaa", Namespace: other},
		Spec:       spawneryv1alpha1.ServerSpec{GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"}},
	}); err != nil {
		t.Fatalf("create the other server: %v", err)
	}

	retireOverTheWire(t, f, pod, "lobby-aaaa")

	if !retiring(t, f, f.ns, "lobby-aaaa") {
		t.Error("the agent's own server was not retired")
	}
	if retiring(t, f, other, "lobby-aaaa") {
		t.Error("a server in another namespace was retired by a name that matched")
	}
}

func TestRetiringAServerThisNetworkDoesNotHaveIsNotFound(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-aaaa")
	makeServer(t, f, "lobby-aaaa")

	resp := retireOverTheWire(t, f, pod, "a-server-nobody-has")
	if got := resp.GetError().GetReason(); got != agentpb.RequestError_NOT_FOUND {
		t.Fatalf("reason = %v, want NOT_FOUND", got)
	}
	if retiring(t, f, f.ns, "lobby-aaaa") {
		t.Error("a request naming a server that does not exist retired a different one")
	}
}

// REFUSED, not answered again: a second admin must be able to tell "I did
// this" from "somebody beat me to it".
func TestRetiringAnAlreadyRetiringServerIsRefused(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-aaaa")
	makeServer(t, f, "lobby-aaaa")

	if first := retireOverTheWire(t, f, pod, "lobby-aaaa"); first.GetRetire() == nil {
		t.Fatalf("the first ask was not answered with a RetireResult: %+v", first.GetResult())
	}
	second := retireOverTheWire(t, f, pod, "lobby-aaaa")
	if got := second.GetError().GetReason(); got != agentpb.RequestError_REFUSED {
		t.Fatalf("reason = %v, want REFUSED for a server that is already retiring", got)
	}
	if !retiring(t, f, f.ns, "lobby-aaaa") {
		t.Error("the second, refused ask cleared spec.retire")
	}
}

// Asserted through retire so a verb added outside the channel-wide bound fails here.
// One stream: the bucket refills a token a second, which separate dials would earn back.
func TestTheRateBoundCoversEveryVerbAndNotJustConnect(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-aaaa")
	makeServer(t, f, "lobby-aaaa")

	stream, done := dialAgent(t, f.ctx, f.addr, f.ca,
		f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer done()
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("the opening message never arrived: %v", err)
	}

	asks := agentserver.RequestBurst + 1
	for i := 0; i < asks; i++ {
		if err := stream.Send(&agentpb.ServerMessage{
			Message: &agentpb.ServerMessage_CloudRequest{
				CloudRequest: &agentpb.CloudRequest{
					Id: uint64(i + 1),
					Request: &agentpb.CloudRequest_Retire{
						// Unknown name: every ask costs a token and writes nothing.
						Retire: &agentpb.RetireRequest{Server: "a-server-nobody-has"},
					},
				},
			},
		}); err != nil {
			t.Fatalf("send ask %d: %v", i+1, err)
		}
	}

	// The stream also carries reports the operator sends on its own schedule.
	deadline := time.Now().Add(10 * time.Second)
	var last *agentpb.CloudResponse
	for last == nil {
		msg, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if resp := msg.GetCloudResponse(); resp != nil && resp.GetId() == uint64(asks) {
			last = resp
		}
		if time.Now().After(deadline) {
			t.Fatal("the last answer never arrived")
		}
	}
	if got := last.GetError().GetReason(); got != agentpb.RequestError_RATE_LIMITED {
		t.Fatalf("reason = %v on ask %d, want RATE_LIMITED past a burst of %d",
			got, asks, agentserver.RequestBurst)
	}
}

func TestUnretireOverTheWireHoldsTheServer(t *testing.T) {
	f := newServerFixture(t)
	pod := f.pod("lobby-aaaa")
	makeServer(t, f, "lobby-aaaa")
	retireOverTheWire(t, f, pod, "lobby-aaaa")

	resp := unretireOverTheWire(t, f, pod, "lobby-aaaa")
	if resp.GetUnretire().GetServer() != "lobby-aaaa" {
		t.Fatalf("answer = %+v, want an UnretireResult naming lobby-aaaa", resp.GetResult())
	}
	if retiring(t, f, f.ns, "lobby-aaaa") {
		t.Error("spec.retire is still true")
	}
}
