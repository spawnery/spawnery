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
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// EffectiveScheduling is what a pod of the group gets: the group's own
// scheduling, or the network default where the group sets none. A group's
// scheduling replaces the default wholesale rather than merging with it;
// merging would make it impossible to drop an inherited nodeSelector.
func EffectiveScheduling(net *spawneryv1alpha1.Network, own *spawneryv1alpha1.Scheduling) *spawneryv1alpha1.Scheduling {
	if own != nil {
		return own
	}
	if net.Spec.Defaults != nil {
		return net.Spec.Defaults.Scheduling
	}
	return nil
}

// SchedulingRefusal reports whether the effective scheduling stays within
// what the network allows, and names the first thing that does not.
//
// The message names the key and the Network field that would allow it,
// because the person reading it is a group author who can write neither the
// taint nor the Network, and "not allowed" without the field sends them to
// look through the wrong object.
func SchedulingRefusal(
	network *spawneryv1alpha1.Network,
	scheduling *spawneryv1alpha1.Scheduling,
	namespace string,
) (string, bool) {
	if scheduling == nil ||
		(len(scheduling.Tolerations) == 0 && len(scheduling.NodeSelector) == 0 && scheduling.Affinity == nil) {
		return "", true
	}
	policy := network.Spec.Scheduling
	if policy == nil {
		return fmt.Sprintf("network %q allows no scheduling; its spec.scheduling names what a group may ask "+
			"of the scheduler, and it is unset", network.Name), false
	}
	for _, t := range scheduling.Tolerations {
		if t.Key == "" {
			return "a toleration with no key tolerates every taint, which no network allows", false
		}
		if !slices.Contains(policy.AllowedTolerationKeys, t.Key) {
			return fmt.Sprintf("toleration key %q is not in network %q's spec.scheduling.allowedTolerationKeys",
				t.Key, network.Name), false
		}
	}
	for key := range scheduling.NodeSelector {
		if !slices.Contains(policy.AllowedNodeSelectorKeys, key) {
			return fmt.Sprintf("nodeSelector key %q is not in network %q's spec.scheduling.allowedNodeSelectorKeys",
				key, network.Name), false
		}
	}
	if a := scheduling.Affinity; a != nil {
		if a.NodeAffinity != nil {
			var terms []corev1.NodeSelectorTerm
			if r := a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution; r != nil {
				terms = append(terms, r.NodeSelectorTerms...)
			}
			for _, p := range a.NodeAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
				terms = append(terms, p.Preference)
			}
			for _, term := range terms {
				for _, req := range append(slices.Clone(term.MatchExpressions), term.MatchFields...) {
					if !slices.Contains(policy.AllowedNodeSelectorKeys, req.Key) {
						return fmt.Sprintf("node affinity key %q is not in network %q's spec.scheduling.allowedNodeSelectorKeys",
							req.Key, network.Name), false
					}
				}
			}
		}
		var podTerms []corev1.PodAffinityTerm
		if a.PodAffinity != nil {
			podTerms = append(podTerms, a.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution...)
			for _, p := range a.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
				podTerms = append(podTerms, p.PodAffinityTerm)
			}
		}
		if a.PodAntiAffinity != nil {
			podTerms = append(podTerms, a.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution...)
			for _, p := range a.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
				podTerms = append(podTerms, p.PodAffinityTerm)
			}
		}
		for _, term := range podTerms {
			if term.NamespaceSelector != nil {
				return "a pod affinity term with a namespaceSelector can name any namespace, which no network allows", false
			}
			for _, ns := range term.Namespaces {
				if ns != namespace && !slices.Contains(policy.AllowedAffinityNamespaces, ns) {
					return fmt.Sprintf("pod affinity names namespace %q, which is neither this group's own nor in "+
						"network %q's spec.scheduling.allowedAffinityNamespaces", ns, network.Name), false
				}
			}
		}
	}
	return "", true
}

// HostPortRefusal reports whether a HostPort proxy's port lies in the
// network's range.
func HostPortRefusal(network *spawneryv1alpha1.Network, port int32) (string, bool) {
	policy := network.Spec.Scheduling
	if policy == nil || policy.HostPortRange == nil {
		return fmt.Sprintf("network %q allows no host port; its spec.scheduling.hostPortRange is unset", network.Name), false
	}
	r := policy.HostPortRange
	if port < r.Min || port > r.Max {
		return fmt.Sprintf("host port %d is outside network %q's spec.scheduling.hostPortRange %d-%d",
			port, network.Name, r.Min, r.Max), false
	}
	return "", true
}
