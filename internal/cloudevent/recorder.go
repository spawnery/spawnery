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

package cloudevent

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"

	"github.com/spawnery/spawnery/internal/agentpb"
)

// Sink is where derived events go. The two fan-outs implement it.
//
// One method, and no error: a feed nobody is watching must not be able to fail
// a reconcile. Delivery is best-effort by design -- see agentpb.CloudEvent.
type Sink interface {
	Publish(namespace string, ev *agentpb.CloudEvent)
}

// Recorder records an event to Kubernetes and derives a CloudEvent from the
// same call.
//
// **One seam and not thirty.** The operator records through this interface in
// thirty places across five controllers, and every one of them now feeds the
// chat without knowing it does. The alternative -- a second call beside each
// recorder call -- is thirty chances to forget, and forgetting is invisible:
// the Kubernetes event is still there, so nothing looks broken except that one
// kind of thing never appears in chat.
//
// It implements events.EventRecorder, so wrapping is a one-line change at each
// construction site and no call site changes at all.
type Recorder struct {
	// Inner is the manager's own recorder. Required.
	Inner events.EventRecorder
	// Sink is where the feed's copy goes. Nil means no feed, which is a state
	// and not a bug: a recorder may be built before the fan-outs exist.
	Sink Sink
}

// Eventf records, then derives.
//
// Kubernetes first, deliberately. If deriving ever panicked, the recorded
// event would already be queued -- and the feed is the half this project can
// afford to lose.
//
// The note reaches Kubernetes unformatted with its args, exactly as it did
// before this wrapper existed, and is formatted only for the feed. Both sides
// therefore say the same sentence, which is what makes "the chat shows what
// kubectl shows" true of the text and not merely of the fact.
func (r Recorder) Eventf(
	regarding runtime.Object, related runtime.Object,
	eventtype, reason, action, note string, args ...interface{},
) {
	r.Inner.Eventf(regarding, related, eventtype, reason, action, note, args...)
	if r.Sink == nil {
		return
	}
	// Only when there are args, so a note carrying a bare percent sign is not
	// mangled by a Sprintf that has nothing to substitute.
	//
	// **Deliberately untested, and that is worth stating.** `go vet` is the
	// real defence here and it is thorough: it refuses such a note at a direct
	// call site, through a variadic forwarder like internal/certs/events.go's,
	// and as a non-constant format string. Every way of writing the test is
	// something vet will not let compile, which is the same as saying the case
	// cannot reach this line in a repository whose CI runs vet. The branch
	// stays because it is one line and vet is a lint rather than a compiler.
	formatted := note
	if len(args) > 0 {
		formatted = fmt.Sprintf(note, args...)
	}
	if namespace, ev, ok := Derive(regarding, eventtype, reason, formatted); ok {
		r.Sink.Publish(namespace, ev)
	}
}
