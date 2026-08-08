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
	"bytes"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	// Test-only: main.go itself stays free of the Kubernetes dependency tree
	// (see the package doc comment), but the test needs the real values
	// internal/podspec builds the readiness probe from, not a restatement of
	// them.
	"github.com/spawnery/spawnery/internal/podspec"
)

// respondOnce serves exactly one status response. All lengths here are below
// 128, so every varint is a single byte and the frame can be built by hand —
// which keeps this test independent of internal/slp's unexported helpers.
func respondOnce(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 256)
		_, _ = conn.Read(buf)

		doc := `{"version":{"name":"Paper 26.2","protocol":776}}`
		body := append([]byte{0x00, byte(len(doc))}, doc...)
		frame := append([]byte{byte(len(body))}, body...)
		_, _ = conn.Write(frame)
	}()

	return ln.Addr().(*net.TCPAddr).Port
}

func TestRunSucceedsWhenTheServerAnswers(t *testing.T) {
	port := respondOnce(t)

	var stderr bytes.Buffer
	code := run([]string{"--host", "127.0.0.1", "--port", strconv.Itoa(port)}, &stderr)

	if code != 0 {
		t.Errorf("exit code is %d, want 0; stderr: %s", code, stderr.String())
	}
}

func TestRunFailsWhenNothingListens(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	var stderr bytes.Buffer
	code := run([]string{"--host", "127.0.0.1", "--port", strconv.Itoa(port), "--timeout", "1s"}, &stderr)

	if code != 1 {
		t.Errorf("exit code is %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "spawnery-slp:") {
		t.Errorf("stderr is %q, want it to name the tool and the reason", stderr.String())
	}
}

func TestRunRejectsAnUnknownFlag(t *testing.T) {
	var stderr bytes.Buffer
	code := run([]string{"--nonsense"}, &stderr)

	if code != 2 {
		t.Errorf("exit code is %d, want 2 for a usage error", code)
	}
}

// The probe in internal/podspec passes only --host and --port and gives the
// tool five seconds. Anything the probe does not pass has to have a usable
// default, and the tool's own deadline has to fire first so it exits with a
// message instead of being killed by the kubelet.
//
// The assertions read the port and the probe's TimeoutSeconds off a pod
// BuildServerPod actually produces, rather than restating the numbers as
// literals: a literal here would stay green even if internal/podspec's
// probe changed underneath it, which is exactly the regression this test
// exists to catch.
func TestDefaultsMatchTheReadinessProbe(t *testing.T) {
	net := &spawneryv1alpha1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: "minecraft"},
		Spec: spawneryv1alpha1.NetworkSpec{
			ForwardingSecretRef: spawneryv1alpha1.ObjectRef{Name: "velocity-forwarding-secret"},
		},
	}
	group := &spawneryv1alpha1.ServerGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby", Namespace: "minecraft"},
		Spec: spawneryv1alpha1.ServerGroupSpec{
			NetworkRef:                    spawneryv1alpha1.ObjectRef{Name: "production"},
			Type:                          spawneryv1alpha1.ServerGroupEphemeral,
			Image:                         "ghcr.io/spawnery/paper:26.2-0.1.0",
			MaxPlayers:                    100,
			TerminationGracePeriodSeconds: 60,
			Scaling: &spawneryv1alpha1.ScalingSpec{
				MinReplicas: 1, MaxReplicas: 10, SpareSlots: 40,
			},
		},
	}
	srv := &spawneryv1alpha1.Server{
		ObjectMeta: metav1.ObjectMeta{Name: "lobby-x7k2", Namespace: "minecraft", UID: "server-uid"},
		Spec: spawneryv1alpha1.ServerSpec{
			GroupRef: spawneryv1alpha1.ObjectRef{Name: "lobby"},
		},
	}

	pod, err := podspec.BuildServerPod(net, group, srv, "spawnery-operator.spawnery-system.svc:9443")
	if err != nil {
		t.Fatalf("BuildServerPod: %v", err)
	}
	probe := pod.Spec.Containers[0].ReadinessProbe
	probeTimeout := time.Duration(probe.TimeoutSeconds) * time.Second

	if defaultTimeout >= probeTimeout {
		t.Errorf("default timeout is %v, want it below the probe's %v", defaultTimeout, probeTimeout)
	}
	if defaultHost != "127.0.0.1" {
		t.Errorf("default host is %q, want %q", defaultHost, "127.0.0.1")
	}
	if defaultPort != int(podspec.MinecraftPort) {
		t.Errorf("default port is %d, want %d", defaultPort, podspec.MinecraftPort)
	}
}
