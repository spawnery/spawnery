/*
Copyright paul_wtf.

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
// The identity never comes from the message: a compromised server naming
// itself could report zero players for a full server and have it deleted.
package grpcauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"google.golang.org/grpc/peer"
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

type Identity struct {
	Namespace      string
	PodName        string
	PodUID         string
	ServiceAccount string
	Role           agent.Role
	// Group is read here because the pod is fetched during authentication anyway;
	// a proxy session needs it for DrainPlayers fallbacks.
	Group string
}

// TokenReviewer is narrow so an unreachable API server can be tested without
// a cluster. authnclient.TokenReviewInterface satisfies it.
type TokenReviewer interface {
	Create(ctx context.Context, tr *authnv1.TokenReview, opts metav1.CreateOptions) (*authnv1.TokenReview, error)
}

type PodChecker interface {
	LookupPod(ctx context.Context, namespace, name, uid string, role agent.Role) (group string, ok bool, err error)
}

type ClientPodChecker struct{ Client client.Client }

// LookupPod insists on the managed-by label and the requested role label,
// the same two labels OrphanReconciler.Sweep uses for "one of ours"; the two
// must agree, or a pod could pass here yet be swept from the registry.
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

func roleLabelFor(role agent.Role) string {
	if role == agent.RoleProxy {
		return podspec.RoleProxy
	}
	return podspec.RoleServer
}

type Authenticator struct {
	Reviews  TokenReviewer
	Pods     PodChecker
	Audience string

	// Cache may be nil: then every call reviews.
	Cache *ReviewCache

	// Limiter may be nil: then nothing is refused.
	Limiter *PeerLimiter
}

// unavailableErr marks the API server failing to answer, as opposed to a
// refusal; the interceptor maps it to codes.Unavailable so an agent retries.
type unavailableErr struct{ err error }

func (e *unavailableErr) Error() string { return e.err.Error() }
func (e *unavailableErr) Unwrap() error { return e.err }

func wrapUnavailable(err error) error { return &unavailableErr{err} }

func isUnavailable(err error) bool {
	var u *unavailableErr
	return errors.As(err, &u)
}

// exhaustedErr marks a rate-limit refusal; it maps to codes.ResourceExhausted.
type exhaustedErr struct{ err error }

func (e *exhaustedErr) Error() string { return e.err.Error() }
func (e *exhaustedErr) Unwrap() error { return e.err }

func wrapExhausted(err error) error { return &exhaustedErr{err} }

func isExhausted(err error) bool {
	var e *exhaustedErr
	return errors.As(err, &e)
}

// peerAddr is the only identity available before the TokenReview. It returns
// the host, not IP:port: the ephemeral port changes on every dial, and keying
// on it would give a reconnect loop a fresh PeerBurst per connection.
func peerAddr(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok || p.Addr == nil {
		return "unknown"
	}
	addr := p.Addr.String()
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

func serviceAccountFor(role agent.Role) string {
	if role == agent.RoleProxy {
		return podspec.ProxyServiceAccountName
	}
	return podspec.ServerServiceAccountName
}

// TokenReview is cluster-scoped, so this right cannot be narrowed to a namespace
// the way the Secret and the Lease are.
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create

// reviewToken stops short of the role check and the pod lookup, which is what
// makes its answer cacheable: the role varies per call, and the pod lookup must
// stay live.
func (a *Authenticator) reviewToken(ctx context.Context, token string) (reviewResult, error) {
	review, err := a.Reviews.Create(ctx, &authnv1.TokenReview{
		Spec: authnv1.TokenReviewSpec{Token: token, Audiences: []string{a.Audience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return reviewResult{}, wrapUnavailable(fmt.Errorf("token review unavailable: %w", err))
	}
	if !review.Status.Authenticated {
		return reviewResult{}, fmt.Errorf("token not authenticated: %s", review.Status.Error)
	}
	if !containsString(review.Status.Audiences, a.Audience) {
		return reviewResult{}, fmt.Errorf("token not authenticated for audience %q", a.Audience)
	}
	namespace, name, ok := splitServiceAccount(review.Status.User.Username)
	if !ok {
		return reviewResult{}, fmt.Errorf("not a service account: %q", review.Status.User.Username)
	}
	podName := firstExtra(review.Status.User.Extra, claimPodName)
	podUID := firstExtra(review.Status.User.Extra, claimPodUID)
	if podName == "" || podUID == "" {
		return reviewResult{}, fmt.Errorf("token is not bound to a pod")
	}
	return reviewResult{
		Namespace:      namespace,
		ServiceAccount: name,
		PodName:        podName,
		PodUID:         podUID,
	}, nil
}

// Authenticate holds the rate limit rather than the interceptor because the
// limit applies only on a cache miss. Concurrent misses on one token are not
// coalesced: both review and both store the same answer.
func (a *Authenticator) Authenticate(ctx context.Context, token string, want agent.Role) (Identity, error) {
	if token == "" {
		return Identity{}, fmt.Errorf("no token presented")
	}

	res, err, cached := a.Cache.lookup(token)
	if cached {
		ReviewCacheHits.Inc()
	} else {
		ReviewCacheMisses.Inc()
		addr := peerAddr(ctx)
		if !a.Limiter.allow(addr) {
			RateLimited.Inc()
			return Identity{}, wrapExhausted(
				fmt.Errorf("too many token checks from %s", addr))
		}
		res, err = a.reviewToken(ctx, token)
		a.Cache.store(token, res, err)
	}
	if err != nil {
		return Identity{}, err
	}

	// After the cache: the role depends on the session asked for, not the token.
	if wantSA := serviceAccountFor(want); res.ServiceAccount != wantSA {
		return Identity{}, fmt.Errorf("service account %q may not open a %s session, %q may",
			res.ServiceAccount, want, wantSA)
	}

	// Never cached, so deleting a pod revokes it immediately.
	group, exists, err := a.Pods.LookupPod(ctx, res.Namespace, res.PodName, res.PodUID, want)
	if err != nil {
		return Identity{}, wrapUnavailable(fmt.Errorf("look up pod %s/%s: %w", res.Namespace, res.PodName, err))
	}
	if !exists {
		return Identity{}, fmt.Errorf("pod %s/%s is not a Spawnery pod", res.Namespace, res.PodName)
	}

	return Identity{
		Namespace:      res.Namespace,
		PodName:        res.PodName,
		PodUID:         res.PodUID,
		ServiceAccount: res.ServiceAccount,
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
