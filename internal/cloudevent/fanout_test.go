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
	"testing"

	"github.com/spawnery/spawnery/internal/agentpb"
)

type recordingPublisher struct{ subjects []string }

func (r *recordingPublisher) Publish(_ string, ev *agentpb.CloudEvent) {
	r.subjects = append(r.subjects, ev.GetSubject())
}

func TestAPrivateEventReachesTheProxiesAndNoBackend(t *testing.T) {
	backends, proxies := &recordingPublisher{}, &recordingPublisher{}
	f := Fanout{Backends: backends, Proxies: proxies}

	f.Publish("minecraft", &agentpb.CloudEvent{Kind: "PodCreated", Subject: "challenge-3f2b1c9a"}, true)
	f.Publish("minecraft", &agentpb.CloudEvent{Kind: "PodCreated", Subject: "lobby-a3f9"}, false)

	if len(backends.subjects) != 1 || backends.subjects[0] != "lobby-a3f9" {
		t.Errorf("backends got %v, want only lobby-a3f9", backends.subjects)
	}
	if len(proxies.subjects) != 2 {
		t.Errorf("proxies got %v, want both events", proxies.subjects)
	}
}
