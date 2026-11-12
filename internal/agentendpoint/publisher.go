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

// Package agentendpoint publishes the leader's address as the EndpointSlices
// of the selectorless agent Service.
package agentendpoint

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/spawnery/spawnery/internal/leaseguard"
)

const (
	// ManagedBy differs from the EndpointSlice controller's value, so that
	// controller leaves these slices alone.
	ManagedBy       = "spawnery.cloud-operator"
	DefaultInterval = 30 * time.Second

	portName         = "agent"
	withdrawDeadline = 2 * time.Second
	// controller-runtime's leader election retry period.
	retryPeriod = 2 * time.Second
)

var ErrNotHolder = leaseguard.ErrNotHolder

var families = []discoveryv1.AddressType{discoveryv1.AddressTypeIPv4, discoveryv1.AddressTypeIPv6}

type Identity struct {
	Name, Hostname, Node string
	UID                  types.UID
	IPs                  []string
}

// Publisher uses an uncached client: with --namespace set, the manager's cache
// does not cover the operator's own namespace.
type Publisher struct {
	Client    client.Client
	Namespace string
	Service   string
	Port      int32
	// Lease is the manager's LeaderElectionID. Empty means no holder check,
	// for a manager without leader election: no Lease exists, and the
	// publisher is then the only one.
	Lease    string
	Pod      Identity
	Serving  <-chan struct{}
	Interval time.Duration
}

// spawnery-system is a placeholder that hack/chart-templates.sh replaces with
// the release namespace.
// +kubebuilder:rbac:groups=discovery.k8s.io,namespace=spawnery-system,resources=endpointslices,verbs=get;list;create;update;delete
// +kubebuilder:rbac:groups="",namespace=spawnery-system,resources=endpoints,verbs=get;delete
// No services marker: the ClusterRole's services get already reaches this
// namespace.

// ParseIPs reads the downward API's status.podIPs, comma-separated.
func ParseIPs(podIPs string) ([]string, error) {
	if strings.TrimSpace(podIPs) == "" {
		return nil, errors.New("no pod IPs")
	}
	var ips []string
	for _, field := range strings.Split(podIPs, ",") {
		addr, err := netip.ParseAddr(strings.TrimSpace(field))
		if err != nil {
			return nil, fmt.Errorf("pod IPs %q: %w", podIPs, err)
		}
		ips = append(ips, addr.String())
	}
	return ips, nil
}

func SliceName(service string, family discoveryv1.AddressType) string {
	return service + "-agent-" + strings.ToLower(string(family))
}

func (p *Publisher) NeedLeaderElection() bool { return true }

func (p *Publisher) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("agentendpoint")
	defer func() {
		withdrawCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), withdrawDeadline)
		defer cancel()
		if err := p.Withdraw(withdrawCtx); err != nil {
			logger.Error(err, "withdrawing the agent endpoint")
		}
	}()

	select {
	case <-p.Serving:
	case <-ctx.Done():
		return nil
	}

	interval := p.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	published := false
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		err := p.Assert(ctx)
		switch {
		case err != nil && !published:
			logger.Error(err, "publishing the agent endpoint")
			timer.Reset(retryPeriod)
			continue
		case err != nil:
			logger.Error(err, "re-asserting the agent endpoint")
		case !published:
			published = true
			logger.Info("published the agent endpoint", "service", p.Service, "ips", p.Pod.IPs)
		}
		timer.Reset(interval)
	}
}

// Assert reads the Lease before any write, so a leader resumed after a freeze
// past its lease finds its successor named there and writes nothing.
func (p *Publisher) Assert(ctx context.Context) error {
	if err := p.checkHolder(ctx); err != nil {
		return err
	}

	svc := &corev1.Service{}
	if err := p.Client.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: p.Service}, svc); err != nil {
		return fmt.Errorf("get Service %s: %w", p.Service, err)
	}

	byFamily := map[discoveryv1.AddressType]string{}
	for _, ip := range p.Pod.IPs {
		family := familyOf(ip)
		if _, seen := byFamily[family]; !seen {
			byFamily[family] = ip
		}
	}
	for _, family := range families {
		ip, ok := byFamily[family]
		if !ok {
			if err := p.deleteSlice(ctx, family); err != nil {
				return err
			}
			continue
		}
		if err := p.writeSlice(ctx, svc, family, ip); err != nil {
			return err
		}
	}

	return errors.Join(p.deleteEndpoints(ctx), p.deleteForeignSlices(ctx))
}

func (p *Publisher) checkHolder(ctx context.Context) error {
	if p.Lease == "" {
		return nil
	}
	return leaseguard.CheckHolder(ctx, p.Client, types.NamespacedName{Namespace: p.Namespace, Name: p.Lease}, p.Pod.Hostname)
}

func (p *Publisher) writeSlice(ctx context.Context, svc *corev1.Service, family discoveryv1.AddressType, ip string) error {
	return retry.OnError(retry.DefaultRetry, func(err error) bool {
		return apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err)
	}, func() error { return p.writeSliceOnce(ctx, svc, family, ip) })
}

func (p *Publisher) writeSliceOnce(ctx context.Context, svc *corev1.Service, family discoveryv1.AddressType, ip string) error {
	name := SliceName(p.Service, family)
	existing := &discoveryv1.EndpointSlice{}
	err := p.Client.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: name}, existing)
	if apierrors.IsNotFound(err) {
		s := &discoveryv1.EndpointSlice{
			ObjectMeta:  metav1.ObjectMeta{Name: name, Namespace: p.Namespace},
			AddressType: family,
		}
		p.fill(s, svc, ip)
		if err := p.Client.Create(ctx, s); err != nil {
			return fmt.Errorf("create EndpointSlice %s: %w", name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get EndpointSlice %s: %w", name, err)
	}

	desired := existing.DeepCopy()
	p.fill(desired, svc, ip)
	if equality.Semantic.DeepEqual(existing, desired) {
		return nil
	}
	if err := p.Client.Update(ctx, desired); err != nil {
		return fmt.Errorf("update EndpointSlice %s: %w", name, err)
	}
	return nil
}

func (p *Publisher) fill(s *discoveryv1.EndpointSlice, svc *corev1.Service, ip string) {
	if s.Labels == nil {
		s.Labels = map[string]string{}
	}
	s.Labels[discoveryv1.LabelServiceName] = p.Service
	s.Labels[discoveryv1.LabelManagedBy] = ManagedBy
	// No blockOwnerDeletion: setting it needs update on services/finalizers.
	s.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "v1", Kind: "Service", Name: svc.Name, UID: svc.UID,
	}}
	ep := discoveryv1.Endpoint{
		Addresses:  []string{ip},
		Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
		TargetRef: &corev1.ObjectReference{
			Kind: "Pod", Namespace: p.Namespace, Name: p.Pod.Name, UID: p.Pod.UID,
		},
	}
	if p.Pod.Node != "" {
		ep.NodeName = ptr.To(p.Pod.Node)
	}
	s.Endpoints = []discoveryv1.Endpoint{ep}
	// For a selectorless Service kube-proxy takes the port from the slice, and
	// matches it to the Service port by name.
	s.Ports = []discoveryv1.EndpointPort{{
		Name: ptr.To(portName), Port: ptr.To(p.Port), Protocol: ptr.To(corev1.ProtocolTCP),
	}}
}

func (p *Publisher) deleteSlice(ctx context.Context, family discoveryv1.AddressType) error {
	s := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{
		Name: SliceName(p.Service, family), Namespace: p.Namespace,
	}}
	if err := p.Client.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete EndpointSlice %s: %w", s.Name, err)
	}
	return nil
}

// deleteEndpoints removes what the endpoints controller wrote while the
// Service had a selector; the mirroring controller would otherwise turn it
// into one more slice. Metadata only, because the typed Endpoints API is
// deprecated.
func (p *Publisher) deleteEndpoints(ctx context.Context) error {
	ep := &metav1.PartialObjectMetadata{}
	ep.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Endpoints"))
	err := p.Client.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: p.Service}, ep)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get Endpoints %s: %w", p.Service, err)
	}
	if err := p.Client.Delete(ctx, ep); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete Endpoints %s: %w", p.Service, err)
	}
	return nil
}

// deleteForeignSlices removes the EndpointSlice controller's slices, which it
// stops maintaining, but does not delete, once the Service loses its selector.
func (p *Publisher) deleteForeignSlices(ctx context.Context) error {
	list := &discoveryv1.EndpointSliceList{}
	if err := p.Client.List(ctx, list, client.InNamespace(p.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: p.Service}); err != nil {
		return fmt.Errorf("list EndpointSlices of %s: %w", p.Service, err)
	}
	var errs []error
	for i := range list.Items {
		s := &list.Items[i]
		if s.Labels[discoveryv1.LabelManagedBy] == ManagedBy {
			continue
		}
		if err := p.Client.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("delete EndpointSlice %s: %w", s.Name, err))
		}
	}
	return errors.Join(errs...)
}

// Withdraw empties only slices that still name this pod, and needs no Lease:
// the resourceVersion from its read makes an update that races the next
// leader's write fail with a conflict, which it leaves alone.
func (p *Publisher) Withdraw(ctx context.Context) error {
	var errs []error
	for _, family := range families {
		name := SliceName(p.Service, family)
		s := &discoveryv1.EndpointSlice{}
		err := p.Client.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: name}, s)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("get EndpointSlice %s: %w", name, err))
			continue
		}
		if !namesOnly(s, p.Pod.UID) {
			continue
		}
		s.Endpoints = []discoveryv1.Endpoint{}
		if err := p.Client.Update(ctx, s); err != nil && !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("empty EndpointSlice %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func namesOnly(s *discoveryv1.EndpointSlice, uid types.UID) bool {
	if len(s.Endpoints) == 0 {
		return false
	}
	for _, ep := range s.Endpoints {
		if ep.TargetRef == nil || ep.TargetRef.UID != uid {
			return false
		}
	}
	return true
}

func familyOf(ip string) discoveryv1.AddressType {
	if addr, err := netip.ParseAddr(ip); err == nil && addr.Is4() {
		return discoveryv1.AddressTypeIPv4
	}
	return discoveryv1.AddressTypeIPv6
}
