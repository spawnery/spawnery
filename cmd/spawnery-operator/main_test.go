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
	"strings"
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

// TestTaintKeysSetRejectsAWholeTaint closes the half of the milestone 4c-3
// entry in docs/known-issues.md that can be closed: "nor is the key itself
// checked against anything — a typo in -drain-taint is indistinguishable from
// a taint key that legitimately does not exist in this cluster."
//
// A well-formed key that is simply absent still cannot be told from a typo, and
// nothing can tell those apart. What can be caught is the mistake to expect:
// taints are written key=value:Effect nearly everywhere a person meets one, so
// passing the whole taint is the likely slip — and it was the one this operator
// survived worst. Such a key matches no taint that exists, so the flag was
// accepted, nothing ever drained, and nothing said why.
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
			// The message has to show what a key looks like, or a reader who
			// wrote the taint out has no way to see what is wrong with it.
			if !strings.Contains(err.Error(), "takes the key alone") {
				t.Errorf("error = %q, want it to say what this flag takes", err)
			}
			if len(keys) != 0 {
				t.Errorf("keys = %v after a refusal, want none kept", keys)
			}
		})
	}
}

// TestTaintKeysSetAcceptsRealKeys is the other half. Refusing too much would be
// worse than refusing nothing: an operator whose cluster uses a legitimate key
// this validation happens to dislike cannot drain at all.
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

// The leader-election Lease goes in the operator's own namespace, named rather
// than derived.
//
// Left empty, controller-runtime reads the namespace out of the ServiceAccount
// mount, which exists only inside a pod. A local `go run` therefore died at
// startup unless it was told --leader-elect=false, which is what every runbook
// under docs/ passes and what docs/known-issues.md carried as an open
// precondition for milestone 6. In a cluster the value is the same one
// controller-runtime would have derived — the chart sets POD_NAMESPACE from
// metadata.namespace and --operator-namespace defaults to it — so this moves
// no lease and needs no new grant.
func TestTheLeaderElectionLeaseLandsInTheOperatorNamespace(t *testing.T) {
	opts := managerOptions(managerFlags{operatorNamespace: "spawnery-system"})
	if opts.LeaderElectionNamespace != "spawnery-system" {
		t.Errorf("LeaderElectionNamespace = %q, want the operator's own namespace. Empty is "+
			"what makes a local run fail at startup: controller-runtime then looks for a "+
			"ServiceAccount mount that only exists in a pod",
			opts.LeaderElectionNamespace)
	}

	// --namespace is a different question and must not be confused with it: it
	// narrows what the cache watches, not where the lock lives.
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
