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

package grpcauth_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/grpcauth"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/testenv"
)

type authFixture struct {
	t    *testing.T
	ctx  context.Context
	c    client.Client
	cs   *kubernetes.Clientset
	ns   string
	auth *grpcauth.Authenticator
}

func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	c, ctx := testenv.Client(t)
	ns := testenv.Namespace(t, ctx, c)
	cs, err := kubernetes.NewForConfig(testenv.Config(t))
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	for _, name := range []string{podspec.ServerServiceAccountName, podspec.ProxyServiceAccountName} {
		sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		if err := c.Create(ctx, sa); err != nil {
			t.Fatalf("create ServiceAccount %s: %v", name, err)
		}
	}
	return &authFixture{
		t: t, ctx: ctx, c: c, cs: cs, ns: ns,
		auth: &grpcauth.Authenticator{
			Reviews:  cs.AuthenticationV1().TokenReviews(),
			Pods:     &grpcauth.ClientPodChecker{Client: c},
			Audience: podspec.AgentTokenAudience,
		},
	}
}

// pod creates a managed server pod and returns it.
func (f *authFixture) pod(name string) *corev1.Pod {
	f.t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: f.ns,
			Labels:    podspec.ServerLabels("production", "lobby", name),
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: podspec.ServerServiceAccountName,
			Containers:         []corev1.Container{{Name: "minecraft", Image: "example/paper:1"}},
		},
	}
	if err := f.c.Create(f.ctx, pod); err != nil {
		f.t.Fatalf("create pod: %v", err)
	}
	return pod
}

// proxyPod creates a pod running under the proxy ServiceAccount. It exists
// only so a proxy token can be minted at all: the real TokenRequest API
// refuses to bind a token for one ServiceAccount to a pod that runs under a
// different one, so a proxy token needs a genuine proxy pod behind it.
func (f *authFixture) proxyPod(name string) *corev1.Pod {
	f.t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: f.ns,
			Labels: map[string]string{
				podspec.LabelManagedBy: podspec.ManagedByValue,
				podspec.LabelNetwork:   "production",
				podspec.LabelGroup:     "gateway",
				podspec.LabelRole:      podspec.RoleProxy,
			},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: podspec.ProxyServiceAccountName,
			Containers:         []corev1.Container{{Name: "velocity", Image: "example/velocity:1"}},
		},
	}
	if err := f.c.Create(f.ctx, pod); err != nil {
		f.t.Fatalf("create pod: %v", err)
	}
	return pod
}

// token mints a token the way the kubelet would.
func (f *authFixture) token(sa string, audiences []string, boundTo *corev1.Pod) string {
	f.t.Helper()
	spec := authnv1.TokenRequestSpec{
		Audiences:         audiences,
		ExpirationSeconds: ptr.To(int64(600)),
	}
	if boundTo != nil {
		spec.BoundObjectRef = &authnv1.BoundObjectReference{
			Kind: "Pod", APIVersion: "v1", Name: boundTo.Name, UID: boundTo.UID,
		}
	}
	tr, err := f.cs.CoreV1().ServiceAccounts(f.ns).CreateToken(f.ctx, sa,
		&authnv1.TokenRequest{Spec: spec}, metav1.CreateOptions{})
	if err != nil {
		f.t.Fatalf("TokenRequest for %s: %v", sa, err)
	}
	return tr.Status.Token
}

func TestAcceptsAPodBoundServerToken(t *testing.T) {
	f := newAuthFixture(t)
	pod := f.pod("lobby-abcd")

	id, err := f.auth.Authenticate(f.ctx, f.token(podspec.ServerServiceAccountName,
		[]string{podspec.AgentTokenAudience}, pod), agent.RoleServer)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	if id.PodName != pod.Name {
		t.Errorf("PodName = %q, want %q", id.PodName, pod.Name)
	}
	if id.PodUID != string(pod.UID) {
		t.Errorf("PodUID = %q, want %q", id.PodUID, pod.UID)
	}
	if id.Namespace != f.ns {
		t.Errorf("Namespace = %q, want %q", id.Namespace, f.ns)
	}
	if id.Role != agent.RoleServer {
		t.Errorf("Role = %q, want %q", id.Role, agent.RoleServer)
	}
}

// Each of these must be refused. Without them the audit says nothing.
func TestRejections(t *testing.T) {
	cases := []struct {
		name    string
		token   func(f *authFixture) string
		role    agent.Role
		wantErr string
	}{
		{
			name: "token without an audience",
			token: func(f *authFixture) string {
				return f.token(podspec.ServerServiceAccountName, nil, f.pod("lobby-noaud"))
			},
			role:    agent.RoleServer,
			wantErr: "not authenticated",
		},
		{
			name: "token for another audience",
			token: func(f *authFixture) string {
				return f.token(podspec.ServerServiceAccountName, []string{"something-else"}, f.pod("lobby-otheraud"))
			},
			role:    agent.RoleServer,
			wantErr: "not authenticated",
		},
		{
			name: "token not bound to a pod",
			token: func(f *authFixture) string {
				return f.token(podspec.ServerServiceAccountName, []string{podspec.AgentTokenAudience}, nil)
			},
			role:    agent.RoleServer,
			wantErr: "not bound to a pod",
		},
		{
			name: "proxy token on a server session",
			token: func(f *authFixture) string {
				pod := f.proxyPod("gateway-abcd")
				return f.token(podspec.ProxyServiceAccountName, []string{podspec.AgentTokenAudience}, pod)
			},
			role:    agent.RoleServer,
			wantErr: "service account",
		},
		{
			name:    "garbage instead of a token",
			token:   func(f *authFixture) string { return "not.a.token" },
			role:    agent.RoleServer,
			wantErr: "not authenticated",
		},
		{
			name:    "empty token",
			token:   func(f *authFixture) string { return "" },
			role:    agent.RoleServer,
			wantErr: "no token",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuthFixture(t)
			_, err := f.auth.Authenticate(f.ctx, tc.token(f), tc.role)
			if err == nil {
				t.Fatal("Authenticate accepted the token")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// Defence in depth: a hand-built pod using the same ServiceAccount can never
// speak for another server, but it must not be able to fill the registry with
// entries that have no CR behind them either.
func TestRejectsAPodThatIsNotOurs(t *testing.T) {
	f := newAuthFixture(t)

	foreign := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "not-ours", Namespace: f.ns},
		Spec: corev1.PodSpec{
			ServiceAccountName: podspec.ServerServiceAccountName,
			Containers:         []corev1.Container{{Name: "c", Image: "example/x:1"}},
		},
	}
	if err := f.c.Create(f.ctx, foreign); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	_, err := f.auth.Authenticate(f.ctx, f.token(podspec.ServerServiceAccountName,
		[]string{podspec.AgentTokenAudience}, foreign), agent.RoleServer)
	if err == nil {
		t.Fatal("Authenticate accepted a pod without the spawnery role label")
	}
	if !strings.Contains(err.Error(), "pod") {
		t.Errorf("error = %q, want it to mention the pod", err)
	}
}

// The pod exists, is genuinely managed by Spawnery, and its ServiceAccount
// matches — but its role label says proxy while a ServerSession is what was
// requested. PodExists must refuse it on the role label alone.
func TestRejectsAPodLabelledForTheOtherRole(t *testing.T) {
	f := newAuthFixture(t)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lobby-wrongrole",
			Namespace: f.ns,
			Labels: map[string]string{
				podspec.LabelManagedBy: podspec.ManagedByValue,
				podspec.LabelNetwork:   "production",
				podspec.LabelGroup:     "lobby",
				podspec.LabelRole:      podspec.RoleProxy, // wrong: a ServerSession is what is requested below
			},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: podspec.ServerServiceAccountName,
			Containers:         []corev1.Container{{Name: "minecraft", Image: "example/paper:1"}},
		},
	}
	if err := f.c.Create(f.ctx, pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	_, err := f.auth.Authenticate(f.ctx, f.token(podspec.ServerServiceAccountName,
		[]string{podspec.AgentTokenAudience}, pod), agent.RoleServer)
	if err == nil {
		t.Fatal("Authenticate accepted a pod labelled for the other role")
	}
	if !strings.Contains(err.Error(), "pod") {
		t.Errorf("error = %q, want it to mention the pod", err)
	}
}

// The pod carries the right role label but not the managed-by label.
// OrphanReconciler.Sweep treats both labels together as "one of ours"; if
// PodExists accepted the role label alone, a pod that Sweep would reap as
// foreign could still open a session in the meantime.
func TestRejectsAPodWithoutTheManagedByLabel(t *testing.T) {
	f := newAuthFixture(t)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lobby-nomanagedby",
			Namespace: f.ns,
			Labels:    map[string]string{podspec.LabelRole: podspec.RoleServer},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: podspec.ServerServiceAccountName,
			Containers:         []corev1.Container{{Name: "minecraft", Image: "example/paper:1"}},
		},
	}
	if err := f.c.Create(f.ctx, pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	_, err := f.auth.Authenticate(f.ctx, f.token(podspec.ServerServiceAccountName,
		[]string{podspec.AgentTokenAudience}, pod), agent.RoleServer)
	if err == nil {
		t.Fatal("Authenticate accepted a pod without the managed-by label")
	}
	if !strings.Contains(err.Error(), "pod") {
		t.Errorf("error = %q, want it to mention the pod", err)
	}
}

// An unreachable API server must look different from a refused token: the
// agent should back off and retry, not conclude its credentials are wrong.
// This is the one case that needs no cluster, hence the narrow TokenReviewer.
func TestTokenReviewUnavailableIsNotARejection(t *testing.T) {
	a := &grpcauth.Authenticator{
		Reviews:  failingReviewer{},
		Pods:     refusingPodChecker{},
		Audience: podspec.AgentTokenAudience,
	}

	_, err := a.Authenticate(context.Background(), "some-token", agent.RoleServer)
	if err == nil {
		t.Fatal("Authenticate succeeded although the API server was unreachable")
	}
	if !strings.Contains(err.Error(), "unavailable") {
		t.Errorf("error = %q, want it to name the outage rather than the token", err)
	}
	// The pod checker must never be reached — the review failed first.
}

type failingReviewer struct{}

func (failingReviewer) Create(context.Context, *authnv1.TokenReview, metav1.CreateOptions) (
	*authnv1.TokenReview, error) {
	return nil, errors.New("connection refused")
}

type refusingPodChecker struct{}

func (refusingPodChecker) LookupPod(context.Context, string, string, string, agent.Role) (string, bool, error) {
	return "", false, errors.New("the pod checker must not be reached")
}

// The group label of the pod has to survive the trip through the token, because
// a proxy session's DrainPlayers messages carry the fallback groups of exactly
// that ProxyGroup and nothing on the wire could tell the operator which one it
// is.
func TestIdentityCarriesTheGroupLabel(t *testing.T) {
	f := newAuthFixture(t)

	server := f.pod("lobby-group")
	id, err := f.auth.Authenticate(f.ctx, f.token(podspec.ServerServiceAccountName,
		[]string{podspec.AgentTokenAudience}, server), agent.RoleServer)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.Group != "lobby" {
		t.Errorf("server Group = %q, want %q", id.Group, "lobby")
	}

	proxy := f.proxyPod("gateway-group")
	id, err = f.auth.Authenticate(f.ctx, f.token(podspec.ProxyServiceAccountName,
		[]string{podspec.AgentTokenAudience}, proxy), agent.RoleProxy)
	if err != nil {
		t.Fatalf("Authenticate proxy: %v", err)
	}
	if id.Group != "gateway" {
		t.Errorf("proxy Group = %q, want %q", id.Group, "gateway")
	}
	if id.Role != agent.RoleProxy {
		t.Errorf("Role = %q, want %q", id.Role, agent.RoleProxy)
	}
}

// The ReviewCache caches the token review, never the pod lookup. This is the
// property the whole split in Authenticate exists to preserve: if the pod
// lookup were ever folded into what the cache remembers, deleting a pod would
// stop being an immediate revocation for as long as the cached entry lived.
// Only a real API server can show this, because a fake PodChecker cannot
// distinguish "queried and found gone" from "never queried."
func TestDeletingAPodRevokesImmediatelyDespiteTheCache(t *testing.T) {
	f := newAuthFixture(t)
	f.auth.Cache = grpcauth.NewReviewCache(time.Now)

	pod := f.pod("lobby-revoke")
	token := f.token(podspec.ServerServiceAccountName,
		[]string{podspec.AgentTokenAudience}, pod)

	if _, err := f.auth.Authenticate(f.ctx, token, agent.RoleServer); err != nil {
		t.Fatalf("first Authenticate: %v", err)
	}

	if err := f.c.Delete(f.ctx, pod); err != nil {
		t.Fatalf("delete pod: %v", err)
	}

	// The token review itself is now served from the cache -- PositiveTTL is
	// 60s, comfortably longer than this test takes to run -- so this proves
	// the pod lookup ran live even on a cache hit.
	if _, err := f.auth.Authenticate(f.ctx, token, agent.RoleServer); err == nil {
		t.Fatal("Authenticate accepted a token for a deleted pod; " +
			"the cache must never cover the pod lookup")
	}
}

func TestRoleForMethod(t *testing.T) {
	cases := []struct {
		method string
		want   agent.Role
		ok     bool
	}{
		{"/spawnery.agent.v1alpha1.AgentService/ServerSession", agent.RoleServer, true},
		{"/spawnery.agent.v1alpha1.AgentService/ProxySession", agent.RoleProxy, true},
		{"/spawnery.agent.v1alpha1.AgentService/Anything", "", false},
	}
	for _, tc := range cases {
		got, ok := grpcauth.RoleForMethod(tc.method)
		if ok != tc.ok || got != tc.want {
			t.Errorf("RoleForMethod(%q) = %q,%v want %q,%v", tc.method, got, ok, tc.want, tc.ok)
		}
	}
}
