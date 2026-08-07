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

package controller

import (
	"fmt"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/spawnery/spawnery/internal/agent"
)

// Options are the knobs the operator binary passes to the controllers.
type Options struct {
	// Agents is the shared runtime state of all connected agents.
	Agents *agent.Registry
	// Clock is the time source. Injectable for tests.
	Clock func() time.Time
	// StartupDeadline is how long a server may take to reach Ready.
	StartupDeadline time.Duration
	// PlayerStatusInterval throttles player-count writes into etcd.
	PlayerStatusInterval time.Duration
	// OrphanInterval is how often the orphan sweep runs.
	OrphanInterval time.Duration
	// Registrar reaches the proxies. Milestone 1 wires the no-op.
	Registrar Registrar
}

// SetupAll registers every controller and the orphan sweep with the manager.
func SetupAll(mgr ctrl.Manager, opts Options) error {
	if err := (&NetworkReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("network"),
		Clock:    opts.Clock,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup network controller: %w", err)
	}

	if err := (&ServerGroupReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("servergroup"),
		Agents:   opts.Agents,
		Clock:    opts.Clock,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup server group controller: %w", err)
	}

	if err := (&ServerReconciler{
		Client:               mgr.GetClient(),
		Scheme:               mgr.GetScheme(),
		Recorder:             mgr.GetEventRecorderFor("server"),
		Agents:               opts.Agents,
		Clock:                opts.Clock,
		StartupDeadline:      opts.StartupDeadline,
		PlayerStatusInterval: opts.PlayerStatusInterval,
		Registrar:            opts.Registrar,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setup server controller: %w", err)
	}

	if err := mgr.Add(&OrphanReconciler{
		Client:   mgr.GetClient(),
		Recorder: mgr.GetEventRecorderFor("orphan"),
		Agents:   opts.Agents,
		Interval: opts.OrphanInterval,
	}); err != nil {
		return fmt.Errorf("add orphan sweep: %w", err)
	}

	return nil
}
