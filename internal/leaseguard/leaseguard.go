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

// Package leaseguard tells leader-bound code whether this process has lately
// seen itself as the holder of the leader Lease. A leader frozen past its
// lease (a paused node, SIGSTOP) resumes believing it leads until its next
// renewal fails; what it reads from memory meanwhile is minutes old.
package leaseguard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// DefaultInterval is controller-runtime's leader election retry period, the
// cadence at which a healthy leader renews.
const DefaultInterval = 2 * time.Second

var ErrNotHolder = errors.New("this pod does not hold the leader lease")

// CheckHolder reads the Lease and fails unless hostname holds it.
func CheckHolder(ctx context.Context, c client.Reader, lease types.NamespacedName, hostname string) error {
	l := &coordinationv1.Lease{}
	if err := c.Get(ctx, lease, l); err != nil {
		return fmt.Errorf("get Lease %s: %w", lease.Name, err)
	}
	holder := ptr.Deref(l.Spec.HolderIdentity, "")
	// controller-runtime's identity is <hostname>_<uuid>.
	if !strings.HasPrefix(holder, hostname+"_") {
		return fmt.Errorf("%w: Lease %s is held by %q", ErrNotHolder, lease.Name, holder)
	}
	return nil
}

// Guard uses an uncached client: with --namespace set, the manager's cache
// does not cover the operator's own namespace.
type Guard struct {
	Client    client.Client
	Namespace string
	// Lease is the manager's LeaderElectionID.
	Lease string
	// Hostname is what controller-runtime puts in front of the holder identity.
	Hostname string
	Interval time.Duration
	Clock    func() time.Time

	mu        sync.Mutex
	confirmed time.Time
}

func (g *Guard) NeedLeaderElection() bool { return true }

func (g *Guard) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("leaseguard")
	if err := g.Confirm(ctx); err != nil {
		logger.Error(err, "confirming the leader lease")
	}
	ticker := time.NewTicker(g.interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := g.Confirm(ctx); err != nil {
				logger.Error(err, "confirming the leader lease")
			}
		}
	}
}

// Confirm reads the Lease once. Seeing another holder withdraws an earlier
// confirmation at once; a failed read leaves it to age out.
func (g *Guard) Confirm(ctx context.Context) error {
	err := CheckHolder(ctx, g.Client, types.NamespacedName{Namespace: g.Namespace, Name: g.Lease}, g.Hostname)
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case err == nil:
		g.confirmed = g.now()
	case errors.Is(err, ErrNotHolder):
		g.confirmed = time.Time{}
	}
	return err
}

// Fresh is true when a confirmation is younger than three intervals: a read
// or two lost to a slow API server is not a lost lease. A nil Guard is fresh.
func (g *Guard) Fresh() bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.confirmed.IsZero() && g.now().Sub(g.confirmed) < 3*g.interval()
}

func (g *Guard) interval() time.Duration {
	if g.Interval <= 0 {
		return DefaultInterval
	}
	return g.Interval
}

func (g *Guard) now() time.Time {
	if g.Clock == nil {
		return time.Now()
	}
	return g.Clock()
}
