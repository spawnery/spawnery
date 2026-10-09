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
	"sync"
)

// ReadRetention reads a group's policy; a missing file is the zero policy.
// With the ETag of the last read it asks with a HEAD first and answers
// changed false without reading the file again.
func ReadRetention(ctx context.Context, st Store, base, namespace, group, etag string) (r Retention, newETag string, changed bool, err error) {
	key := RetentionKey(base, namespace, group)
	if etag != "" {
		info, err := st.Head(ctx, key)
		if errors.Is(err, ErrNotFound) {
			return Retention{}, "", true, nil
		}
		if err != nil {
			return Retention{}, "", false, err
		}
		if info.ETag == etag {
			return Retention{}, etag, false, nil
		}
	}
	b, info, err := readObject(ctx, st, key)
	if errors.Is(err, ErrNotFound) {
		return Retention{}, "", true, nil
	}
	if err != nil {
		return Retention{}, "", false, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return Retention{}, "", false, fmt.Errorf("decode %s: %w", key, err)
	}
	return r, info.ETag, true, nil
}

func WriteRetention(ctx context.Context, st Store, base, namespace, group string, r Retention) error {
	key := RetentionKey(base, namespace, group)
	if r == (Retention{}) {
		return st.Delete(ctx, key)
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = st.Put(ctx, key, bytes.NewReader(b), PutCondition{})
	return err
}

// Policies remembers what it wrote per group, so that the reconciler's
// passes over an unchanged policy cost no request.
type Policies struct {
	Store Store
	Base  string

	mu      sync.Mutex
	written map[string]Retention
}

func (p *Policies) Sync(ctx context.Context, namespace, group string, r Retention) error {
	k := namespace + "/" + group
	p.mu.Lock()
	last, known := p.written[k]
	p.mu.Unlock()
	if known && last == r {
		return nil
	}
	if err := WriteRetention(ctx, p.Store, p.Base, namespace, group, r); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.written == nil {
		p.written = map[string]Retention{}
	}
	p.written[k] = r
	return nil
}
