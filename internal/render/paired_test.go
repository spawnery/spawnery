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

package render

import (
	"strings"
	"testing"
)

// The one test whose absence would not show up as a failure anywhere else.
//
// Both halves look correct in isolation: a backend with online-mode=false is
// what forwarding needs, and a proxy with online-mode=true is what
// authentication needs. Swap them and everything still starts, every other
// test still passes, and the network is open — anyone can connect straight to
// a backend under any name. Only asserting both in one place says otherwise.
func TestOnlineModeIsOffOnTheBackendAndOnOnTheProxy(t *testing.T) {
	backend, err := Paper(paperValues(), "s3cret", nil)
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	proxy, err := Velocity(velocityValues(), testSecretPath, nil)
	if err != nil {
		t.Fatalf("Velocity: %v", err)
	}

	props := string(backend["server.properties"])
	if !strings.Contains(props, "online-mode=false") {
		t.Errorf("the backend authenticates players itself, which breaks every forwarded join:\n%s", props)
	}

	toml := string(proxy["velocity.toml"])
	if !strings.Contains(toml, "online-mode = true") {
		t.Errorf("the proxy does not authenticate players, so the whole network is offline-mode:\n%s", toml)
	}
}
