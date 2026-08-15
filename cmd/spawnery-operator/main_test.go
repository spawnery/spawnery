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

package main

import (
	"testing"
	"time"
)

// The endpoint and the certificate SANs are derived from the same two strings.
// This pins the shape an agent dials; certs.ServingDNSNames issues exactly
// this name among its four.
func TestAgentEndpointMatchesTheServingName(t *testing.T) {
	got := agentEndpoint("spawnery-system")
	if want := "spawnery-operator.spawnery-system.svc:9443"; got != want {
		t.Errorf("agentEndpoint = %q, want %q", got, want)
	}
}

// An operator without a namespace would issue a certificate for the wrong
// names and hand its agents the wrong address. Both failures surface as a TLS
// error in a game server pod minutes later, which is why this one is fatal at
// startup instead.
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

// A standby must never become an endpoint of the agent Service: the gRPC
// service is leader-bound, so agents landing on a standby would fill a
// registry no controller reads and their servers would never reach Ready.
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

// The kubelet probes on a timer, so the check has to answer rather than wait
// for an election that may never come to this replica.
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

// An empty -drain-taint would match nothing, which is silent everywhere it
// matters: nodeDeparting would just never see it in the list, so a mistyped
// flag would fail open rather than error at startup.
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
