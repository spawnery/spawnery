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

// Package statepush turns "a server's lifecycle changed" into a fresh
// NetworkState for the proxies, at most once per namespace per interval. The
// Server controller sees the change; the proxy registry owns the sessions; this
// package sits between them for the same reason as cloudevent: neither may
// import the other.
package statepush

import (
	"context"
	"sync"
	"time"
)

// DefaultInterval is how soon after a push the next one for the same namespace
// may go out.
const DefaultInterval = time.Second

// Target is whatever sends a namespace's sessions a fresh picture.
type Target interface {
	SendState(ctx context.Context, namespace string)
}

// Options configure a Notifier.
type Options struct {
	// Proxies receive the pushes. Required.
	Proxies Target
	// Interval is the shortest time between two pushes for one namespace;
	// DefaultInterval when zero.
	Interval time.Duration
}

// Notifier pushes the picture of a namespace when something in it changed. The
// first change in a quiet namespace goes out at once, without making the caller
// wait for it; changes that follow within the interval share one push at its
// end, built from whatever the cache holds by then.
type Notifier struct {
	proxies  Target
	interval time.Duration

	mu      sync.Mutex
	ctx     context.Context
	stopped bool
	states  map[string]*namespaceState
}

type namespaceState struct {
	last time.Time
	// timer is set while a push is waiting for the interval to pass.
	timer *time.Timer
}

// New returns a Notifier that is ready to take changes. Start gives it a
// lifetime.
func New(opts Options) *Notifier {
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	return &Notifier{
		proxies:  opts.Proxies,
		interval: interval,
		ctx:      context.Background(),
		states:   map[string]*namespaceState{},
	}
}

// Changed says that a server in the namespace changed in a way proxies show.
// It must be called after the change is visible in the cache the picture is
// built from, which is why the Server reconciler calls it and the request
// handlers do not.
func (n *Notifier) Changed(namespace string) {
	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return
	}
	st := n.states[namespace]
	if st == nil {
		st = &namespaceState{}
		n.states[namespace] = st
	}
	if st.timer != nil {
		n.mu.Unlock()
		return
	}
	if wait := time.Until(st.last.Add(n.interval)); wait > 0 {
		st.timer = time.AfterFunc(wait, func() { n.flush(namespace) })
		n.mu.Unlock()
		return
	}
	st.last = time.Now()
	ctx := n.ctx
	n.mu.Unlock()

	// Off the caller's goroutine: SendState can wait on the registry's lock
	// while a resync holds it, and the caller is the Server controller's one
	// worker.
	go n.proxies.SendState(ctx, namespace)
}

func (n *Notifier) flush(namespace string) {
	n.mu.Lock()
	if n.stopped {
		n.mu.Unlock()
		return
	}
	st := n.states[namespace]
	st.timer = nil
	st.last = time.Now()
	ctx := n.ctx
	n.mu.Unlock()

	n.proxies.SendState(ctx, namespace)
}

// Start runs until ctx ends, then drops every push still waiting.
func (n *Notifier) Start(ctx context.Context) error {
	n.mu.Lock()
	n.ctx = ctx
	n.mu.Unlock()

	<-ctx.Done()

	n.mu.Lock()
	defer n.mu.Unlock()
	n.stopped = true
	for _, st := range n.states {
		if st.timer != nil {
			st.timer.Stop()
			st.timer = nil
		}
	}
	return nil
}

// NeedLeaderElection runs the notifier beside the controllers that feed it,
// which are leader-only.
func (n *Notifier) NeedLeaderElection() bool { return true }
