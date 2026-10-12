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

package v1alpha1

import "testing"

func TestVoiceBackendPortIsOffWithoutTheBlock(t *testing.T) {
	n := &Network{}
	if port, on := n.VoiceBackendPort(); on || port != 0 {
		t.Fatalf("VoiceBackendPort() = %d, %v; want 0, false", port, on)
	}
}

func TestVoiceBackendPortDefaultsForAnObjectThatSkippedDefaulting(t *testing.T) {
	n := &Network{Spec: NetworkSpec{Voice: &NetworkVoiceSpec{}}}
	if port, on := n.VoiceBackendPort(); !on || port != DefaultVoiceBackendPort {
		t.Fatalf("VoiceBackendPort() = %d, %v; want %d, true", port, on, DefaultVoiceBackendPort)
	}
}

func TestVoiceBackendPortReadsTheSpec(t *testing.T) {
	n := &Network{Spec: NetworkSpec{Voice: &NetworkVoiceSpec{BackendPort: 24500}}}
	if port, on := n.VoiceBackendPort(); !on || port != 24500 {
		t.Fatalf("VoiceBackendPort() = %d, %v; want 24500, true", port, on)
	}
}

func TestVoiceExposeFollowsTheGameWithoutAnOverride(t *testing.T) {
	g := &ProxyGroup{Spec: ProxyGroupSpec{Expose: ExposeSpec{Type: ExposeLoadBalancer}}}
	got, own := g.VoiceExpose()
	if own || got.Type != ExposeLoadBalancer {
		t.Fatalf("VoiceExpose() = %+v, %v; want the game's LoadBalancer, false", got, own)
	}
	g.Spec.Voice = &ProxyVoiceSpec{}
	if got, own = g.VoiceExpose(); own || got.Type != ExposeLoadBalancer {
		t.Fatalf("an empty voice block must still follow the game; got %+v, %v", got, own)
	}
}

func TestVoiceExposeTakesTheOverride(t *testing.T) {
	g := &ProxyGroup{Spec: ProxyGroupSpec{
		Expose: ExposeSpec{Type: ExposeLoadBalancer},
		Voice: &ProxyVoiceSpec{Expose: &ExposeSpec{
			Type: ExposeHostPort, HostPort: &HostPortSpec{Port: 25565},
		}},
	}}
	got, own := g.VoiceExpose()
	if !own || got.Type != ExposeHostPort || got.HostPort.Port != 25565 {
		t.Fatalf("VoiceExpose() = %+v, %v; want the HostPort override, true", got, own)
	}
}
