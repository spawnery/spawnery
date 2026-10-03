//go:build e2e

// Package e2e drives the operator in a real cluster.
//
// It runs the operator under its own ServiceAccount against a real
// authorizer, which catches a permission missing from both the ClusterRole
// and internal/rbacaudit's table. It needs the cluster hack/e2e.sh builds.
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
	// operatorNamespace is deliberately not the chart's default, so a
	// hard-coded spawnery-system in the chart's RBAC fails; see
	// OPERATOR_NAMESPACE in hack/e2e.sh.
	operatorNamespace = "platform-system"

	testNamespace = "minecraft"

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

// TestSpawneryUnderItsOwnServiceAccount is the whole run. The scenarios depend
// on one another, so their order is written down here; the denial check is
// last because it judges everything the run did.
//
// A scenario that patches a ServerGroup's spec also bumps its generation and
// starts a rolling update; say in the assertion which branch it pins.
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
	t.Run("the operator checked its own permissions", theOperatorCheckedItsOwnPermissions)
	t.Run("the operator was never denied", theOperatorWasNeverDenied)
}

func theOperatorIsUp(t *testing.T) {
	pod := operatorPod(t, operatorNamespace)

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

// denialsIn picks the RBAC denials out of an operator log: lines carrying the
// API server's `is forbidden:`, except Pod Security rejections, which
// aForbiddenHostPortIsReportedOnTheGroup causes on purpose.
//
// A green run is evidence about write paths only: a revoked cached read logs
// no denial, and readForwardingSecret folds a 403 into a condition without
// logging. theOperatorCheckedItsOwnPermissions covers the rest.
func denialsIn(log string) []string {
	var offenders []string
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "is forbidden:") &&
			!strings.Contains(line, "violates PodSecurity") {
			offenders = append(offenders, line)
		}
	}
	return offenders
}

func theOperatorWasNeverDenied(t *testing.T) {
	log, restarts := operatorLog(t, operatorNamespace)
	if restarts > 0 {
		t.Errorf("the operator container has restarted %d time(s) during the run. The "+
			"log below covers the current process and, where the API server still "+
			"holds it, the one before it -- but with %d restart(s) this check can no "+
			"longer account for the whole run, so a denial in a process it cannot read "+
			"would look identical to no denial at all", restarts, restarts)
	}

	offenders := denialsIn(log)
	if len(offenders) > 0 {
		t.Errorf("the operator was denied %d time(s) under its own ServiceAccount:\n%s\n\n"+
			"This is the assertion internal/rbacaudit cannot make. It compares the "+
			"generated role against its table in both directions, so a permission "+
			"missing from both leaves it green while this fails.",
			len(offenders), strings.Join(offenders, "\n"))
	}
}

// operatorPod returns the single operator pod in namespace, or fails. The
// tutorial scenario installs into a different namespace than the main suite.
func operatorPod(t *testing.T, namespace string) *corev1.Pod {
	t.Helper()
	var pods corev1.PodList
	err := k8s.List(ctx, &pods,
		client.InNamespace(namespace),
		client.MatchingLabels{
			"app.kubernetes.io/name":      "spawnery",
			"app.kubernetes.io/component": "operator",
		})
	if err != nil {
		t.Fatalf("list operator pods: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("got %d operator pods in %s, want exactly one", len(pods.Items), namespace)
	}
	return &pods.Items[0]
}

// operatorLog reads the operator's log through the API, prepending the
// previous container's where there is one, and reports how many times its
// container has restarted. Kubernetes keeps only one previous log, so beyond
// one restart the log has a hole the caller must report.
func operatorLog(t *testing.T, namespace string) (string, int32) {
	t.Helper()
	pod := operatorPod(t, namespace)

	var restarts int32
	for _, c := range pod.Status.ContainerStatuses {
		restarts += c.RestartCount
	}

	var b strings.Builder
	if restarts > 0 {
		if prev, err := readPodLog(namespace, pod.Name, &corev1.PodLogOptions{Previous: true}); err == nil {
			b.WriteString(prev)
			b.WriteString("\n")
		} else {
			t.Logf("the previous container's log is not available (%v); this check can "+
				"only read the current process", err)
		}
	}

	body, err := readPodLog(namespace, pod.Name, &corev1.PodLogOptions{})
	switch {
	case err == nil:
		b.WriteString(body)
	case restarts > 0:
		// GetLogs refuses a container between restart attempts.
		t.Logf("the current container's log is not available (%v); this check has only "+
			"the previous container's", err)
	default:
		t.Fatalf("read logs of %s: %v", pod.Name, err)
	}
	return b.String(), restarts
}

func readPodLog(namespace, name string, opts *corev1.PodLogOptions) (string, error) {
	stream, err := clientset.CoreV1().Pods(namespace).GetLogs(name, opts).Stream(ctx)
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
// last thing it saw when it gives up. A timeout's denial hint reads the
// operator in operatorNamespace; see eventuallyIn.
func eventually(t *testing.T, deadline time.Duration, what string, cond func() (bool, string)) {
	t.Helper()
	eventuallyIn(t, operatorNamespace, deadline, what, cond)
}

func eventuallyIn(t *testing.T, operatorNS string, deadline time.Duration, what string, cond func() (bool, string)) {
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
	t.Fatalf("timed out after %s waiting for %s; last seen: %s%s", deadline, what, last, denialHint(t, operatorNS))
}

// denialHint is what a timed-out wait adds to its own failure: any RBAC
// denial in the operator log, since a missing permission stalls the scenario
// long before theOperatorWasNeverDenied runs. Best effort.
func denialHint(t *testing.T, namespace string) string {
	t.Helper()
	log, _ := operatorLog(t, namespace)
	offenders := denialsIn(log)
	if len(offenders) == 0 {
		return ""
	}
	shown := offenders
	if len(shown) > 3 {
		shown = shown[:3]
	}
	return fmt.Sprintf("\n\nthe operator was denied %d time(s) while this waited, "+
		"which is the likeliest reason the state never arrived:\n%s",
		len(offenders), strings.Join(shown, "\n"))
}

// eventuallyStable is eventually for a condition that must stay true for the
// whole of hold, since a churning count can pass through the right value on
// its way elsewhere. Its clock resets whenever cond goes false.
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
