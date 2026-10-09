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
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type countingStore struct {
	*MemStore
	gets, heads, puts, deletes atomic.Int64
}

func (c *countingStore) Get(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error) {
	c.gets.Add(1)
	return c.MemStore.Get(ctx, key)
}

func (c *countingStore) Head(ctx context.Context, key string) (ObjectInfo, error) {
	c.heads.Add(1)
	return c.MemStore.Head(ctx, key)
}

func (c *countingStore) Put(ctx context.Context, key string, body io.ReadSeeker, cond PutCondition) (ObjectInfo, error) {
	c.puts.Add(1)
	return c.MemStore.Put(ctx, key, body, cond)
}

func (c *countingStore) Delete(ctx context.Context, key string) error {
	c.deletes.Add(1)
	return c.MemStore.Delete(ctx, key)
}

func TestAPolicyRoundTripsAndTheZeroPolicyIsNoFile(t *testing.T) {
	st := NewMemStore(time.Now)
	ctx := context.Background()
	want := Retention{Last: 12, Hourly: 24, Daily: 7, Weekly: 4, Monthly: 3}
	if err := WriteRetention(ctx, st, "base", "ns", "g", want); err != nil {
		t.Fatal(err)
	}
	if k := RetentionKey("base", "ns", "g"); k != "base/.retention/ns/g.json" {
		t.Fatalf("RetentionKey = %q", k)
	}
	got, etag, changed, err := ReadRetention(ctx, st, "base", "ns", "g", "")
	if err != nil || got != want || etag == "" || !changed {
		t.Fatalf("ReadRetention = %+v, %q, %v, %v", got, etag, changed, err)
	}
	if _, again, changed, err := ReadRetention(ctx, st, "base", "ns", "g", etag); err != nil || changed || again != etag {
		t.Fatalf("a read with the current ETag = %q, %v, %v; want unchanged", again, changed, err)
	}
	if err := WriteRetention(ctx, st, "base", "ns", "g", Retention{}); err != nil {
		t.Fatal(err)
	}
	if len(st.Keys()) != 0 {
		t.Fatalf("the zero policy left %v", st.Keys())
	}
	got, etag, changed, err = ReadRetention(ctx, st, "base", "ns", "g", etag)
	if err != nil || got != (Retention{}) || etag != "" || !changed {
		t.Fatalf("a read after the deletion = %+v, %q, %v, %v; want the zero policy, changed", got, etag, changed, err)
	}
}

func TestAMalformedPolicyIsAnError(t *testing.T) {
	st := NewMemStore(time.Now)
	ctx := context.Background()
	if _, err := st.Put(ctx, RetentionKey("", "ns", "g"), strings.NewReader("last: 3"), PutCondition{}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := ReadRetention(ctx, st, "", "ns", "g", ""); err == nil {
		t.Fatal("a policy that is not JSON was read without an error")
	}
}

func TestPoliciesWriteOnlyWhatChanged(t *testing.T) {
	st := &countingStore{MemStore: NewMemStore(time.Now)}
	p := &Policies{Store: st, Base: ""}
	ctx := context.Background()
	for range 2 {
		if err := p.Sync(ctx, "ns", "g", Retention{Last: 3}); err != nil {
			t.Fatal(err)
		}
	}
	if got := st.puts.Load(); got != 1 {
		t.Fatalf("puts = %d after the same policy twice, want 1", got)
	}
	if err := p.Sync(ctx, "ns", "g", Retention{Last: 4}); err != nil {
		t.Fatal(err)
	}
	if got := st.puts.Load(); got != 2 {
		t.Fatalf("puts = %d after a change, want 2", got)
	}
	for range 2 {
		if err := p.Sync(ctx, "ns", "g", Retention{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := st.deletes.Load(); got != 1 {
		t.Fatalf("deletes = %d after dropping the policy twice, want 1", got)
	}
}
