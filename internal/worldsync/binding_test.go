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
	"context"
	"errors"
	"testing"
	"time"
)

func TestWriteBindingNamesTheBindingThatIsThere(t *testing.T) {
	st := NewMemStore(time.Now)
	ctx := context.Background()
	old := Binding{Server: "g-wxyz", Pod: "ns/g-wxyz"}
	if err := WriteBinding(ctx, st, "", kept, old); err != nil {
		t.Fatal(err)
	}
	if err := WriteBinding(ctx, st, "", kept, old); err != nil {
		t.Fatalf("the same binding again: %v", err)
	}
	err := WriteBinding(ctx, st, "", kept, Binding{Server: "g-abcd", Pod: "ns/g-abcd"})
	var bound *BoundError
	if !errors.As(err, &bound) || bound.Have != old || !errors.Is(err, ErrBound) {
		t.Fatalf("err = %v, want a BoundError naming %+v", err, old)
	}
}

func TestReplaceBindingOnlyWhileNothingTookTheWorld(t *testing.T) {
	old := Binding{Server: "g-wxyz", Pod: "ns/g-wxyz"}
	mine := Binding{Server: "g-abcd", Pod: "ns/g-abcd"}
	for name, tc := range map[string]struct {
		setup   func(t *testing.T, st *MemStore)
		stale   Binding
		replace bool
	}{
		"binding only": {stale: old, replace: true},
		"a lease is there": {stale: old, setup: func(t *testing.T, st *MemStore) {
			_, _ = put(t, st, WorldPrefix("", kept)+LeaseName, "{}", PutCondition{})
		}},
		"a manifest": {stale: old, setup: func(t *testing.T, st *MemStore) {
			_, _ = put(t, st, WorldPrefix("", kept)+ManifestName, "{}", PutCondition{})
		}},
		"it changed since": {stale: Binding{Server: "g-other", Pod: "ns/g-other"}},
	} {
		t.Run(name, func(t *testing.T) {
			st := NewMemStore(time.Now)
			ctx := context.Background()
			if err := WriteBinding(ctx, st, "", kept, old); err != nil {
				t.Fatal(err)
			}
			if tc.setup != nil {
				tc.setup(t, st)
			}
			err := ReplaceBinding(ctx, st, "", kept, tc.stale, mine)
			have, rerr := ReadBinding(ctx, st, WorldPrefix("", kept))
			if rerr != nil {
				t.Fatal(rerr)
			}
			if tc.replace {
				if err != nil || have != mine {
					t.Fatalf("err = %v, binding = %+v, want %+v", err, have, mine)
				}
				return
			}
			if !errors.Is(err, ErrBound) || have != old {
				t.Fatalf("err = %v, binding = %+v: want ErrBound and the old binding", err, have)
			}
		})
	}
}
