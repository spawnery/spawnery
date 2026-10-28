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

// Package voice decides what a proxy tells a Simple Voice Chat client about
// where to send its UDP. The controller publishes the result on the group's
// status and proxyreg sends it to the group's proxies, so both read it here.
package voice

import (
	"net"
	"strconv"

	corev1 "k8s.io/api/core/v1"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
)

// RelayPort is the UDP port the relay binds inside every proxy pod.
const RelayPort int32 = 25565

// Endpoint is what the secret packet is rewritten to. With no Host and no
// OwnNode, the client keeps the host it joined through.
type Endpoint struct {
	Host    string
	Port    int32
	OwnNode bool
}

// PublicPort is the UDP port clients dial, by the strategy voice leaves by.
func PublicPort(group *spawneryv1alpha1.ProxyGroup) int32 {
	spec, _ := group.VoiceExpose()
	switch spec.Type {
	case spawneryv1alpha1.ExposeNodePort:
		if spec.NodePort != nil {
			return spec.NodePort.Port
		}
	case spawneryv1alpha1.ExposeHostPort:
		if spec.HostPort != nil {
			return spec.HostPort.Port
		}
	case spawneryv1alpha1.ExposeClusterIP:
		if spec.ClusterIP != nil {
			if _, port, ok := splitAddress(spec.ClusterIP.Address); ok {
				return port
			}
		}
		return 0
	case spawneryv1alpha1.ExposeLoadBalancer:
		return RelayPort
	}
	return 0
}

// Announce is the endpoint for this group's clients; false when voice is off,
// there is no port, the LoadBalancer has no ingress yet, or a ClusterIP
// address is missing or unparsable.
func Announce(network *spawneryv1alpha1.Network, group *spawneryv1alpha1.ProxyGroup) (Endpoint, bool) {
	if _, on := network.VoiceBackendPort(); !on {
		return Endpoint{}, false
	}
	spec, own := group.VoiceExpose()
	port := PublicPort(group)
	if port == 0 {
		return Endpoint{}, false
	}
	if !own {
		return Endpoint{Port: port}, true
	}
	switch spec.Type {
	case spawneryv1alpha1.ExposeHostPort, spawneryv1alpha1.ExposeNodePort:
		return Endpoint{Port: port, OwnNode: true}, true
	case spawneryv1alpha1.ExposeClusterIP:
		if spec.ClusterIP == nil {
			return Endpoint{}, false
		}
		host, _, ok := splitAddress(spec.ClusterIP.Address)
		if !ok {
			return Endpoint{}, false
		}
		return Endpoint{Host: host, Port: port}, true
	case spawneryv1alpha1.ExposeLoadBalancer:
		host, _, ok := splitAddress(group.Status.VoiceAddress)
		if !ok || host == "" {
			return Endpoint{}, false
		}
		return Endpoint{Host: host, Port: port}, true
	}
	return Endpoint{}, false
}

// KeepsClientIP is whether the game connection and the voice packets both reach
// a proxy with the client's own source address. NodePort is Local by
// construction, and ClusterIP stands behind something the operator knows
// nothing about.
func KeepsClientIP(group *spawneryv1alpha1.ProxyGroup) bool {
	voiceSpec, _ := group.VoiceExpose()
	return exposeKeepsClientIP(group.Spec.Expose) && exposeKeepsClientIP(voiceSpec)
}

func exposeKeepsClientIP(spec spawneryv1alpha1.ExposeSpec) bool {
	switch spec.Type {
	case spawneryv1alpha1.ExposeNodePort, spawneryv1alpha1.ExposeHostPort:
		return true
	case spawneryv1alpha1.ExposeLoadBalancer:
		return spec.LoadBalancer == nil ||
			spec.LoadBalancer.ExternalTrafficPolicy != corev1.ServiceExternalTrafficPolicyCluster
	}
	return false
}

// splitAddress reads "host" or "host:port"; a bare host means 25565.
func splitAddress(address string) (string, int32, bool) {
	if address == "" {
		return "", 0, false
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return address, RelayPort, true
	}
	port, err := strconv.ParseInt(portText, 10, 32)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, false
	}
	return host, int32(port), true
}
