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

package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

func TestVoiceAddress(t *testing.T) {
	voiceNet := &spawneryv1alpha1.Network{Spec: spawneryv1alpha1.NetworkSpec{
		Voice: &spawneryv1alpha1.NetworkVoiceSpec{}}}
	lb := spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeLoadBalancer}
	nodePort := spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeNodePort,
		NodePort: &spawneryv1alpha1.NodePortSpec{Port: 30765}}

	withStatus := func(spec spawneryv1alpha1.ExposeSpec, override *spawneryv1alpha1.ExposeSpec, address string) *spawneryv1alpha1.ProxyGroup {
		g := groupExposing(spec)
		if override != nil {
			g.Spec.Voice = &spawneryv1alpha1.ProxyVoiceSpec{Expose: override}
		}
		g.Status.Address = address
		return g
	}

	cases := []struct {
		name     string
		net      *spawneryv1alpha1.Network
		g        *spawneryv1alpha1.ProxyGroup
		pods     []corev1.Pod
		voiceSvc *corev1.Service
		want     string
	}{
		{"voice off", &spawneryv1alpha1.Network{}, withStatus(lb, nil, "203.0.113.7:25565"),
			[]corev1.Pod{readyProxyPod("192.168.1.10", 0)}, nil, ""},
		{"game's LoadBalancer", voiceNet, withStatus(lb, nil, "203.0.113.7:25565"),
			[]corev1.Pod{readyProxyPod("192.168.1.10", 0)}, nil, "203.0.113.7:25565"},
		{"game's NodePort: same node port", voiceNet, withStatus(nodePort, nil, "192.168.1.10:30765"),
			[]corev1.Pod{readyProxyPod("192.168.1.10", 0)}, nil, "192.168.1.10:30765"},
		{"no game address yet: nothing", voiceNet, withStatus(lb, nil, ""),
			[]corev1.Pod{readyProxyPod("192.168.1.10", 0)}, nil, ""},
		{"LoadBalancer override: the voice Service's ingress", voiceNet, withStatus(nodePort, &lb, "192.168.1.10:30765"),
			[]corev1.Pod{readyProxyPod("192.168.1.10", 0)}, loadBalancerService("198.51.100.4", ""), "198.51.100.4:25565"},
		{"LoadBalancer override without ingress", voiceNet, withStatus(nodePort, &lb, "192.168.1.10:30765"),
			[]corev1.Pod{readyProxyPod("192.168.1.10", 0)}, loadBalancerService("", ""), ""},
		{"NodePort override without its nodePort block", voiceNet,
			withStatus(lb, &spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeNodePort}, "203.0.113.7:25565"),
			[]corev1.Pod{readyProxyPod("192.168.1.10", 0)}, nil, ""},
		{"HostPort override without its hostPort block", voiceNet,
			withStatus(lb, &spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeHostPort}, "203.0.113.7:25565"),
			[]corev1.Pod{readyProxyPod("192.168.1.10", 0)}, nil, ""},
		{"ClusterIP override without its clusterIP block", voiceNet,
			withStatus(lb, &spawneryv1alpha1.ExposeSpec{Type: spawneryv1alpha1.ExposeClusterIP}, "203.0.113.7:25565"),
			[]corev1.Pod{readyProxyPod("192.168.1.10", 0)}, clusterIPService(), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := voiceAddress(c.net, c.g, c.pods, c.voiceSvc); got != c.want {
				t.Fatalf("voiceAddress() = %q; want %q", got, c.want)
			}
		})
	}
}
