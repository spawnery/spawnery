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
}

// TokenReviewer submits a token to the real authenticator of the API server.
// It is deliberately narrow — just the one method we use — so an unreachable
// API server can be exercised in a test without a cluster.
// authnclient.TokenReviewInterface satisfies it.
type TokenReviewer interface {
	Create(ctx context.Context, tr *authnv1.TokenReview, opts metav1.CreateOptions) (*authnv1.TokenReview, error)
}

// PodChecker answers whether a pod the token names is one of ours.
type PodChecker interface {
	PodExists(ctx context.Context, namespace, name, uid string) (bool, error)
}

// ClientPodChecker reads through the manager's cache.
type ClientPodChecker struct{ Client client.Client }

// PodExists implements PodChecker. It insists on the role label as well, so a
// hand-built pod with the same ServiceAccount cannot open a session.
func (c *ClientPodChecker) PodExists(ctx context.Context, namespace, name, uid string) (bool, error) {
	pod := &corev1.Pod{}
	err := c.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, pod)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if string(pod.UID) != uid {
		return false, nil
	}
	return pod.Labels[podspec.LabelRole] == podspec.RoleServer ||
		pod.Labels[podspec.LabelRole] == podspec.RoleProxy, nil
}

// Authenticator checks tokens against the real authenticator of the API
// server.
type Authenticator struct {
	Reviews  TokenReviewer
	Pods     PodChecker
	Audience string
}

// serviceAccountFor is which ServiceAccount may open a session in this role.
func serviceAccountFor(role agent.Role) string {
	if role == agent.RoleProxy {
		return podspec.ProxyServiceAccountName
	}
	return podspec.ServerServiceAccountName
}

// Authenticate returns the identity behind a token, or why it is refused.
func (a *Authenticator) Authenticate(ctx context.Context, token string, want agent.Role) (Identity, error) {
	if token == "" {
		return Identity{}, fmt.Errorf("no token presented")
	}

	review, err := a.Reviews.Create(ctx, &authnv1.TokenReview{
		Spec: authnv1.TokenReviewSpec{Token: token, Audiences: []string{a.Audience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return Identity{}, fmt.Errorf("token review unavailable: %w", err)
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

	exists, err := a.Pods.PodExists(ctx, namespace, podName, podUID)
	if err != nil {
		return Identity{}, fmt.Errorf("look up pod %s/%s: %w", namespace, podName, err)
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
