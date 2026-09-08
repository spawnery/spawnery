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

package podspec

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

func networkAllowing(policy *spawneryv1alpha1.SchedulingPolicy) *spawneryv1alpha1.Network {
	return &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: "minecraft"},
		Spec:       spawneryv1alpha1.NetworkSpec{Scheduling: policy},
	}
}

func TestSchedulingRefusalCoversEveryWayOffTheNamespace(t *testing.T) {
	allowed := &spawneryv1alpha1.SchedulingPolicy{
		AllowedTolerationKeys:     []string{"spawnery.cloud/game"},
		AllowedNodeSelectorKeys:   []string{"topology.kubernetes.io/zone"},
		AllowedAffinityNamespaces: []string{"minecraft-shared"},
	}
	for _, tc := range []struct {
		name       string
		network    *spawneryv1alpha1.Network
		scheduling *spawneryv1alpha1.Scheduling
		want       string // substring of the refusal; empty means accepted
	}{
		{"nothing asked, nothing allowed", networkAllowing(nil), nil, ""},
		{"empty scheduling, nothing allowed", networkAllowing(nil), &spawneryv1alpha1.Scheduling{}, ""},
		{"a toleration on a network that allows no scheduling", networkAllowing(nil),
			&spawneryv1alpha1.Scheduling{Tolerations: []corev1.Toleration{{Key: "spawnery.cloud/game", Operator: corev1.TolerationOpExists}}},
			"spec.scheduling names what a group may ask"},
		{"an allowed toleration", networkAllowing(allowed),
			&spawneryv1alpha1.Scheduling{Tolerations: []corev1.Toleration{{Key: "spawnery.cloud/game", Operator: corev1.TolerationOpExists}}},
			""},
		{"a toleration on a key the network does not name", networkAllowing(allowed),
			&spawneryv1alpha1.Scheduling{Tolerations: []corev1.Toleration{{Key: "node-role.kubernetes.io/control-plane", Operator: corev1.TolerationOpExists}}},
			`"node-role.kubernetes.io/control-plane" is not in network "production"'s spec.scheduling.allowedTolerationKeys`},
		{"a toleration with no key tolerates everything", networkAllowing(allowed),
			&spawneryv1alpha1.Scheduling{Tolerations: []corev1.Toleration{{Operator: corev1.TolerationOpExists}}},
			"tolerates every taint"},
		{"an allowed nodeSelector key", networkAllowing(allowed),
			&spawneryv1alpha1.Scheduling{NodeSelector: map[string]string{"topology.kubernetes.io/zone": "a"}},
			""},
		{"a nodeSelector on a control-plane label", networkAllowing(allowed),
			&spawneryv1alpha1.Scheduling{NodeSelector: map[string]string{"node-role.kubernetes.io/control-plane": ""}},
			"allowedNodeSelectorKeys"},
		{"node affinity on a key the network does not name", networkAllowing(allowed),
			&spawneryv1alpha1.Scheduling{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpIn, Values: []string{"cp-1"}}},
				}}},
			}}},
			`node affinity key "kubernetes.io/hostname"`},
		{"preferred node affinity is checked like required", networkAllowing(allowed),
			&spawneryv1alpha1.Scheduling{Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
				PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{{Weight: 1, Preference: corev1.NodeSelectorTerm{
					MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{"cp-1"}}},
				}}},
			}}},
			`node affinity key "metadata.name"`},
		{"pod affinity in the group's own namespace", networkAllowing(allowed),
			&spawneryv1alpha1.Scheduling{Affinity: &corev1.Affinity{PodAffinity: &corev1.PodAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "kubernetes.io/hostname", Namespaces: []string{"minecraft"}}},
			}}},
			""},
		{"pod affinity in an allowed namespace", networkAllowing(allowed),
			&spawneryv1alpha1.Scheduling{Affinity: &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
				PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{Weight: 1, PodAffinityTerm: corev1.PodAffinityTerm{TopologyKey: "kubernetes.io/hostname", Namespaces: []string{"minecraft-shared"}}}},
			}}},
			""},
		{"pod affinity naming a foreign namespace", networkAllowing(allowed),
			&spawneryv1alpha1.Scheduling{Affinity: &corev1.Affinity{PodAffinity: &corev1.PodAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "kubernetes.io/hostname", Namespaces: []string{"kube-system"}}},
			}}},
			`namespace "kube-system"`},
		{"pod affinity with a namespaceSelector", networkAllowing(allowed),
			&spawneryv1alpha1.Scheduling{Affinity: &corev1.Affinity{PodAffinity: &corev1.PodAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{TopologyKey: "kubernetes.io/hostname", NamespaceSelector: &metav1.LabelSelector{}}},
			}}},
			"namespaceSelector"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message, ok := SchedulingRefusal(tc.network, tc.scheduling, "minecraft")
			if tc.want == "" {
				if !ok {
					t.Errorf("refused: %s", message)
				}
				return
			}
			if ok {
				t.Fatal("accepted")
			}
			if !strings.Contains(message, tc.want) {
				t.Errorf("refusal = %q, want it to contain %q", message, tc.want)
			}
		})
	}
}

func TestEffectiveSchedulingIsTheGroupsOrElseTheNetworks(t *testing.T) {
	net := &spawneryv1alpha1.Network{Spec: spawneryv1alpha1.NetworkSpec{Defaults: &spawneryv1alpha1.Defaults{
		Scheduling: &spawneryv1alpha1.Scheduling{NodeSelector: map[string]string{"from": "network"}},
	}}}
	if got := EffectiveScheduling(net, nil); got == nil || got.NodeSelector["from"] != "network" {
		t.Errorf("EffectiveScheduling(nil group) = %+v, want the network default", got)
	}
	own := &spawneryv1alpha1.Scheduling{}
	if got := EffectiveScheduling(net, own); got != own {
		t.Error("a group's own scheduling, even empty, did not replace the network default wholesale")
	}
	if got := EffectiveScheduling(&spawneryv1alpha1.Network{}, nil); got != nil {
		t.Errorf("EffectiveScheduling(no defaults) = %+v, want nil", got)
	}
}

func TestHostPortRefusalNeedsARangeAndStaysInIt(t *testing.T) {
	if message, ok := HostPortRefusal(networkAllowing(nil), 30565); ok {
		t.Error("a host port was accepted by a network with no range")
	} else if !strings.Contains(message, "hostPortRange is unset") {
		t.Errorf("refusal = %q, want it to name the field", message)
	}
	ranged := networkAllowing(&spawneryv1alpha1.SchedulingPolicy{HostPortRange: &spawneryv1alpha1.PortRange{Min: 30000, Max: 30100}})
	if message, ok := HostPortRefusal(ranged, 30565); ok {
		t.Error("a port outside the range was accepted")
	} else if !strings.Contains(message, "30000-30100") {
		t.Errorf("refusal = %q, want it to name the range", message)
	}
	for _, port := range []int32{30000, 30050, 30100} {
		if message, ok := HostPortRefusal(ranged, port); !ok {
			t.Errorf("port %d inside the range was refused: %s", port, message)
		}
	}
}
