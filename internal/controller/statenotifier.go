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

package controller

import (
	"sync"

	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/types"

	"github.com/spawnery/spawnery/internal/agentpb"
)

// StateNotifier is told when a server changed in a way the proxies show, so
// they get a fresh picture now and not at the next resync.
type StateNotifier interface {
	Changed(namespace string)
}

// visibleMemo remembers what each server last looked like to the proxies, so
// a reconcile that changed nothing they show says nothing. The zero value is
// ready to use.
type visibleMemo struct {
	mu      sync.Mutex
	entries map[types.NamespacedName]*agentpb.ServerState
}

// note stores state and reports whether it differs from the last one, which
// includes a server never seen before.
func (m *visibleMemo) note(key types.NamespacedName, state *agentpb.ServerState) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if prev, ok := m.entries[key]; ok && proto.Equal(prev, state) {
		return false
	}
	if m.entries == nil {
		m.entries = map[types.NamespacedName]*agentpb.ServerState{}
	}
	m.entries[key] = state
	return true
}

// forget drops a server and reports whether it was known.
func (m *visibleMemo) forget(key types.NamespacedName) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.entries[key]
	delete(m.entries, key)
	return ok
}
