/*
Copyright The Spawnery Authors.

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

package agentserver

import (
	"context"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/spawnery/spawnery/internal/podspec"
)

// FleetCountInterval is how often the pod count is refreshed. It is slow on
// purpose. What the count feeds is a ceiling four times the fleet's legitimate
// steady state (see FleetConnectionsPerAgent), so being a scale-up behind
// moves nothing: the bound it turns on is still above any working agent's
// peak, and a fleet that just doubled sits at half its new ceiling either way.
// The cost of counting is a walk over every managed pod in the cache, and that
// is not a thing to do every second for a number that tolerates a minute.
const FleetCountInterval = 30 * time.Second

// FleetCounter counts the pods this operator manages, which is how many agent
// connections its endpoint ought to be holding. PeerLimiter turns that into a
// bound; see its doc comment for what the bound is and why it is derived from
// this number rather than fixed.
//
// It reads the manager's cache, so a count costs no API call. It is a
// Runnable rather than a call in Accept's path for exactly that reason
// inverted: the cache read is cheap but not free, and an attacker choosing the
// accept rate would be choosing how often it happened.
type FleetCounter struct {
	// Pods lists pods. The manager's cached client in the operator, which
	// already holds a pod informer for the controllers' sake.
	Pods client.Reader
	// Interval defaults to FleetCountInterval.
	Interval time.Duration

	// size is the last successful count plus one, so that the zero value of
	// the struct reads as never counted. The offset is here rather than a
	// separate flag because the two would be read separately and a count could
	// then be seen without its flag; it is a plain field literal in
	// cmd/spawnery-operator, so there is no constructor to seed a sentinel in.
	// Read from the accept path on every connection, hence the atomic.
	size atomic.Int64
}

// Size is what PeerLimiter.Expect wants: the count, and whether there is one
// yet. It reports unknown until the first count succeeds, which is the whole
// reason this type exists rather than a closure over a List -- an empty cache
// and a cluster with no pods are the same number and must not be the same
// answer. See Expect for what each of them does to the bound.
func (c *FleetCounter) Size() (int, bool) {
	size := c.size.Load()
	if size == 0 {
		return 0, false
	}
	return int(size - 1), true
}

// Start refreshes the count until ctx ends. It counts once immediately, so an
// operator that has just become leader is not blind for an interval.
func (c *FleetCounter) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("fleetcounter")
	interval := c.Interval
	if interval <= 0 {
		interval = FleetCountInterval
	}

	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if err := c.count(ctx); err != nil {
			// Logged and carried, not returned: returning would take the
			// manager down over a cache read, and the last count is still the
			// best answer anyone has. A first count that fails leaves the
			// bound off, which Expect calls the fail-open direction.
			logger.Error(err, "counting managed pods for the fleet connection bound")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// NeedLeaderElection makes this leader-bound, like the endpoint whose bound it
// feeds. A non-leader serves no agents and needs no count.
func (c *FleetCounter) NeedLeaderElection() bool { return true }

// count lists the managed pods and stores how many ought to hold a connection.
func (c *FleetCounter) count(ctx context.Context) error {
	pods := &corev1.PodList{}
	if err := c.Pods.List(ctx, pods,
		client.MatchingLabels{podspec.LabelManagedBy: podspec.ManagedByValue}); err != nil {
		return err
	}

	expected := 0
	for i := range pods.Items {
		if finishedPod(pods.Items[i].Status.Phase) {
			continue
		}
		expected++
	}
	c.size.Store(int64(expected) + 1)
	ExpectedAgents.Set(float64(expected))
	return nil
}

// finishedPod says the pod's containers have exited for good, so it holds no
// connection and never will again.
//
// This is the only place the count is narrowed, and the direction matters. A
// Pending pod is counted though it holds nothing, because counting high only
// loosens a bound; a Failed one is not, because a cluster that keeps its
// failures around would otherwise raise the ceiling by every pod that ever
// died -- which is the same as having no ceiling, arrived at quietly.
func finishedPod(phase corev1.PodPhase) bool {
	return phase == corev1.PodSucceeded || phase == corev1.PodFailed
}
