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
	// operatorNamespace is where config/deploy/ puts everything, and the
	// literal the kubebuilder markers carry for the operator's own Secret and
	// Lease rights. See docs/known-issues.md, "spawnery-system is hard-wired
	// into the RBAC markers".
	operatorNamespace = "spawnery-system"

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
			t.Errorf("the operator container has restarted %d time(s); the log below is "+
				"the current process only, so an earlier denial may not appear in it",
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
func theOperatorWasNeverDenied(t *testing.T) {
	var offenders []string
	for _, line := range strings.Split(operatorLog(t), "\n") {
		if strings.Contains(line, "is forbidden:") {
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

// operatorLog reads the operator's whole log through the API.
func operatorLog(t *testing.T) string {
	t.Helper()
	pod := operatorPod(t)
	stream, err := clientset.CoreV1().
		Pods(operatorNamespace).
		GetLogs(pod.Name, &corev1.PodLogOptions{}).
		Stream(ctx)
	if err != nil {
		t.Fatalf("stream logs of %s: %v", pod.Name, err)
	}
	defer func() { _ = stream.Close() }()

	body, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read logs of %s: %v", pod.Name, err)
	}
	return string(body)
}

// eventually polls cond until it holds or the deadline passes, and reports the
// last thing it saw when it gives up.
//
// It is the only waiting construct in this package. A run built on fixed sleeps
// turns flaky under load, and a flaky E2E run is ignored within weeks -- which
// is §4 of the 2026-08-07 E2E design, kept.
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
