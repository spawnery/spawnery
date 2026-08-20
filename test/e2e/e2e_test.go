//go:build e2e

// Package e2e drives the operator in a real cluster.
//
// It exists for one assertion internal/rbacaudit structurally cannot make.
// That audit compares the generated ClusterRole against a hand-maintained
// table in both directions, so it catches drift -- but a permission missing
// from *both* leaves the suite green while the operator still walks into a
// Forbidden the first time it runs under its own ServiceAccount. Proving
// completeness needs a real process under a real authorizer, and that is what
// this package watches.
//
// The build tag keeps it out of `go test ./...` and out of `make test`: it
// needs a cluster that hack/e2e.sh builds, and the commit loop stays where it
// is.
package e2e

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/yaml"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

const (
	// operatorNamespace is where hack/e2e.sh installs the chart, and it is
	// deliberately not the chart's own default. What that buys is split, and
	// the larger half never reaches this package: a spawnery-system literal
	// left in one of the chart's own-namespace RBAC fields is refused at
	// admission by Kubernetes, so `helm install` fails and hack/e2e.sh aborts
	// under set -e before `go test` runs -- measured, and no scenario here
	// executed at all. The half that would reach this package is a literal in
	// a subject namespace, which applies cleanly and is by design caught at
	// runtime by theOperatorWasNeverDenied once the denial it causes lands on
	// a write verb; that path was never mutated, so it is reasoning rather
	// than measurement. See the comment on OPERATOR_NAMESPACE in hack/e2e.sh.
	operatorNamespace = "platform-system"

	// testNamespace is where test/e2e/manifests/e2e.yaml puts its objects.
	testNamespace = "minecraft"

	// repoRoot is relative because `go test` runs each binary with its own
	// package directory as the working directory.
	repoRoot = "../.."
)

var (
	k8s       client.Client
	clientset *kubernetes.Clientset
	ctx       context.Context
)

func TestMain(m *testing.M) {
	cfg, err := config.GetConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "no usable kubeconfig: %v\nRun this through hack/e2e.sh.\n", err)
		os.Exit(1)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "build scheme: %v\n", err)
		os.Exit(1)
	}
	if err := spawneryv1alpha1.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "build scheme: %v\n", err)
		os.Exit(1)
	}

	k8s, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "build client: %v\n", err)
		os.Exit(1)
	}
	clientset, err = kubernetes.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "build clientset: %v\n", err)
		os.Exit(1)
	}
	ctx = context.Background()

	os.Exit(m.Run())
}

// TestSpawneryUnderItsOwnServiceAccount is the whole run, ordered explicitly.
//
// Go runs top-level tests in the order they appear and files in alphabetical
// order, which would make the order between scenarios an accident of file
// naming. These depend on one another -- the manifest has to exist before
// anything can scale it -- so the order is written down instead. The denial
// check is last because it judges everything the run did.
func TestSpawneryUnderItsOwnServiceAccount(t *testing.T) {
	t.Run("the operator is up and has not restarted", theOperatorIsUp)
	t.Run("the test manifest is accepted", theTestManifestIsAccepted)
	t.Run("the group scales up", theGroupScalesUp)
	t.Run("the group sheds surplus at a lowered ceiling", theCeilingShedsSurplus)
	t.Run("the orphan sweep removes a stray pod", theOrphanSweepRemovesAStrayPod)
	t.Run("the finalizer is released", theFinalizerIsReleased)
	t.Run("the startup deadline fails a server and clears it", theStartupDeadlineFailsAServerAndClearsIt)
	t.Run("a persistent group's claim outlives its server", aPersistentGroupsClaimOutlivesItsServer)
	t.Run("the proxy group gets its Service", theProxyGroupGetsItsService)
	t.Run("the LoadBalancer group gets its Service", theLoadBalancerGroupGetsItsService)
	t.Run("the HostPort group binds the port and has no Service", theHostPortGroupBindsThePortAndHasNoService)
	t.Run("a switch to HostPort removes the Service", aSwitchToHostPortRemovesTheService)
	t.Run("the ClusterIP group gets a plain Service with no node port", theClusterIPGroupGetsAPlainServiceWithNoNodePort)
	t.Run("a forbidden host port is reported on the group", aForbiddenHostPortIsReportedOnTheGroup)
	t.Run("the operator holds its secret and its lease", theOperatorHoldsItsSecretAndItsLease)
	t.Run("the table holds against the real authorizer", theTableHoldsAgainstTheRealAuthorizer)
	t.Run("the network gets its policy", theNetworkGetsItsPolicy)
	t.Run("the operator stays ready behind its own policy", theOperatorStaysReadyBehindItsOwnPolicy)
	t.Run("the operator was never denied", theOperatorWasNeverDenied)
}

// theOperatorIsUp checks the pod the whole run depends on. A crash loop here
// reads as every later scenario timing out, which says nothing about the cause.
func theOperatorIsUp(t *testing.T) {
	pod := operatorPod(t)

	ready := false
	for _, c := range pod.Status.ContainerStatuses {
		if c.Ready {
			ready = true
		}
		if c.RestartCount > 0 {
			t.Errorf("the operator container has restarted %d time(s) before the run even "+
				"began. Kubernetes keeps one container back, so from the second restart "+
				"on there is a stretch of this operator's life nothing can read, and "+
				"theOperatorWasNeverDenied's silence stops meaning anything about it",
				c.RestartCount)
		}
	}
	if !ready {
		t.Fatalf("the operator pod %s is not ready: phase %s", pod.Name, pod.Status.Phase)
	}
}

// theOperatorWasNeverDenied is the reason this package exists.
//
// It matches the API server's own phrasing -- `is forbidden:` -- rather than
// the bare word. Spawnery has a condition reason called SecretReadForbidden
// (milestone 5c), and matching "forbidden" alone would turn a correctly
// reported missing secret into a false accusation about RBAC.
//
// What a pass here does and does not establish was measured both ways, and
// the answer is narrower than "this check works". Task 4's verification
// mutated the ClusterRole and the namespaced Role four separate ways
// (task-4-report.md, "Fix round 1"): denying a cache-backed List (pods, then
// networks) revoked the permission for real but produced no observable call
// at all -- no log line, no 403 in the operator's own client metrics --
// across seven and three-quarter minutes of continuous watching. Denying a
// direct, uncached call that gates the operator's own readiness (the TLS
// secret's create, the leader election lease's update) produced a real,
// quoted `is forbidden:` line every time, but also kept the pod from ever
// reaching Available, so hack/e2e.sh's rollout wait timed out before this
// test ever ran.
//
// Task 5 broke that deadlock, once the scenarios below started applying a
// Network and its groups: removing `create` on pods -- a WRITE verb --
// produced a quoted
// `is forbidden: ... cannot create resource "pods"` on the first attempt,
// with the operator still healthy and this check still able to read it. So
// what is measured is narrow: a revoked write fires this check, and the two
// cache-backed lists that were tried did not. Reads as a class were not
// measured -- no uncached read was ever revoked and watched here. The
// explanation that would generalise it -- that such a read goes through the
// manager's cache, whose initial sync is a watch rather than a list, so a
// revoked read verb never reaches a request the API server could deny -- is a
// hypothesis nothing has established; do not restate it as fact.
//
// There is also at least one uncached read this check would miss for an
// entirely unrelated reason, and it is the very one the paragraph at the top
// of this comment is about: readForwardingSecret
// (internal/controller/forwardingsecret.go) folds a real 403 into a condition
// message that carries no `is forbidden:` substring, and nothing on that path
// logs. This check can only see what something logs, so an error the code
// handles well is invisible to it.
//
// Read a green run as evidence about write paths, and see
// docs/known-issues.md's two milestone 6a sections for what it does not
// establish beyond them. Do not add a sleep to manufacture traffic.
//
// The restart re-check below is not redundant with theOperatorIsUp. That
// subtest runs first, before any scenario has driven a single call; this one
// runs a minute and a half later and judges everything in between. A container
// that was OOM-killed somewhere in the middle -- not hypothetical on a 3.9 GB
// host with no swap -- would leave this check reading a log that begins after
// the interesting part, and a replacement making no denied call of its own
// would report PASS over a run it had covered a fraction of.
func theOperatorWasNeverDenied(t *testing.T) {
	log, restarts := operatorLog(t)
	if restarts > 0 {
		t.Errorf("the operator container has restarted %d time(s) during the run. The "+
			"log below covers the current process and, where the API server still "+
			"holds it, the one before it -- but with %d restart(s) this check can no "+
			"longer account for the whole run, so a denial in a process it cannot read "+
			"would look identical to no denial at all", restarts, restarts)
	}

	var offenders []string
	for _, line := range strings.Split(log, "\n") {
		// A Pod Security rejection is not an RBAC denial, and it carries the
		// same `is forbidden:` prefix. aForbiddenHostPortIsReportedOnTheGroup
		// causes one on purpose -- it is the only enforced refusal this run
		// can observe -- and without this exclusion the last and most
		// important scenario of the run would fail for a reason another
		// scenario created.
		//
		// The exclusion is one substring on purpose. Everything else the API
		// server phrases with `is forbidden:` still counts, including an RBAC
		// denial on a pod create, which shares nothing with this text.
		if strings.Contains(line, "is forbidden:") &&
			!strings.Contains(line, "violates PodSecurity") {
			offenders = append(offenders, line)
		}
	}
	if len(offenders) > 0 {
		t.Errorf("the operator was denied %d time(s) under its own ServiceAccount:\n%s\n\n"+
			"This is the assertion internal/rbacaudit cannot make. It compares the "+
			"generated role against its table in both directions, so a permission "+
			"missing from both leaves it green while this fails.",
			len(offenders), strings.Join(offenders, "\n"))
	}
}

// operatorPod returns the single operator pod, or fails.
func operatorPod(t *testing.T) *corev1.Pod {
	t.Helper()
	var pods corev1.PodList
	err := k8s.List(ctx, &pods,
		client.InNamespace(operatorNamespace),
		client.MatchingLabels{
			"app.kubernetes.io/name":      "spawnery",
			"app.kubernetes.io/component": "operator",
		})
	if err != nil {
		t.Fatalf("list operator pods: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("got %d operator pods, want exactly one", len(pods.Items))
	}
	return &pods.Items[0]
}

// operatorLog reads the operator's log through the API, and reports how many
// times its container has restarted.
//
// An empty PodLogOptions returns the *current* container's log and nothing
// else, so on a restarted pod it silently begins wherever the last process
// did. Where the kubelet still holds the previous container's log --
// terminationMessage aside, one back is all Kubernetes keeps -- this prepends
// it, so "the operator's whole log", which the README and the handover both
// claim, is true for the common single-restart case. The restart count comes
// back with it because beyond one restart it is not true, and the caller has
// to say so rather than quietly assert over a hole.
func operatorLog(t *testing.T) (string, int32) {
	t.Helper()
	pod := operatorPod(t)

	var restarts int32
	for _, c := range pod.Status.ContainerStatuses {
		restarts += c.RestartCount
	}

	var b strings.Builder
	if restarts > 0 {
		// Best effort: the previous container's log is gone if the kubelet has
		// already rotated it away, and a Fatal here would turn a diagnostic
		// into the failure.
		if prev, err := readPodLog(pod.Name, &corev1.PodLogOptions{Previous: true}); err == nil {
			b.WriteString(prev)
			b.WriteString("\n")
		} else {
			t.Logf("the previous container's log is not available (%v); this check can "+
				"only read the current process", err)
		}
	}

	body, err := readPodLog(pod.Name, &corev1.PodLogOptions{})
	switch {
	case err == nil:
		b.WriteString(body)
	case restarts > 0:
		// A container that has restarted may be between attempts right now,
		// and GetLogs refuses a container that has not started. Failing here
		// would replace the caller's report of the restart -- the thing that
		// actually went wrong -- with a message about a log read.
		t.Logf("the current container's log is not available (%v); this check has only "+
			"the previous container's", err)
	default:
		t.Fatalf("read logs of %s: %v", pod.Name, err)
	}
	return b.String(), restarts
}

// readPodLog streams one container log of a pod in the operator's namespace.
func readPodLog(name string, opts *corev1.PodLogOptions) (string, error) {
	stream, err := clientset.CoreV1().Pods(operatorNamespace).GetLogs(name, opts).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = stream.Close() }()

	body, err := io.ReadAll(stream)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// eventually polls cond until it holds or the deadline passes, and reports the
// last thing it saw when it gives up.
//
// It is this package's default waiting construct. A run built on fixed sleeps
// turns flaky under load, and a flaky E2E run is ignored within weeks -- which
// is §4 of the 2026-08-07 E2E design, kept. eventuallyStable is its sibling,
// for the one assertion that needs a condition to hold rather than merely to
// have occurred.
func eventually(t *testing.T, deadline time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	stop := time.Now().Add(deadline)
	last := "nothing observed yet"
	for time.Now().Before(stop) {
		ok, detail := cond()
		if ok {
			return
		}
		last = detail
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s; last seen: %s", deadline, what, last)
}

// eventuallyStable is eventually's sibling for a condition that must hold,
// not merely occur once. eventually returns on the first poll that satisfies
// cond, which a transient state can also satisfy without the thing under test
// having actually happened -- this package's own lifecycle scenarios churn
// Servers continuously (see nonFailedServersInGroup), so a count can pass
// through the right value on its way to a different one. eventuallyStable
// instead requires cond to stay true for the whole of hold before it
// succeeds, and resets its clock the moment cond goes false again.
func eventuallyStable(t *testing.T, deadline, hold time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	stop := time.Now().Add(deadline)
	last := "nothing observed yet"
	var since time.Time
	for time.Now().Before(stop) {
		ok, detail := cond()
		if ok {
			if since.IsZero() {
				since = time.Now()
			}
			if time.Since(since) >= hold {
				return
			}
		} else {
			since = time.Time{}
			last = detail
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s to hold for %s; last seen: %s", deadline, what, hold, last)
}

// applyManifest creates every document of a multi-document manifest, tolerating
// objects that are already there. Pass client.DryRunAll to check that a
// manifest is *accepted* without creating anything.
func applyManifest(t *testing.T, rel string, opts ...client.CreateOption) {
	t.Helper()

	f, err := os.Open(repoRoot + "/" + rel)
	if err != nil {
		t.Fatalf("open %s: %v", rel, err)
	}
	defer func() { _ = f.Close() }()

	docs := utilyaml.NewYAMLReader(bufio.NewReader(f))
	for {
		doc, err := docs.Read()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if strings.TrimSpace(string(doc)) == "" {
			continue
		}
		obj := &unstructured.Unstructured{}
		if err := yaml.Unmarshal(doc, obj); err != nil {
			t.Fatalf("decode a document of %s: %v", rel, err)
		}
		if obj.GetKind() == "" {
			continue
		}
		if err := k8s.Create(ctx, obj, opts...); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create %s %s/%s from %s: %v",
				obj.GetKind(), obj.GetNamespace(), obj.GetName(), rel, err)
		}
	}
}
