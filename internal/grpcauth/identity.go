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

// Package grpcauth turns a bearer token into the identity of exactly one pod.
//
// The identity never comes from the message. If a compromised server could
// name itself in Hello, it could report PlayerCount{0} for a full server and
// have it deleted — a direct breach of the core invariant.
package grpcauth

import (
	"context"
	"errors"
	"fmt"
	"strings"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/podspec"
)

const (
	claimPodName = "authentication.kubernetes.io/pod-name"
	claimPodUID  = "authentication.kubernetes.io/pod-uid"

	saPrefix = "system:serviceaccount:"
)

// Identity is who is on the other end of a stream.
type Identity struct {
	Namespace string
	PodName   string
	// PodUID is the registry key — Lookup and Forget are keyed on it.
	PodUID         string
	ServiceAccount string
	Role           agent.Role
	// Group is the pod's spawnery.cloud/group label. A proxy session needs it
	// to know which fallback groups its DrainPlayers messages carry, and the
	// pod is fetched during authentication anyway — reading it here costs one
	// map lookup and saves proxyreg a second Get of the same object.
	Group string
}

// TokenReviewer submits a token to the real authenticator of the API server.
// It is deliberately narrow — just the one method we use — so an unreachable
// API server can be exercised in a test without a cluster.
// authnclient.TokenReviewInterface satisfies it.
type TokenReviewer interface {
	Create(ctx context.Context, tr *authnv1.TokenReview, opts metav1.CreateOptions) (*authnv1.TokenReview, error)
}

// PodChecker answers whether a pod the token names is one of ours, in the role
// the caller wants to act as, and which group it belongs to.
type PodChecker interface {
	LookupPod(ctx context.Context, namespace, name, uid string, role agent.Role) (group string, ok bool, err error)
}

// ClientPodChecker reads through the manager's cache.
type ClientPodChecker struct{ Client client.Client }

// LookupPod implements PodChecker. It insists on the managed-by label and the
// role label matching the requested role, so a hand-built pod — or one
// labelled for the other role — cannot open a session. This mirrors the two
// labels OrphanReconciler.Sweep uses to decide what "one of ours" means; the
// two places must agree, or a pod could pass here yet be swept from the
// registry as foreign.
func (c *ClientPodChecker) LookupPod(ctx context.Context, namespace, name, uid string, role agent.Role) (string, bool, error) {
	pod := &corev1.Pod{}
	err := c.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, pod)
	if apierrors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if string(pod.UID) != uid {
		return "", false, nil
	}
	if pod.Labels[podspec.LabelManagedBy] != podspec.ManagedByValue {
		return "", false, nil
	}
	if pod.Labels[podspec.LabelRole] != roleLabelFor(role) {
		return "", false, nil
	}
	return pod.Labels[podspec.LabelGroup], true, nil
}

// roleLabelFor is the podspec.LabelRole value a pod acting in this role must
// carry.
func roleLabelFor(role agent.Role) string {
	if role == agent.RoleProxy {
		return podspec.RoleProxy
	}
	return podspec.RoleServer
}

// Authenticator checks tokens against the real authenticator of the API
// server.
type Authenticator struct {
	Reviews  TokenReviewer
	Pods     PodChecker
	Audience string
}

// unavailableErr marks an error as the Kubernetes API server itself failing
// to answer — the TokenReview call or the pod lookup — as opposed to a token
// or pod that was checked and refused. The interceptor maps it to
// codes.Unavailable so an agent backs off and retries instead of concluding
// its credentials are wrong.
type unavailableErr struct{ err error }

func (e *unavailableErr) Error() string { return e.err.Error() }
func (e *unavailableErr) Unwrap() error { return e.err }

func wrapUnavailable(err error) error { return &unavailableErr{err} }

// isUnavailable reports whether err means the API server could not be
// reached, rather than that it refused the credentials.
func isUnavailable(err error) bool {
	var u *unavailableErr
	return errors.As(err, &u)
}

// serviceAccountFor is which ServiceAccount may open a session in this role.
func serviceAccountFor(role agent.Role) string {
	if role == agent.RoleProxy {
		return podspec.ProxyServiceAccountName
	}
	return podspec.ServerServiceAccountName
}

// TokenReview is cluster-scoped, so this right cannot be narrowed to a namespace
// the way the Secret and the Lease are.
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create

// Authenticate returns the identity behind a token, or why it is refused.
func (a *Authenticator) Authenticate(ctx context.Context, token string, want agent.Role) (Identity, error) {
	if token == "" {
		return Identity{}, fmt.Errorf("no token presented")
	}

	review, err := a.Reviews.Create(ctx, &authnv1.TokenReview{
		Spec: authnv1.TokenReviewSpec{Token: token, Audiences: []string{a.Audience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return Identity{}, wrapUnavailable(fmt.Errorf("token review unavailable: %w", err))
	}
	if !review.Status.Authenticated {
		return Identity{}, fmt.Errorf("token not authenticated: %s", review.Status.Error)
	}
	if !containsString(review.Status.Audiences, a.Audience) {
		return Identity{}, fmt.Errorf("token not authenticated for audience %q", a.Audience)
	}

	namespace, name, ok := splitServiceAccount(review.Status.User.Username)
	if !ok {
		return Identity{}, fmt.Errorf("not a service account: %q", review.Status.User.Username)
	}
	if wantSA := serviceAccountFor(want); name != wantSA {
		return Identity{}, fmt.Errorf("service account %q may not open a %s session, %q may",
			name, want, wantSA)
	}

	podName := firstExtra(review.Status.User.Extra, claimPodName)
	podUID := firstExtra(review.Status.User.Extra, claimPodUID)
	if podName == "" || podUID == "" {
		return Identity{}, fmt.Errorf("token is not bound to a pod")
	}

	group, exists, err := a.Pods.LookupPod(ctx, namespace, podName, podUID, want)
	if err != nil {
		return Identity{}, wrapUnavailable(fmt.Errorf("look up pod %s/%s: %w", namespace, podName, err))
	}
	if !exists {
		return Identity{}, fmt.Errorf("pod %s/%s is not a Spawnery pod", namespace, podName)
	}

	return Identity{
		Namespace:      namespace,
		PodName:        podName,
		PodUID:         podUID,
		ServiceAccount: name,
		Role:           want,
		Group:          group,
	}, nil
}

func splitServiceAccount(username string) (namespace, name string, ok bool) {
	rest, found := strings.CutPrefix(username, saPrefix)
	if !found {
		return "", "", false
	}
	namespace, name, found = strings.Cut(rest, ":")
	if !found || namespace == "" || name == "" {
		return "", "", false
	}
	return namespace, name, true
}

func firstExtra(extra map[string]authnv1.ExtraValue, key string) string {
	values, ok := extra[key]
	if !ok || len(values) == 0 {
		return ""
	}
	return values[0]
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
