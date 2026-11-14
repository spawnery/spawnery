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

package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spawnery/spawnery/internal/controller"
)

type fakeInformer struct {
	cache.Informer
	synced bool
}

func (f fakeInformer) HasSynced() bool { return f.synced }

// fakeInformers blocks like controller-runtime's cache does: on an unsynced
// informer unless told BlockUntilSynced(false), and on everything while hang
// is open.
type fakeInformers struct {
	mu       sync.Mutex
	unsynced map[string]bool
	hang     chan struct{}
	release  chan struct{}
	calls    int
}

func newFakeInformers(t *testing.T) *fakeInformers {
	f := &fakeInformers{unsynced: map[string]bool{}, release: make(chan struct{})}
	t.Cleanup(func() { close(f.release) })
	return f
}

func (f *fakeInformers) GetInformer(ctx context.Context, obj client.Object, opts ...cache.InformerGetOption) (cache.Informer, error) {
	f.mu.Lock()
	f.calls++
	hang := f.hang
	f.mu.Unlock()
	if hang != nil {
		select {
		case <-hang:
		case <-f.release:
			return nil, fmt.Errorf("test over")
		}
	}
	f.mu.Lock()
	synced := !f.unsynced[fmt.Sprintf("%T", obj)]
	f.mu.Unlock()

	var o cache.InformerGetOptions
	for _, opt := range opts {
		opt(&o)
	}
	if !synced && (o.BlockUntilSynced == nil || *o.BlockUntilSynced) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-f.release:
			return nil, fmt.Errorf("test over")
		}
	}
	return fakeInformer{synced: synced}, nil
}

func probe(t *testing.T, s *cachesSynced) (time.Duration, error) {
	t.Helper()
	start := time.Now()
	answer := make(chan error, 1)
	go func() { answer <- s.Check(httptest.NewRequest("GET", "/readyz", nil)) }()
	select {
	case err := <-answer:
		return time.Since(start), err
	case <-time.After(5 * time.Second):
		t.Fatal("readyz had not answered after 5s")
		return 0, nil
	}
}

func TestReadyzWaitsForEveryWatchedInformer(t *testing.T) {
	for _, obj := range controller.WatchedObjects() {
		name := fmt.Sprintf("%T", obj)
		t.Run(name, func(t *testing.T) {
			f := newFakeInformers(t)
			f.unsynced[name] = true
			s := &cachesSynced{informers: f, objects: controller.WatchedObjects(), timeout: readyzTimeout}
			_, err := probe(t, s)
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Errorf("readyz = %v with %s unsynced, want an error naming it: the "+
					"pod would be Ready before its controllers can take over", err, name)
			}
		})
	}

	f := newFakeInformers(t)
	s := &cachesSynced{informers: f, objects: controller.WatchedObjects(), timeout: readyzTimeout}
	if _, err := probe(t, s); err != nil {
		t.Errorf("readyz = %v with every informer synced, want nil", err)
	}
}

func TestReadyzDoesNotWaitForAnInformerToSync(t *testing.T) {
	f := newFakeInformers(t)
	f.unsynced["*v1.Pod"] = true
	s := &cachesSynced{informers: f, objects: controller.WatchedObjects(), timeout: readyzTimeout}
	took, err := probe(t, s)
	if err == nil {
		t.Fatal("readyz = nil with the Pod informer unsynced")
	}
	if took > time.Second {
		t.Errorf("readyz answered after %s, want within 1s: the kubelet gives up after its "+
			"timeoutSeconds and counts a failure", took)
	}
	if !strings.Contains(err.Error(), "not synced") {
		t.Errorf("readyz = %v, want the unsynced informers named rather than a timeout", err)
	}
}

func TestReadyzAnswersWhileTheCacheHangs(t *testing.T) {
	f := newFakeInformers(t)
	f.hang = make(chan struct{})
	s := &cachesSynced{informers: f, objects: controller.WatchedObjects(), timeout: readyzTimeout}

	for i := range 3 {
		took, err := probe(t, s)
		if err == nil {
			t.Fatalf("probe %d: readyz = nil while GetInformer hangs", i)
		}
		if took > time.Second {
			t.Errorf("probe %d answered after %s, want within 1s", i, took)
		}
	}
	f.mu.Lock()
	calls := f.calls
	f.mu.Unlock()
	if calls != 1 {
		t.Errorf("GetInformer was called %d times while the first call hung, want 1: each "+
			"probe would leave another goroutine waiting on the API server", calls)
	}

	close(f.hang)
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := probe(t, s)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("readyz = %v 5s after GetInformer stopped hanging, want nil", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
