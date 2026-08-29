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

// Package cloudevent turns what the operator records into what an
// administrator reads in chat.
//
// Its own package because three packages need it and none may import the
// others: internal/controller records the events, and internal/serverreg and
// internal/proxyreg deliver them. A copy in each would be the two independent
// derivations section 4.4 of the design exists to prevent.
package cloudevent

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agentpb"
)

// Derive turns one recorded event into at most one CloudEvent.
//
// **The note is carried verbatim.** It is the operator's own sentence, already
// written for a person, and rewording it here is exactly how a chat feed comes
// to disagree with `kubectl get events` about the same fact -- which is the
// property section 4.4 of the design is protecting.
//
// It reports ok=false for anything the feed cannot address. An object with no
// namespace has nowhere to go, and a kind this has never seen is one nobody
// decided was worth a chat line: defaulting to "show it" would make every
// event type added later a surprise in somebody's chat, which is how a feed
// becomes one people turn off.
func Derive(
	regarding runtime.Object, eventtype, reason, note string,
) (string, *agentpb.CloudEvent, bool) {
	var namespace, subject, group string
	switch o := regarding.(type) {
	case *spawneryv1alpha1.Server:
		namespace, subject, group = o.Namespace, o.Name, o.Spec.GroupRef.Name
	case *spawneryv1alpha1.ServerGroup:
		// Its own group, so that collapsing needs no special case for events
		// about groups.
		namespace, subject, group = o.Namespace, o.Name, o.Name
	case *spawneryv1alpha1.ProxyGroup:
		namespace, subject, group = o.Namespace, o.Name, o.Name
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
