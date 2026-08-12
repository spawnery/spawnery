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

// Three settings across two files carry the words "online mode", and only one
// of them moves with ProxyGroup.spec.config.onlineMode. Turning the proxy's
// off is a decision about whether players are authenticated; it says nothing
// about whether the backends trust what the proxy forwards, because modern
// forwarding works identically either way. A future edit that "made the two
// agree" would hand every player an offline-mode UUID even on an
// online-mode network and detach them from their own inventories, and no test
// of either flavour on its own would notice.
func TestAnOfflineModeProxyChangesNothingOnTheBackend(t *testing.T) {
	off := false
	v := velocityValues()
	v.OnlineMode = &off

	proxy, err := Velocity(v, testSecretPath, nil)
	if err != nil {
		t.Fatalf("Velocity: %v", err)
	}
	if !strings.Contains(string(proxy["velocity.toml"]), "online-mode = false") {
		t.Fatalf("the proxy is not in offline mode, so this test proves nothing:\n%s", proxy["velocity.toml"])
	}

	backend, err := Paper(paperValues(), "s3cret", nil)
	if err != nil {
		t.Fatalf("Paper: %v", err)
	}
	global := string(backend["config/paper-global.yml"])
	if !strings.Contains(global, "online-mode: true") {
		t.Errorf("proxies.velocity.online-mode is not true; the backend stopped trusting the forwarded identity and every player gets an offline-mode UUID:\n%s", global)
	}
	props := string(backend["server.properties"])
	if !strings.Contains(props, "online-mode=false") {
		t.Errorf("the backend started authenticating players itself, which breaks every forwarded join:\n%s", props)
	}
}
