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

import "github.com/spawnery/spawnery/internal/agentpb"

// Publisher delivers an event to the sessions of one kind of agent.
type Publisher interface {
	Publish(namespace string, ev *agentpb.CloudEvent)
}

// Fanout sends an event to the backends and to the proxies, since an
// administrator may be on either; a private one goes to the proxies only.
type Fanout struct {
	Backends Publisher
	Proxies  Publisher
}

func (f Fanout) Publish(namespace string, ev *agentpb.CloudEvent, private bool) {
	if !private {
		f.Backends.Publish(namespace, ev)
	}
	f.Proxies.Publish(namespace, ev)
}
