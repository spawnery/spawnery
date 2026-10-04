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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/podspec"
)

// The real session code: an execute waits off the proxy's loop, so a status
// request sent after it is answered first, while the server has not replied.
func TestAnExecuteWaitsWithoutHoldingTheProxysOtherAnswers(t *testing.T) {
	f := newServerFixture(t)
	if err := f.c.Create(f.ctx, &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: f.ns},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "secret"},
			Commands:            &spawneryv1alpha1.NetworkCommands{Execute: true},
		},
	}); err != nil {
		t.Fatalf("create Network: %v", err)
	}
	pod := f.pod("lobby-aaaa")
	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-aaaa", Namespace: f.ns},
		Spec:       spawneryv1alpha1.ServerSpec{GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"}},
	}
	if err := f.c.Create(f.ctx, srv); err != nil {
		t.Fatalf("create Server: %v", err)
	}
	srv.Status = spawneryv1alpha1.ServerStatus{Phase: "Ready", PodName: pod.Name, PodUID: string(pod.UID)}
	if err := f.c.Status().Update(f.ctx, srv); err != nil {
		t.Fatalf("Server status: %v", err)
	}

	server, closeServer := dialAgent(t, f.ctx, f.addr, f.ca,
		f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, pod))
	defer closeServer()
	proxy, closeProxy := dialProxy(t, f.ctx, f.addr, f.ca,
		f.token(podspec.ProxyServiceAccountName, []string{podspec.AgentTokenAudience}, f.proxyPod("gateway-0")))
	defer closeProxy()

	// The network state comes through the fan-out, so the session has joined it.
	for {
		msg, err := server.Recv()
		if err != nil {
			t.Fatalf("server Recv: %v", err)
		}
		if msg.GetNetworkState() != nil {
			break
		}
	}

	send := func(id uint64, req *agentpb.CloudRequest) {
		req.Id = id
		if err := proxy.Send(&agentpb.ProxyMessage{Message: &agentpb.ProxyMessage_CloudRequest{CloudRequest: req}}); err != nil {
			t.Fatalf("send %d: %v", id, err)
		}
	}
	send(1, &agentpb.CloudRequest{Request: &agentpb.CloudRequest_Execute{Execute: &agentpb.ExecuteRequest{
		Target: "lobby-aaaa", Command: "list", Issuer: "alice"}}})
	send(2, &agentpb.CloudRequest{Request: &agentpb.CloudRequest_Status{Status: &agentpb.StatusRequest{}}})

	var command *agentpb.ExecuteCommand
	for command == nil {
		msg, err := server.Recv()
		if err != nil {
			t.Fatalf("server Recv: %v", err)
		}
		command = msg.GetExecuteCommand()
	}

	var order []uint64
	answered := func() {
		for {
			msg, err := proxy.Recv()
			if err != nil {
				t.Fatalf("proxy Recv: %v", err)
			}
			if resp := msg.GetCloudResponse(); resp != nil {
				order = append(order, resp.GetId())
				if resp.GetId() == 1 {
					outcomes := resp.GetExecute().GetOutcomes()
					if len(outcomes) != 1 || !outcomes[0].GetOk() || outcomes[0].GetOutput()[0] != "nobody" {
						t.Errorf("execute answer = %+v, want lobby-aaaa ok with its line", resp)
					}
				}
				return
			}
		}
	}
	answered()
	if err := server.Send(&agentpb.ServerMessage{Message: &agentpb.ServerMessage_ExecuteOutcome{
		ExecuteOutcome: &agentpb.ExecuteOutcome{Id: command.GetId(), Ok: true, Output: []string{"nobody"}},
	}}); err != nil {
		t.Fatalf("server Send: %v", err)
	}
	answered()

	if len(order) != 2 || order[0] != 2 || order[1] != 1 {
		t.Errorf("answers arrived in order %v, want the status (2) before the execute (1)", order)
	}
}
