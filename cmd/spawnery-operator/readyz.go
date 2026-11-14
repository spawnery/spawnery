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
	"net/http"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/spawnery/spawnery/internal/controller"
)

// Half the readiness probe's default timeoutSeconds of 1.
const readyzTimeout = 500 * time.Millisecond

type informerGetter interface {
	GetInformer(ctx context.Context, obj client.Object, opts ...cache.InformerGetOption) (cache.Informer, error)
}

// cachesSynced is Ready once the informer of every object has synced. The
// cache's WaitForCacheSync would not do: it holds no informer before the
// controllers' warmup has asked for them, and then reports synced.
type cachesSynced struct {
	informers informerGetter
	objects   []client.Object
	timeout   time.Duration

	mu    sync.Mutex
	round *syncRound
}

// A round outlives a probe that timed out, and the next probe waits on it
// rather than starting another: the first GetInformer of a type looks up its
// REST mapping, which can wait on API discovery.
type syncRound struct {
	done chan struct{}
	err  error
}

func readyzCheck(mgr manager.Manager) healthz.Checker {
	return (&cachesSynced{
		informers: mgr.GetCache(),
		objects:   controller.WatchedObjects(),
		timeout:   readyzTimeout,
	}).Check
}

func (s *cachesSynced) Check(req *http.Request) error {
	r := s.current()
	timer := time.NewTimer(s.timeout)
	defer timer.Stop()
	select {
	case <-r.done:
		return r.err
	case <-timer.C:
		return fmt.Errorf("the informers were not checked within %s", s.timeout)
	case <-req.Context().Done():
		return req.Context().Err()
	}
}

func (s *cachesSynced) current() *syncRound {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.round == nil {
		r := &syncRound{done: make(chan struct{})}
		s.round = r
		go func() {
			r.err = s.unsynced()
			s.mu.Lock()
			s.round = nil
			s.mu.Unlock()
			close(r.done)
		}()
	}
	return s.round
}

func (s *cachesSynced) unsynced() error {
	var pending []string
	for _, obj := range s.objects {
		informer, err := s.informers.GetInformer(context.Background(), obj, cache.BlockUntilSynced(false))
		if err != nil {
			return fmt.Errorf("informer for %T: %w", obj, err)
		}
		if !informer.HasSynced() {
			pending = append(pending, fmt.Sprintf("%T", obj))
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("informers not synced yet: %s", strings.Join(pending, ", "))
	}
	return nil
}
