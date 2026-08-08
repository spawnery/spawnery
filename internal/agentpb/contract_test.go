/*
Copyright The Spawnery Authors.

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

package agentpb_test

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/spawnery/spawnery/internal/agentpb"
)

// The service name is part of the wire contract: the Kotlin agent of
// milestone 2b addresses exactly this string.
func TestServiceName(t *testing.T) {
	if got := agentpb.AgentService_ServiceDesc.ServiceName; got != "spawnery.agent.v1alpha1.AgentService" {
		t.Errorf("ServiceName = %q", got)
	}
}

func TestBothStreamsAreBidirectional(t *testing.T) {
	for _, s := range agentpb.AgentService_ServiceDesc.Streams {
		if !s.ClientStreams || !s.ServerStreams {
			t.Errorf("%s is not bidirectional: client=%v server=%v",
				s.StreamName, s.ClientStreams, s.ServerStreams)
		}
	}
	if len(agentpb.AgentService_ServiceDesc.Streams) != 2 {
		t.Errorf("got %d streams, want ProxySession and ServerSession",
			len(agentpb.AgentService_ServiceDesc.Streams))
	}
}

// An unknown oneof branch must survive a round trip untouched, because a
// newer agent talking to an older operator has to keep working.
func TestServerMessageRoundTrip(t *testing.T) {
	in := &agentpb.ServerMessage{
		Message: &agentpb.ServerMessage_PlayerCount{
			PlayerCount: &agentpb.PlayerCount{Players: 7, Slots: 100},
		},
	}
	raw, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := &agentpb.ServerMessage{}
	if err := proto.Unmarshal(raw, out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, ok := out.GetMessage().(*agentpb.ServerMessage_PlayerCount)
	if !ok {
		t.Fatalf("branch = %T, want PlayerCount", out.GetMessage())
	}
	if got.PlayerCount.GetPlayers() != 7 || got.PlayerCount.GetSlots() != 100 {
		t.Errorf("got %d/%d, want 7/100", got.PlayerCount.GetPlayers(), got.PlayerCount.GetSlots())
	}
}
