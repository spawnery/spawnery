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

package controller

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/podspec"
	"github.com/spawnery/spawnery/internal/voice"
)

var errForeignVoiceService = errors.New("the voice Service name is taken by an object this group does not control")

// voiceServicePort is the UDP twin of the game's port: the same number and,
// for NodePort, the same node port, since the API server keys both on protocol.
func voiceServicePort(nodePort int32) corev1.ServicePort {
	return corev1.ServicePort{
		Name:       podspec.VoicePortName,
		Port:       voice.RelayPort,
		TargetPort: intstr.FromString(podspec.VoicePortName),
		Protocol:   corev1.ProtocolUDP,
		NodePort:   nodePort,
	}
}

// voiceOwnService is the override voice leaves by when it needs a Service of
// its own; HostPort binds on the node instead.
func voiceOwnService(network *spawneryv1alpha1.Network, group *spawneryv1alpha1.ProxyGroup) (spawneryv1alpha1.ExposeSpec, bool) {
	if _, on := network.VoiceBackendPort(); !on {
		return spawneryv1alpha1.ExposeSpec{}, false
	}
	spec, own := group.VoiceExpose()
	return spec, own && spec.Type != spawneryv1alpha1.ExposeHostPort
}

func (r *ProxyGroupReconciler) reconcileVoiceService(
	ctx context.Context,
	network *spawneryv1alpha1.Network,
	group *spawneryv1alpha1.ProxyGroup,
) (*corev1.Service, error) {
	name := podspec.VoiceServiceName(group.Name)
	spec, wanted := voiceOwnService(network, group)

	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: group.Namespace}, existing)
	switch {
	case apierrors.IsNotFound(err):
		existing = nil
	case err != nil:
		return nil, err
	}
	if existing != nil {
		if owner := metav1.GetControllerOf(existing); owner == nil || owner.UID != group.UID {
			if wanted {
				return nil, errForeignVoiceService
			}
			return nil, nil
		}
		if !wanted {
			uid, rv := existing.UID, existing.ResourceVersion
			return nil, client.IgnoreNotFound(r.Delete(ctx, existing, &client.DeleteOptions{
				Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv},
			}))
		}
	}
	if !wanted {
		return nil, nil
	}

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: group.Namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if svc.Labels == nil {
			svc.Labels = map[string]string{}
		}
		svc.Labels[podspec.LabelManagedBy] = podspec.ManagedByValue
		var lbAnnotations map[string]string
		if spec.Type == spawneryv1alpha1.ExposeLoadBalancer && spec.LoadBalancer != nil {
			lbAnnotations = spec.LoadBalancer.Annotations
		}
		applyExposeAnnotations(svc, lbAnnotations)
		svc.Spec.Selector = podspec.ProxyLabels(group.Spec.NetworkRef.Name, group.Name)
		port := voiceServicePort(0)
		svc.Spec.ExternalTrafficPolicy = ""
		switch spec.Type {
		case spawneryv1alpha1.ExposeLoadBalancer:
			svc.Spec.Type = corev1.ServiceTypeLoadBalancer
			policy := corev1.ServiceExternalTrafficPolicyLocal
			if spec.LoadBalancer != nil && spec.LoadBalancer.ExternalTrafficPolicy != "" {
				policy = spec.LoadBalancer.ExternalTrafficPolicy
			}
			svc.Spec.ExternalTrafficPolicy = policy
		case spawneryv1alpha1.ExposeNodePort:
			svc.Spec.Type = corev1.ServiceTypeNodePort
			svc.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyLocal
			if spec.NodePort != nil {
				port.NodePort = spec.NodePort.Port
			}
		case spawneryv1alpha1.ExposeClusterIP:
			svc.Spec.Type = corev1.ServiceTypeClusterIP
		default:
			return fmt.Errorf("voice expose.type %s reached reconcileVoiceService without a branch", spec.Type)
		}
		svc.Spec.Ports = []corev1.ServicePort{port}
		return controllerutil.SetControllerReference(group, svc, r.Scheme)
	})
	if err != nil {
		return nil, err
	}
	return svc, nil
}

func allocatedVoiceNodePort(svc *corev1.Service) int32 {
	if svc == nil {
		return 0
	}
	for _, p := range svc.Spec.Ports {
		if p.Name == podspec.VoicePortName {
			return p.NodePort
		}
	}
	return 0
}

func voiceAddress(
	network *spawneryv1alpha1.Network,
	group *spawneryv1alpha1.ProxyGroup,
	pods []corev1.Pod,
	gameSvc, voiceSvc *corev1.Service,
) string {
	if _, on := network.VoiceBackendPort(); !on {
		return ""
	}
	join := func(host string, port int32) string { return net.JoinHostPort(host, strconv.Itoa(int(port))) }
	spec, own := group.VoiceExpose()
	if !own {
		if group.Status.Address == "" {
			return ""
		}
		host, _, err := net.SplitHostPort(group.Status.Address)
		if err != nil {
			host = group.Status.Address
		}
		return join(host, voice.PublicPort(group))
	}
	switch spec.Type {
	case spawneryv1alpha1.ExposeHostPort:
		if spec.HostPort == nil {
			return ""
		}
		ip := readyHostIPBindingPort(pods, spec.HostPort.Port)
		if ip == "" {
			return ""
		}
		return join(ip, spec.HostPort.Port)
	case spawneryv1alpha1.ExposeNodePort:
		ip, port := readyHostIP(pods), allocatedVoiceNodePort(voiceSvc)
		if ip == "" || port == 0 {
			return ""
		}
		return join(ip, port)
	case spawneryv1alpha1.ExposeLoadBalancer:
		if voiceSvc == nil || readyHostIP(pods) == "" {
			return ""
		}
		for _, ing := range voiceSvc.Status.LoadBalancer.Ingress {
			if ing.IP != "" {
				return join(ing.IP, voice.RelayPort)
			}
			if ing.Hostname != "" {
				return join(ing.Hostname, voice.RelayPort)
			}
		}
		return ""
	case spawneryv1alpha1.ExposeClusterIP:
		if voiceSvc == nil || spec.ClusterIP == nil || readyHostIP(pods) == "" {
			return ""
		}
		return spec.ClusterIP.Address
	}
	return ""
}

// voicePluginPod names a connected proxy of the group that has Simple Voice
// Chat's own Velocity plugin loaded, or "".
func voicePluginPod(agents *agent.Registry, pods []corev1.Pod) string {
	return ""
}

func setVoiceCondition(group *spawneryv1alpha1.ProxyGroup, network *spawneryv1alpha1.Network, foreign bool, conflictPod string) {
	_, on := network.VoiceBackendPort()
	cond := metav1.Condition{Type: spawneryv1alpha1.ConditionVoice, Status: metav1.ConditionTrue,
		Reason: spawneryv1alpha1.ReasonVoiceRelaying, Message: "this group's proxies relay voice chat"}
	switch {
	case !on && group.Spec.Voice == nil:
		meta.RemoveStatusCondition(&group.Status.Conditions, spawneryv1alpha1.ConditionVoice)
		return
	case !on:
		cond.Status, cond.Reason = metav1.ConditionFalse, spawneryv1alpha1.ReasonVoiceOffInNetwork
		cond.Message = fmt.Sprintf("network %q has no spec.voice; this group's spec.voice is ignored", network.Name)
	case foreign:
		cond.Status, cond.Reason = metav1.ConditionFalse, spawneryv1alpha1.ReasonVoiceServiceNotOurs
		cond.Message = fmt.Sprintf("Service %q exists and is not controlled by this group; voice has no way out",
			podspec.VoiceServiceName(group.Name))
	case conflictPod != "":
		cond.Status, cond.Reason = metav1.ConditionFalse, spawneryv1alpha1.ReasonVoicePluginConflict
		cond.Message = fmt.Sprintf("proxy %s has Simple Voice Chat's own Velocity plugin loaded; "+
			"remove it, spawnery relays voice itself", conflictPod)
	}
	meta.SetStatusCondition(&group.Status.Conditions, cond)
}
