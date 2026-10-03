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
	"strings"
	"testing"
	"time"

	"github.com/spawnery/spawnery/internal/controller"
	"github.com/spawnery/spawnery/internal/phase"
)

// certs.ServingDNSNames issues exactly this name among its four.
func TestAgentEndpointMatchesTheServingName(t *testing.T) {
	got := agentEndpoint("spawnery-system")
	if want := "spawnery-operator.spawnery-system.svc:9443"; got != want {
		t.Errorf("agentEndpoint = %q, want %q", got, want)
	}
}

func TestValidateAgentFlags(t *testing.T) {
	tests := []struct {
		name         string
		namespace    string
		renewAfter   time.Duration
		hardDeadline time.Duration
		wantErr      bool
	}{
		{"the deployment defaults", "spawnery-system", 8 * time.Minute, 10 * time.Minute, false},
		{"no namespace", "", 8 * time.Minute, 10 * time.Minute, true},
		{"renewal at the deadline", "spawnery-system", 10 * time.Minute, 10 * time.Minute, true},
		{"renewal past the deadline", "spawnery-system", 12 * time.Minute, 10 * time.Minute, true},
		{"a deadline past the token's lifetime", "spawnery-system", 8 * time.Minute, 24 * time.Hour, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAgentFlags(tc.namespace, tc.renewAfter, tc.hardDeadline)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateAgentFlags = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestLeaderReadyCheckIsRedBeforeElectionAndGreenAfter(t *testing.T) {
	elected := make(chan struct{})
	check := leaderReadyCheck(elected)

	if err := check(nil); err == nil {
		t.Error("readyz is green before the lock was taken; a standby would attract agents")
	}
	close(elected)
	if err := check(nil); err != nil {
		t.Errorf("readyz is red although this replica holds the lock: %v", err)
	}
}

func TestLeaderReadyCheckDoesNotBlock(t *testing.T) {
	check := leaderReadyCheck(make(chan struct{}))

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = check(nil)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the ready check blocked; every probe of a standby would time out")
	}
}

func TestTaintKeysSetRejectsEmpty(t *testing.T) {
	var keys taintKeys
	if err := keys.Set(""); err == nil {
		t.Error("Set(\"\") returned no error; an empty taint key would match nothing")
	}
	if len(keys) != 0 {
		t.Errorf("Set(\"\") appended anyway: %v", keys)
	}

	if err := keys.Set("node.kubernetes.io/unreachable"); err != nil {
		t.Fatalf("Set of a real key failed: %v", err)
	}
	if got, want := keys.String(), "node.kubernetes.io/unreachable"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestTaintKeysSetRejectsAWholeTaint(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{"a whole taint", "node.kubernetes.io/unreachable=true:NoExecute"},
		{"a key and an effect", "node.kubernetes.io/unreachable:NoExecute"},
		{"a key and a value", "node.kubernetes.io/unreachable=true"},
		{"a key with a space", "node.kubernetes.io/un reachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var keys taintKeys
			err := keys.Set(tc.value)
			if err == nil {
				t.Fatalf("Set(%q) was accepted; it matches no taint that exists, so the "+
					"operator would never drain and never say why", tc.value)
			}
			if !strings.Contains(err.Error(), "takes the key alone") {
				t.Errorf("error = %q, want it to say what this flag takes", err)
			}
			if len(keys) != 0 {
				t.Errorf("keys = %v after a refusal, want none kept", keys)
			}
		})
	}
}

func TestTaintKeysSetAcceptsRealKeys(t *testing.T) {
	for _, value := range []string{
		"node.kubernetes.io/unreachable",
		"node.kubernetes.io/unschedulable",
		"ToBeDeletedByClusterAutoscaler",
		"cloud.google.com/impending-node-termination",
		"a",
	} {
		var keys taintKeys
		if err := keys.Set(value); err != nil {
			t.Errorf("Set(%q) was refused: %v. That is a taint key a real cluster uses", value, err)
		}
	}
}

func TestTheLeaderElectionLeaseLandsInTheOperatorNamespace(t *testing.T) {
	opts := managerOptions(managerFlags{operatorNamespace: "spawnery-system"})
	if opts.LeaderElectionNamespace != "spawnery-system" {
		t.Errorf("LeaderElectionNamespace = %q, want the operator's own namespace. Empty is "+
			"what makes a local run fail at startup: controller-runtime then looks for a "+
			"ServiceAccount mount that only exists in a pod",
			opts.LeaderElectionNamespace)
	}

	// --namespace narrows the cache, not where the lock lives.
	scoped := managerOptions(managerFlags{operatorNamespace: "spawnery-system", watchNamespace: "minecraft"})
	if scoped.LeaderElectionNamespace != "spawnery-system" {
		t.Errorf("LeaderElectionNamespace = %q with --namespace set, want the operator's own",
			scoped.LeaderElectionNamespace)
	}
	if _, ok := scoped.Cache.DefaultNamespaces["minecraft"]; !ok {
		t.Errorf("cache namespaces = %v, want minecraft; --namespace stopped reaching the cache",
			scoped.Cache.DefaultNamespaces)
	}
	if len(opts.Cache.DefaultNamespaces) != 0 {
		t.Errorf("cache namespaces = %v with no --namespace, want the whole cluster",
			opts.Cache.DefaultNamespaces)
	}
}

// TestRescueWindowWarning covers the three readings, above all that the default
// is silent.
func TestRescueWindowWarning(t *testing.T) {
	for _, tc := range []struct {
		what           string
		reportInterval time.Duration
		wantWarning    bool
		wantPhrase     string
	}{
		{
			what:           "the operator's own default, which must say nothing",
			reportInterval: 5 * time.Second,
		},
		{
			what:           "thirteen seconds, which leaves four -- above zero, below a resync",
			reportInterval: 13 * time.Second,
			wantWarning:    true,
			wantPhrase:     "less than the",
		},
		{
			what:           "fifteen, the boundary at which the window closes exactly",
			reportInterval: 15 * time.Second,
			wantWarning:    true,
			wantPhrase:     "no room at all",
		},
		{
			what:           "twenty, where the operator is later than the kick",
			reportInterval: 20 * time.Second,
			wantWarning:    true,
			wantPhrase:     "no room at all",
		},
	} {
		t.Run(tc.what, func(t *testing.T) {
			got := rescueWindowWarning(tc.reportInterval)
			if !tc.wantWarning {
				if got != "" {
					t.Errorf("warned about a report interval of %s: %q", tc.reportInterval, got)
				}
				return
			}
			if got == "" {
				t.Fatalf("said nothing about a report interval of %s", tc.reportInterval)
			}
			if !strings.Contains(got, tc.wantPhrase) {
				t.Errorf("the warning is %q, which does not carry %q -- the two readings need "+
					"different responses and the message is what tells them apart",
					got, tc.wantPhrase)
			}
			if !strings.Contains(got, tc.reportInterval.String()) {
				t.Errorf("the warning is %q and does not name the flag's value; somebody has "+
					"to know which number to change", got)
			}
		})
	}
}

func TestTheRescueWindowThresholdIsTheResyncInterval(t *testing.T) {
	// The largest report interval whose window still clears a resync.
	ok := (phase.VelocityReadTimeout - controller.ResyncInterval) / 2
	if got := rescueWindowWarning(ok); got != "" {
		t.Errorf("warned at a report interval of %s, whose window is exactly the %s resync: %q",
			ok, controller.ResyncInterval, got)
	}
	if got := rescueWindowWarning(ok + time.Second); got == "" {
		t.Errorf("said nothing at a report interval of %s, one second past the boundary", ok+time.Second)
	}
}
