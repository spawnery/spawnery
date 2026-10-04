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

package agentpb_test

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

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

func TestOnDemandRequestsAreOnTheWire(t *testing.T) {
	req := &agentpb.CloudRequest{
		Id: 1,
		Request: &agentpb.CloudRequest_StartServer{
			StartServer: &agentpb.StartServerRequest{Group: "private-servers", Key: "c0ffee"},
		},
	}
	if req.GetStartServer().GetKey() != "c0ffee" {
		t.Fatal("the start_server arm does not carry the key")
	}
	if agentpb.GroupState_ON_DEMAND == agentpb.GroupState_KIND_UNSPECIFIED {
		t.Fatal("ON_DEMAND must be its own value, not the unspecified one")
	}
}

// Generated code is regenerated from the .proto on both sides, so an encoder
// and a decoder always agree with each other; only a number written down here
// notices one that moved.
func TestOnDemandFieldNumbersAreFixed(t *testing.T) {
	for _, c := range []struct {
		msg   proto.Message
		field protoreflect.Name
		want  protoreflect.FieldNumber
	}{
		{&agentpb.CloudRequest{}, "start_server", 8},
		{&agentpb.CloudRequest{}, "stop_server", 9},
		{&agentpb.CloudResponse{}, "start_server", 9},
		{&agentpb.CloudResponse{}, "stop_server", 10},
		{&agentpb.StartServerRequest{}, "group", 1},
		{&agentpb.StartServerRequest{}, "key", 2},
		{&agentpb.StartServerResult{}, "server", 1},
		{&agentpb.StartServerResult{}, "already_running", 2},
		{&agentpb.StopServerRequest{}, "server", 1},
		{&agentpb.StopServerResult{}, "server", 1},
	} {
		md := c.msg.ProtoReflect().Descriptor()
		fd := md.Fields().ByName(c.field)
		if fd == nil {
			t.Errorf("%s has no field %s", md.Name(), c.field)
			continue
		}
		if fd.Number() != c.want {
			t.Errorf("%s.%s is field %d, want %d: a renumbered field is a silent wire break",
				md.Name(), c.field, fd.Number(), c.want)
		}
	}
	if got := agentpb.GroupState_ON_DEMAND.Number(); got != 4 {
		t.Errorf("GroupState.ON_DEMAND is %d, want 4: a renumbered value is a silent wire break", got)
	}
}

func TestJoinPermissionFieldNumbersAreFixed(t *testing.T) {
	md := (&agentpb.GroupState{}).ProtoReflect().Descriptor()
	for name, want := range map[protoreflect.Name]protoreflect.FieldNumber{
		"join_permission":           11,
		"join_permission_deny_only": 12,
	} {
		fd := md.Fields().ByName(name)
		if fd == nil {
			t.Errorf("GroupState has no field %s", name)
			continue
		}
		if fd.Number() != want {
			t.Errorf("GroupState.%s is field %d, want %d: a renumbered field is a silent wire break", name, fd.Number(), want)
		}
	}
}

func TestCloudCommandFieldNumbersAreFixed(t *testing.T) {
	for _, c := range []struct {
		msg   proto.Message
		field protoreflect.Name
		want  protoreflect.FieldNumber
	}{
		{&agentpb.CloudRequest{}, "scale", 13},
		{&agentpb.CloudRequest{}, "force_stop", 14},
		{&agentpb.CloudRequest{}, "execute", 15},
		{&agentpb.CloudResponse{}, "scale", 14},
		{&agentpb.CloudResponse{}, "force_stop", 15},
		{&agentpb.CloudResponse{}, "execute", 16},
		{&agentpb.OperatorToServer{}, "execute_command", 6},
		{&agentpb.ServerMessage{}, "execute_outcome", 6},
		{&agentpb.GroupState{}, "pinned", 13},
		{&agentpb.GroupState{}, "pinned_replicas", 14},
		{&agentpb.GroupState{}, "pinned_until_unix", 15},
		{&agentpb.ScaleRequest{}, "group", 1},
		{&agentpb.ScaleRequest{}, "replicas", 2},
		{&agentpb.ScaleRequest{}, "duration_seconds", 3},
		{&agentpb.ScaleResult{}, "replicas", 1},
		{&agentpb.ScaleResult{}, "expires_at_unix", 2},
		{&agentpb.ForceStopRequest{}, "server", 1},
		{&agentpb.ForceStopRequest{}, "issuer", 2},
		{&agentpb.ForceStopResult{}, "server", 1},
		{&agentpb.ExecuteRequest{}, "target", 1},
		{&agentpb.ExecuteRequest{}, "command", 2},
		{&agentpb.ExecuteRequest{}, "issuer", 3},
		{&agentpb.ExecuteResult{}, "outcomes", 1},
		{&agentpb.ExecuteOutcome{}, "server", 1},
		{&agentpb.ExecuteOutcome{}, "ok", 2},
		{&agentpb.ExecuteOutcome{}, "output", 3},
		{&agentpb.ExecuteOutcome{}, "error", 4},
		{&agentpb.ExecuteOutcome{}, "id", 5},
		{&agentpb.ExecuteCommand{}, "id", 1},
		{&agentpb.ExecuteCommand{}, "command", 2},
	} {
		md := c.msg.ProtoReflect().Descriptor()
		fd := md.Fields().ByName(c.field)
		if fd == nil {
			t.Errorf("%s has no field %s", md.Name(), c.field)
			continue
		}
		if fd.Number() != c.want {
			t.Errorf("%s.%s is field %d, want %d: a renumbered field is a silent wire break",
				md.Name(), c.field, fd.Number(), c.want)
		}
	}
}
