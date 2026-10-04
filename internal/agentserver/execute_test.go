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

package agentserver

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/grpcauth"
)

// recordingFanout is a server fan-out whose live sessions a test chooses, and
// which hands every ExecuteCommand to onSend.
type recordingFanout struct {
	mu     sync.Mutex
	live   map[string]bool
	sent   []string
	onSend func(podUID string, cmd *agentpb.ExecuteCommand)
}

func (f *recordingFanout) Join(context.Context, string, string) (<-chan *agentpb.OperatorToServer, func(), error) {
	return nil, func() {}, nil
}

func (f *recordingFanout) SetInterest(string, bool) {}

func (f *recordingFanout) Send(podUID string, msg *agentpb.OperatorToServer) bool {
	f.mu.Lock()
	live := f.live[podUID]
	if live {
		f.sent = append(f.sent, podUID)
	}
	on := f.onSend
	f.mu.Unlock()
	if live && on != nil {
		on(podUID, msg.GetExecuteCommand())
	}
	return live
}

func network(execute bool) *spawneryv1alpha1.Network {
	return &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: "ns"},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "secret"},
			Commands:            &spawneryv1alpha1.NetworkCommands{Execute: execute},
		},
	}
}

func askExecute(s *Server, id grpcauth.Identity, target, command string) *agentpb.CloudResponse {
	return s.answerCloudRequest(context.Background(), logr.Discard(), id, &agentpb.CloudRequest{
		Id: 3, Request: &agentpb.CloudRequest_Execute{Execute: &agentpb.ExecuteRequest{
			Target: target, Command: command, Issuer: "alice",
		}},
	})
}

// answering makes every live server answer through the real receive path.
func answering(s *Server, fan *recordingFanout, reply func(podUID string, cmd *agentpb.ExecuteCommand) *agentpb.ExecuteOutcome) {
	fan.onSend = func(podUID string, cmd *agentpb.ExecuteCommand) {
		outcome := reply(podUID, cmd)
		if outcome == nil {
			return
		}
		go s.handle(context.Background(), logr.Discard(),
			grpcauth.Identity{Namespace: "ns", PodUID: podUID, Role: agent.RoleServer},
			&agentpb.ServerMessage{Message: &agentpb.ServerMessage_ExecuteOutcome{ExecuteOutcome: outcome}})
	}
}

func TestExecuteIsRefusedBeforeAnythingRuns(t *testing.T) {
	off, _, _ := commandFixture(t, network(false), readyServer("lobby-a", "lobby", "pod-a"))
	on, _, _ := commandFixture(t, network(true), readyServer("lobby-a", "lobby", "pod-a"))
	none, _, _ := commandFixture(t, readyServer("lobby-a", "lobby", "pod-a"))

	for name, c := range map[string]struct {
		s       *Server
		id      grpcauth.Identity
		target  string
		command string
		reason  agentpb.RequestError_Reason
		says    string
	}{
		"from a backend":       {on, serverCaller, "lobby", "list", agentpb.RequestError_REFUSED, "proxy"},
		"switch off":           {off, proxyCaller, "lobby", "list", agentpb.RequestError_REFUSED, "execute is not enabled on this network"},
		"no Network at all":    {none, proxyCaller, "lobby", "list", agentpb.RequestError_REFUSED, "execute is not enabled on this network"},
		"empty command":        {on, proxyCaller, "lobby", " / ", agentpb.RequestError_REFUSED, "no command"},
		"257 characters":       {on, proxyCaller, "lobby", strings.Repeat("x", 257), agentpb.RequestError_REFUSED, "256"},
		"nothing by that name": {on, proxyCaller, "nowhere", "list", agentpb.RequestError_NOT_FOUND, "no server or group"},
	} {
		got := askExecute(c.s, c.id, c.target, c.command).GetError()
		if got.GetReason() != c.reason || !strings.Contains(got.GetMessage(), c.says) {
			t.Errorf("%s: %v, want %s mentioning %q", name, got, c.reason, c.says)
		}
		if sent := c.s.opts.Servers.(*recordingFanout).sent; len(sent) != 0 {
			t.Errorf("%s: a command went out to %v", name, sent)
		}
	}
}

func TestExecuteOnOneServerBringsItsOutputBack(t *testing.T) {
	s, _, rec := commandFixture(t, network(true), readyServer("lobby-a", "lobby", "pod-a"))
	fan := s.opts.Servers.(*recordingFanout)
	fan.live["pod-a"] = true
	answering(s, fan, func(_ string, cmd *agentpb.ExecuteCommand) *agentpb.ExecuteOutcome {
		if cmd.GetCommand() != "list" {
			t.Errorf("command = %q, want list without the slash", cmd.GetCommand())
		}
		return &agentpb.ExecuteOutcome{Id: cmd.GetId(), Ok: true, Output: []string{"There are 0 of a max of 20 players online"}}
	})

	got := askExecute(s, proxyCaller, "lobby-a", "/list").GetExecute().GetOutcomes()

	if len(got) != 1 || got[0].GetServer() != "lobby-a" || !got[0].GetOk() || len(got[0].GetOutput()) != 1 {
		t.Fatalf("outcomes = %+v, want lobby-a ok with one line", got)
	}
	select {
	case ev := <-rec.Events:
		if !strings.Contains(ev, "CommandExecuted") || !strings.Contains(ev, "alice") || strings.Contains(ev, "players online") {
			t.Errorf("event %q: want CommandExecuted naming the issuer and never the output", ev)
		}
	default:
		t.Error("no event was recorded")
	}
}

func TestAServerThatDoesNotAnswerIsListedAndTheRestStillArrive(t *testing.T) {
	s, _, _ := commandFixture(t, network(true),
		readyServer("lobby-a", "lobby", "pod-a"), readyServer("lobby-b", "lobby", "pod-b"),
		readyServer("lobby-c", "lobby", "pod-c"))
	fan := s.opts.Servers.(*recordingFanout)
	fan.live["pod-a"], fan.live["pod-b"] = true, true
	answering(s, fan, func(podUID string, cmd *agentpb.ExecuteCommand) *agentpb.ExecuteOutcome {
		if podUID == "pod-b" {
			return nil
		}
		return &agentpb.ExecuteOutcome{Id: cmd.GetId(), Ok: true}
	})

	start := time.Now()
	got := askExecute(s, proxyCaller, "lobby", "say hi").GetExecute().GetOutcomes()

	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the answer took %s; the wait is 300ms", took)
	}
	if len(got) != 3 {
		t.Fatalf("outcomes = %+v, want all three: lobby-c has no session and is counted", got)
	}
	if got[0].GetServer() != "lobby-a" || !got[0].GetOk() {
		t.Errorf("first = %+v, want lobby-a ok", got[0])
	}
	if got[1].GetServer() != "lobby-b" || got[1].GetOk() || !strings.Contains(got[1].GetError(), "no answer within") {
		t.Errorf("second = %+v, want lobby-b with no answer", got[1])
	}
	if got[2].GetServer() != "lobby-c" || got[2].GetOk() || got[2].GetError() != "agent not connected" {
		t.Errorf("third = %+v, want lobby-c with agent not connected", got[2])
	}
}

func TestAGroupAnswerCarriesNoOutput(t *testing.T) {
	s, _, _ := commandFixture(t, network(true),
		readyServer("lobby-a", "lobby", "pod-a"), readyServer("lobby-b", "lobby", "pod-b"))
	fan := s.opts.Servers.(*recordingFanout)
	fan.live["pod-a"], fan.live["pod-b"] = true, true
	answering(s, fan, func(_ string, cmd *agentpb.ExecuteCommand) *agentpb.ExecuteOutcome {
		return &agentpb.ExecuteOutcome{Id: cmd.GetId(), Ok: true, Output: []string{"a line"}}
	})

	got := askExecute(s, proxyCaller, "lobby", "list").GetExecute().GetOutcomes()

	if len(got) != 2 {
		t.Fatalf("outcomes = %+v, want two", got)
	}
	for _, o := range got {
		if !o.GetOk() || len(o.GetOutput()) != 0 {
			t.Errorf("outcome %+v, want ok with no output", o)
		}
	}
}

func TestOneServerCannotAnswerForAnother(t *testing.T) {
	s, _, _ := commandFixture(t, network(true), readyServer("lobby-a", "lobby", "pod-a"))
	fan := s.opts.Servers.(*recordingFanout)
	fan.live["pod-a"] = true
	fan.onSend = func(_ string, cmd *agentpb.ExecuteCommand) {
		go s.handle(context.Background(), logr.Discard(),
			grpcauth.Identity{Namespace: "ns", PodUID: "pod-evil", Role: agent.RoleServer},
			&agentpb.ServerMessage{Message: &agentpb.ServerMessage_ExecuteOutcome{
				ExecuteOutcome: &agentpb.ExecuteOutcome{Id: cmd.GetId(), Ok: true, Output: []string{"forged"}},
			}})
	}

	got := askExecute(s, proxyCaller, "lobby-a", "list").GetExecute().GetOutcomes()

	if len(got) != 1 || got[0].GetOk() || len(got[0].GetOutput()) != 0 {
		t.Errorf("outcomes = %+v, want lobby-a unanswered: pod-evil answered for it", got)
	}
}

func TestANamedServerWithoutASessionIsUnavailable(t *testing.T) {
	s, _, _ := commandFixture(t, network(true), readyServer("lobby-a", "lobby", "pod-a"))
	if got := askExecute(s, proxyCaller, "lobby-a", "list").GetError(); got.GetReason() != agentpb.RequestError_UNAVAILABLE {
		t.Errorf("error = %v, want UNAVAILABLE", got)
	}
}

func TestAServersOutputIsBoundedAgainByTheOperator(t *testing.T) {
	s, _, _ := commandFixture(t, network(true), readyServer("lobby-a", "lobby", "pod-a"))
	fan := s.opts.Servers.(*recordingFanout)
	fan.live["pod-a"] = true
	answering(s, fan, func(_ string, cmd *agentpb.ExecuteCommand) *agentpb.ExecuteOutcome {
		lines := make([]string, 50)
		for i := range lines {
			lines[i] = strings.Repeat("é", 400)
		}
		return &agentpb.ExecuteOutcome{Id: cmd.GetId(), Ok: true, Output: lines}
	})

	out := askExecute(s, proxyCaller, "lobby-a", "list").GetExecute().GetOutcomes()[0].GetOutput()

	if len(out) != ExecuteMaxLines {
		t.Errorf("%d lines, want %d", len(out), ExecuteMaxLines)
	}
	if n := len([]rune(out[0])); n != ExecuteMaxLineLength {
		t.Errorf("a line of %d characters, want %d, cut on a character boundary", n, ExecuteMaxLineLength)
	}
}
