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

// FleetCountInterval can be slow: the bound it feeds is four times the fleet's
// steady state (FleetConnectionsPerAgent), so a scale-up behind moves nothing.
const FleetCountInterval = 30 * time.Second

// FleetCounter is a Runnable rather than a call in Accept's path so that an
// attacker choosing the accept rate does not choose how often the cache is walked.
type FleetCounter struct {
	Pods client.Reader
	// Interval defaults to FleetCountInterval.
	Interval time.Duration

	// size is the last successful count plus one, so the zero value reads as
	// never counted; a separate flag could be read apart from the count.
	size atomic.Int64
}

// Size reports unknown until the first count succeeds: an empty cache and a
// cluster with no pods are the same number but not the same answer.
func (c *FleetCounter) Size() (int, bool) {
	size := c.size.Load()
	if size == 0 {
		return 0, false
	}
	return int(size - 1), true
}

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
			// Not returned: that would take the manager down over a cache read.
			logger.Error(err, "counting managed pods for the fleet connection bound")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func (c *FleetCounter) NeedLeaderElection() bool { return true }

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

// finishedPod is the only narrowing of the count: Pending pods count because
// counting high only loosens the bound, but kept Failed pods would raise the
// ceiling without limit.
func finishedPod(phase corev1.PodPhase) bool {
	return phase == corev1.PodSucceeded || phase == corev1.PodFailed
}
