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

// Package cloudevent turns what the operator records into what an
// administrator reads in chat. It is its own package because internal/controller
// records the events and internal/serverreg and internal/proxyreg deliver them,
// and none of them may import the others.
package cloudevent

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/podspec"
)

// Derive turns one recorded event into at most one CloudEvent. The note is
// carried verbatim so the chat feed agrees with `kubectl get events`.
//
// It reports ok=false for an object with no namespace and for a kind it has
// never seen, so an event type added later does not surprise anybody's chat.
func Derive(
	regarding runtime.Object, eventtype, reason, note string,
) (string, *agentpb.CloudEvent, bool) {
	var namespace, subject, group string
	switch o := regarding.(type) {
	case *spawneryv1alpha1.Server:
		namespace, subject, group = o.Namespace, o.Name, o.Spec.GroupRef.Name
	case *spawneryv1alpha1.ServerGroup:
		// Its own group, so collapsing needs no special case.
		namespace, subject, group = o.Namespace, o.Name, o.Name
	case *spawneryv1alpha1.ProxyGroup:
		namespace, subject, group = o.Namespace, o.Name, o.Name
	case *corev1.Pod:
		if o.Labels[podspec.LabelRole] != podspec.RoleProxy {
			return "", nil, false
		}
		namespace, subject, group = o.Namespace, o.Name, o.Labels[podspec.LabelGroup]
	default:
		// Networks, Secrets, and anything added later.
		return "", nil, false
	}
	if namespace == "" || subject == "" {
		return "", nil, false
	}
	return namespace, &agentpb.CloudEvent{
		Kind:    reason,
		Subject: subject,
		Group:   group,
		Message: note,
		Warning: eventtype == corev1.EventTypeWarning,
	}, true
}
