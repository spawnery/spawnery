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
	"errors"
	"io"
	"testing"
	"time"
)

func put(t *testing.T, s Store, key, body string, cond PutCondition) (ObjectInfo, error) {
	t.Helper()
	return s.Put(context.Background(), key, bytes.NewReader([]byte(body)), cond)
}

func TestMemStoreCreateOnlyOnce(t *testing.T) {
	s := NewMemStore(time.Now)
	if _, err := put(t, s, "a", "1", PutCondition{IfNoneMatch: true}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := put(t, s, "a", "2", PutCondition{IfNoneMatch: true}); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("second create: err = %v, want ErrPrecondition", err)
	}
}

func TestMemStoreIfMatchComparesBareETags(t *testing.T) {
	s := NewMemStore(time.Now)
	first, _ := put(t, s, "a", "1", PutCondition{})
	if first.ETag == "" || first.ETag[0] == '"' {
		t.Fatalf("ETag %q is empty or quoted", first.ETag)
	}
	if _, err := put(t, s, "a", "2", PutCondition{IfMatch: "deadbeef"}); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("stale If-Match: err = %v, want ErrPrecondition", err)
	}
	if _, err := put(t, s, "a", "2", PutCondition{IfMatch: first.ETag}); err != nil {
		t.Fatalf("current If-Match: %v", err)
	}
	rc, _, err := s.Get(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	if string(got) != "2" {
		t.Fatalf("content = %q, want 2", got)
	}
}

func TestMemStoreMissingIsNotFoundAndDeleteIsIdempotent(t *testing.T) {
	s := NewMemStore(time.Now)
	if _, err := s.Head(context.Background(), "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Head: err = %v, want ErrNotFound", err)
	}
	if err := s.Delete(context.Background(), "x"); err != nil {
		t.Fatalf("Delete of nothing: %v", err)
	}
}

func TestMemStoreListsByPrefixInOrder(t *testing.T) {
	s := NewMemStore(time.Now)
	for _, k := range []string{"w/b", "w/a", "x/a"} {
		put(t, s, k, "", PutCondition{})
	}
	got, _ := s.List(context.Background(), "w/")
	if len(got) != 2 || got[0] != "w/a" || got[1] != "w/b" {
		t.Fatalf("List = %v", got)
	}
}

func TestMemStoreOutageFailsEveryCall(t *testing.T) {
	s := NewMemStore(time.Now)
	boom := errors.New("unreachable")
	s.SetOutage(boom)
	if _, err := put(t, s, "a", "1", PutCondition{}); !errors.Is(err, boom) {
		t.Fatalf("Put during outage: %v", err)
	}
	s.SetOutage(nil)
	if _, err := put(t, s, "a", "1", PutCondition{}); err != nil {
		t.Fatalf("Put after outage: %v", err)
	}
}

func TestLayout(t *testing.T) {
	if got := WorldPrefix("", "ns/g/k"); got != "ns/g/k/" {
		t.Errorf("WorldPrefix = %q", got)
	}
	if got := WorldPrefix("worlds", "ns/g/k"); got != "worlds/ns/g/k/" {
		t.Errorf("WorldPrefix = %q", got)
	}
	if got := DeletionKey("", "ns/g/k"); got != ".deletions/ns/g/k" {
		t.Errorf("DeletionKey = %q", got)
	}
	if got := DeletionPrefix("worlds"); got != "worlds/.deletions/" {
		t.Errorf("DeletionPrefix = %q", got)
	}
}
