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

package cloudevent

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"

	"github.com/spawnery/spawnery/internal/agentpb"
)

// Sink is where derived events go. No error: a feed nobody is watching must not
// fail a reconcile.
type Sink interface {
	Publish(namespace string, ev *agentpb.CloudEvent, private bool)
}

// Recorder records an event to Kubernetes and derives a CloudEvent from the
// same call, so no call site can forget to feed the chat. It implements
// events.EventRecorder, so wrapping changes no call site.
type Recorder struct {
	// Inner is the manager's own recorder. Required.
	Inner events.EventRecorder
	// Sink is where the feed's copy goes. Nil means no feed: a recorder may be
	// built before the fan-outs exist.
	Sink Sink
}

// Eventf records to Kubernetes first, so a panic while deriving cannot lose the
// event. The note reaches Kubernetes unformatted with its args and is
// formatted only for the feed.
func (r Recorder) Eventf(
	regarding runtime.Object, related runtime.Object,
	eventtype, reason, action, note string, args ...interface{},
) {
	r.Inner.Eventf(regarding, related, eventtype, reason, action, note, args...)
	if r.Sink == nil {
		return
	}
	// Only with args, so a bare percent sign is not mangled. Untested: go vet
	// refuses every way of writing such a note.
	formatted := note
	if len(args) > 0 {
		formatted = fmt.Sprintf(note, args...)
	}
	if namespace, ev, ok := Derive(regarding, eventtype, reason, formatted); ok {
		r.Sink.Publish(namespace, ev, Private(regarding))
	}
}
