//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

const (
	tutorialNamespace   = "spawnery-tutorial"
	tutorialManifest    = "docs/tutorial/network.yaml"
	tutorialServerGroup = "lobby"
	tutorialProxyGroup  = "gateway"

	// tutorialJoinPort is docs/tutorial/network.yaml's fixed NodePort. It is
	// mapped to the same number on the host by docs/tutorial/kind-config.yaml,
	// which is the pair a tutorial reader's own client uses too.
	tutorialJoinPort = 30001

	// tutorialOperatorNamespace is where hack/e2e-tutorial.sh installs the
	// chart -- its own default, unlike the main suite's platform-system --
	// because this scenario has to exercise the install README.md actually
	// tells a reader to run. It is not operatorNamespace: the two suites
	// share this package's eventually/denialHint machinery but run against
	// operators in different namespaces, so every wait in this file that
	// might time out has to be told which one to look at.
	tutorialOperatorNamespace = "spawnery-system"
)

// TestTutorialPath drives the tutorial's own path once, join included: apply
// docs/tutorial/network.yaml, wait for a Ready backend and an addressable
// proxy, then join with cmd/spawnery-join --hold so the proxy's
// status.connectedPlayers is non-zero when the assertion reads it.
//
// It does not run under hack/e2e.sh. That script's own manifest
// (test/e2e/manifests/e2e.yaml) deliberately names images that never resolve,
// so no game or proxy process ever starts there -- and this scenario needs
// both, for real, with a real join. Pulling it into that run would mean
// building and loading the Purpur and Velocity images on every push, which
// milestone 6a's design declares a non-goal. hack/e2e-tutorial.sh builds and
// loads them instead, sets SPAWNERY_E2E_TUTORIAL=1, and runs nightly.
func TestTutorialPath(t *testing.T) {
	if os.Getenv("SPAWNERY_E2E_TUTORIAL") != "1" {
		t.Skip("set SPAWNERY_E2E_TUTORIAL=1 to run the tutorial's own path; " +
			"hack/e2e-tutorial.sh does this nightly, hack/e2e.sh does not")
	}

	applyManifest(t, tutorialManifest)
	applyForwardingSecretReader(t, tutorialNamespace)

	eventuallyIn(t, tutorialOperatorNamespace, 3*time.Minute, "the lobby ServerGroup to report a Ready backend", func() (bool, string) {
		var group spawneryv1alpha1.ServerGroup
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: tutorialNamespace, Name: tutorialServerGroup}, &group); err != nil {
			return false, err.Error()
		}
		return group.Status.ReadyReplicas >= 1, fmt.Sprintf("readyReplicas=%d", group.Status.ReadyReplicas)
	})

	eventuallyIn(t, tutorialOperatorNamespace, 2*time.Minute, "the gateway ProxyGroup to be addressable", func() (bool, string) {
		var group spawneryv1alpha1.ProxyGroup
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: tutorialNamespace, Name: tutorialProxyGroup}, &group); err != nil {
			return false, err.Error()
		}
		return group.Status.Address != "", fmt.Sprintf("status.address=%q", group.Status.Address)
	})

	joinPath, err := exec.LookPath("spawnery-join")
	if err != nil {
		t.Fatalf("spawnery-join not on PATH (%v); the dev shell carries it, run this through nix develop", err)
	}

	// spawnery-join is the automated half of milestone 3's success criterion:
	// it logs in far enough to be routed to a backend and answers with an
	// exit code. --hold keeps the connection open, which is the only way the
	// proxy's status.connectedPlayers is non-zero when the assertion below
	// reads it -- so the process is started rather than run to completion
	// first, and the wait below has to land inside the hold.
	//
	// hold is sized against ProxyGroupReconciler's ResyncInterval (5s):
	// connectedPlayers is only refreshed on reconcile, not pushed the instant
	// an agent reports a join, so the window below must clear at least two
	// resync passes with room left for a slow or contended kind cluster.
	const hold = 25 * time.Second
	cmd := exec.Command(joinPath,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(tutorialJoinPort),
		"--timeout", "45s",
		"--hold", hold.String(),
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start spawnery-join: %v", err)
	}

	// Not eventuallyIn: a join that fails rejects itself immediately rather
	// than waiting out --timeout (internal/mcjoin's encryption-request branch
	// returns as soon as the proxy asks for a handshake it cannot answer), so
	// connectedPlayers would simply never reach 1 and a plain poll would
	// report a generic timeout that names nothing about why. Racing the wait
	// against the process exiting means a failing join is reported in its own
	// words instead of hiding behind that timeout.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	joinDeadline := hold - 3*time.Second
	deadline := time.After(joinDeadline)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	last := "nothing observed yet"
	for {
		select {
		case err := <-done:
			t.Fatalf("spawnery-join exited before the gateway ProxyGroup reported the joined player (%v):\n%s", err, out.String())
		case <-deadline:
			t.Fatalf("timed out after %s waiting for the gateway ProxyGroup to report the joined player; last seen: %s%s",
				joinDeadline, last, denialHint(t, tutorialOperatorNamespace))
		case <-ticker.C:
			var group spawneryv1alpha1.ProxyGroup
			if err := k8s.Get(ctx, client.ObjectKey{Namespace: tutorialNamespace, Name: tutorialProxyGroup}, &group); err != nil {
				last = err.Error()
				continue
			}
			last = fmt.Sprintf("connectedPlayers=%d", group.Status.ConnectedPlayers)
			if group.Status.ConnectedPlayers >= 1 {
				if err := <-done; err != nil {
					t.Fatalf("spawnery-join: %v\n%s", err, out.String())
				}
				t.Logf("spawnery-join: %s", out.String())
				return
			}
		}
	}
}

// applyForwardingSecretReader is the one manual step per game namespace
// README.md's Install section names: config/rbac/forwarding-secret-reader.yaml
// carries no namespace of its own, by its own header comment, so applying it
// is what authorises the operator to read this namespace's forwarding secret.
// Its RoleBinding subject is hard-coded to spawnery-system, which is where
// hack/e2e-tutorial.sh installs the chart -- the chart's own default, unlike
// hack/e2e.sh's platform-system -- so nothing here rewrites it.
func applyForwardingSecretReader(t *testing.T, namespace string) {
	t.Helper()
	cmd := exec.Command("kubectl", "apply", "-n", namespace,
		"-f", repoRoot+"/config/rbac/forwarding-secret-reader.yaml")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("apply config/rbac/forwarding-secret-reader.yaml -n %s: %v\n%s", namespace, err, out)
	}
}
