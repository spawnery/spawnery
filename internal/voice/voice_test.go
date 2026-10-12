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

package voice

import (
	"testing"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

func voiceNetwork() *spawneryv1alpha1.Network {
	return &spawneryv1alpha1.Network{Spec: spawneryv1alpha1.NetworkSpec{
		Voice: &spawneryv1alpha1.NetworkVoiceSpec{BackendPort: 24454},
	}}
}

func group(game spawneryv1alpha1.ExposeSpec, override *spawneryv1alpha1.ExposeSpec, voiceAddress string) *spawneryv1alpha1.ProxyGroup {
	g := &spawneryv1alpha1.ProxyGroup{}
	g.Spec.Expose = game
	if override != nil {
		g.Spec.Voice = &spawneryv1alpha1.ProxyVoiceSpec{Expose: override}
	}
	g.Status.VoiceAddress = voiceAddress
	return g
}

var (
	lb       = spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeLoadBalancer}
	nodePort = spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeNodePort,
		NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30765}}
	hostPort = spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeHostPort,
		HostPort: &spawneryv1alpha1.HostPortSpec{Port: 25577}}
	clusterIP = spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeClusterIP,
		ClusterIP: &spawneryv1alpha1.ClusterIPSpec{Address: "voice.example.test:24000"}}
)

func TestAnnounce(t *testing.T) {
	cases := []struct {
		name  string
		g     *spawneryv1alpha1.ProxyGroup
		want  Endpoint
		known bool
	}{
		{"LoadBalancer, game's way: port only", group(lb, nil, "203.0.113.7:25565"), Endpoint{Port: 25565}, true},
		{"NodePort, game's way: the node port, port only", group(nodePort, nil, "192.0.2.1:30765"), Endpoint{Port: 30765}, true},
		{"HostPort, game's way: the host port, port only", group(hostPort, nil, "192.0.2.1:25577"), Endpoint{Port: 25577}, true},
		{"ClusterIP, game's way: the game address's port, port only", group(clusterIP, nil, "voice.example.test:25565"), Endpoint{Port: 24000}, true},
		{"HostPort override: own node", group(lb, &hostPort, "192.0.2.1:25577"), Endpoint{Port: 25577, OwnNode: true}, true},
		{"NodePort override: own node", group(lb, &nodePort, "192.0.2.1:30765"), Endpoint{Port: 30765, OwnNode: true}, true},
		{"LoadBalancer override: the voice Service's ingress", group(nodePort, &lb, "198.51.100.4:25565"), Endpoint{Host: "198.51.100.4", Port: 25565}, true},
		{"LoadBalancer override, no ingress yet: unknown", group(nodePort, &lb, ""), Endpoint{}, false},
		{"ClusterIP override: the address as written", group(lb, &clusterIP, "voice.example.test:24000"), Endpoint{Host: "voice.example.test", Port: 24000}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, known := Announce(voiceNetwork(), c.g)
			if known != c.known || got != c.want {
				t.Fatalf("Announce() = %+v, %v; want %+v, %v", got, known, c.want, c.known)
			}
		})
	}
}

func TestAnnounceIsOffWithoutNetworkVoice(t *testing.T) {
	if _, known := Announce(&spawneryv1alpha1.Network{}, group(lb, nil, "203.0.113.7:25565")); known {
		t.Fatal("a network without spec.voice must announce nothing")
	}
}

func TestAClusterIPOverrideWithoutAPortMeans25565(t *testing.T) {
	bare := spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeClusterIP,
		ClusterIP: &spawneryv1alpha1.ClusterIPSpec{Address: "voice.example.test"}}
	got, known := Announce(voiceNetwork(), group(lb, &bare, "voice.example.test:25565"))
	if !known || got != (Endpoint{Host: "voice.example.test", Port: 25565}) {
		t.Fatalf("Announce() = %+v, %v", got, known)
	}
}
