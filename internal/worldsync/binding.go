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

package worldsync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// BindingName is the object that ties a kept world to the server that keyed
// it. The operator writes it once, before the node agent may take the
// world's lease for that server; a game server can ask for a key, but only
// the operator says which server gets it.
const BindingName = "binding.json"

// ErrBound is a binding that names another server or pod.
var ErrBound = errors.New("worldsync: the world is bound to another server")

// Binding names the server, and the pod as "<namespace>/<name>", that keyed a
// world.
type Binding struct {
	Server string `json:"server"`
	Pod    string `json:"pod"`
}

func ReadBinding(ctx context.Context, st Store, prefix string) (Binding, error) {
	rc, _, err := st.Get(ctx, prefix+BindingName)
	if err != nil {
		return Binding{}, err
	}
	defer func() { _ = rc.Close() }()
	var b Binding
	if err := json.NewDecoder(rc).Decode(&b); err != nil {
		return Binding{}, fmt.Errorf("decode binding: %w", err)
	}
	return b, nil
}

// WriteBinding creates the binding, or accepts one that already says the
// same. It returns ErrBound for one that names another server or pod.
func WriteBinding(ctx context.Context, st Store, base, world string, b Binding) error {
	if err := checkWorld(world); err != nil {
		return err
	}
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	prefix := WorldPrefix(base, world)
	_, err = st.Put(ctx, prefix+BindingName, bytes.NewReader(data), PutCondition{IfNoneMatch: true})
	if !errors.Is(err, ErrPrecondition) {
		return err
	}
	have, err := ReadBinding(ctx, st, prefix)
	if err != nil {
		return err
	}
	if have != b {
		return fmt.Errorf("%w: %s (pod %s)", ErrBound, have.Server, have.Pod)
	}
	return nil
}

// Bind is agentserver.WorldDeleter's binding write.
func (b BucketWorlds) Bind(ctx context.Context, world string, binding Binding) error {
	return WriteBinding(ctx, b.Store, b.Base, world, binding)
}
