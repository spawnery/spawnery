//go:build e2e

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

package e2e

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/certs"
	"github.com/spawnery/spawnery/internal/instance"
	"github.com/spawnery/spawnery/internal/phase"
	"github.com/spawnery/spawnery/internal/podspec"
)

const (
	// onDemandGate keeps this out of hack/e2e.sh's run, whose images never
	// resolve; hack/e2e-ondemand.sh loads a real game image.
	onDemandGate = "SPAWNERY_E2E_ONDEMAND"

	onDemandManifest   = "test/e2e/manifests/ondemand.yaml"
	onDemandNamespace  = "minecraft"
	onDemandGroup      = "private-servers"
	onDemandProxyGroup = "gateway"
	onDemandKey        = "c0ffee"

	// markerPath is inside the world, not at the claim's mount point.
	markerPath = "/data/world/spawnery-e2e-marker"
)

// TestAPrivateServersWorldOutlivesItsServer asks for a private server over a
// real agent session, writes a file into the world it generated, stops it, and
// looks for that file in the pod of the next start.
//
// The session is a proxy's, with a token bound to a real proxy pod, as in
// production. The pod need not be running: grpcauth checks its UID and
// labels.
func TestAPrivateServersWorldOutlivesItsServer(t *testing.T) {
	if os.Getenv(onDemandGate) != "1" {
		t.Skipf("set %s=1 to run the on-demand path; hack/e2e-ondemand.sh does, "+
			"hack/e2e.sh does not -- its images never resolve and this needs a real one",
			onDemandGate)
	}

	applyManifest(t, onDemandManifest)

	server, err := instance.Name(onDemandGroup, onDemandKey)
	if err != nil {
		t.Fatalf("compose the member name: %v", err)
	}
	claim := podspec.DataClaimName(server)

	proxy := aProxyPodOf(t, onDemandNamespace, onDemandProxyGroup)

	first := startServer(t, proxy, onDemandGroup, onDemandKey)
	if first.GetServer() != server {
		t.Fatalf("the operator started %q, want %q: the name a plugin gets back is the one it "+
			"routes the player to", first.GetServer(), server)
	}
	if first.GetAlreadyRunning() {
		t.Fatalf("the first start of %s answered already_running: nothing had asked for this key "+
			"before, so a plugin would skip the wait for a server that is in fact cold -- and "+
			"everything below would be measuring a member this test did not start", server)
	}

	waitReady(t, onDemandNamespace, server, "the first start")

	if err := k8s.Get(ctx, client.ObjectKey{Namespace: onDemandNamespace, Name: claim},
		&corev1.PersistentVolumeClaim{}); err != nil {
		t.Fatalf("get claim %s: %v. The world of a private server lives on this claim, and "+
			"without it there is nothing for the second start to find", claim, err)
	}

	// Unique per run, for clusters kept with E2E_KEEP=1.
	marker := fmt.Sprintf("spawnery-e2e %d", time.Now().UnixNano())
	writeMarker(t, onDemandNamespace, server, marker)

	stopServer(t, proxy, server)

	// Once this pod has been seen NotFound, any pod of this name is a later
	// one, so the read at the end cannot come from the pod that wrote.
	eventually(t, 5*time.Minute, "the stopped server to be gone", func() (bool, string) {
		var srv spawneryv1alpha1.Server
		err := k8s.Get(ctx, client.ObjectKey{Namespace: onDemandNamespace, Name: server}, &srv)
		if apierrors.IsNotFound(err) {
			return true, ""
		}
		if err != nil {
			return false, err.Error()
		}
		return false, "phase " + srv.Status.Phase
	})
	eventually(t, 2*time.Minute, "the stopped server's pod to be gone", func() (bool, string) {
		var pod corev1.Pod
		err := k8s.Get(ctx, client.ObjectKey{Namespace: onDemandNamespace, Name: server}, &pod)
		if apierrors.IsNotFound(err) {
			return true, ""
		}
		if err != nil {
			return false, err.Error()
		}
		return false, "phase " + string(pod.Status.Phase)
	})

	// Held for a window: the garbage collector may not have reacted yet.
	eventuallyStable(t, time.Minute, 15*time.Second,
		fmt.Sprintf("claim %s to outlive the server it was mounted on", claim),
		func() (bool, string) {
			err := k8s.Get(ctx, client.ObjectKey{Namespace: onDemandNamespace, Name: claim},
				&corev1.PersistentVolumeClaim{})
			if err != nil {
				return false, err.Error()
			}
			return true, ""
		})

	second := startServer(t, proxy, onDemandGroup, onDemandKey)
	if second.GetServer() != server {
		t.Fatalf("the second start of key %q made %q, want the same %q: a player who comes back "+
			"under a new name is a player who comes back to an empty world",
			onDemandKey, second.GetServer(), server)
	}
	if second.GetAlreadyRunning() {
		t.Fatalf("the second start answered already_running, yet the first server and its pod " +
			"were both observed gone above")
	}

	waitReady(t, onDemandNamespace, server, "the second start")

	got := readMarker(t, onDemandNamespace, server)
	if got != marker {
		t.Fatalf("the world of %s came back with %q, want %q. This is the promise the on-demand "+
			"type exists for: the server is gone and made again, and the player's world is the "+
			"one they left", server, got, marker)
	}
}

// waitReady waits for phase Ready, when a generated world exists; which says
// which of the two starts timed out.
func waitReady(t *testing.T, namespace, server, which string) {
	t.Helper()
	eventually(t, 6*time.Minute, "the private server to reach Ready on "+which, func() (bool, string) {
		var srv spawneryv1alpha1.Server
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: namespace, Name: server}, &srv); err != nil {
			return false, err.Error()
		}
		if srv.Status.Phase == string(phase.Ready) {
			return true, ""
		}
		return false, fmt.Sprintf("phase %s; %s", srv.Status.Phase, podTrouble(t, namespace, server))
	})
}

// podTrouble is what a wait that gave up adds about the pod's containers.
func podTrouble(t *testing.T, namespace, name string) string {
	t.Helper()
	var pod corev1.Pod
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &pod); err != nil {
		return "pod: " + err.Error()
	}
	var states []string
	for _, c := range pod.Status.ContainerStatuses {
		switch {
		case c.State.Waiting != nil:
			states = append(states, fmt.Sprintf("%s waiting: %s %s", c.Name, c.State.Waiting.Reason, c.State.Waiting.Message))
		case c.State.Terminated != nil:
			states = append(states, fmt.Sprintf("%s terminated: %s", c.Name, c.State.Terminated.Reason))
		default:
			states = append(states, fmt.Sprintf("%s running, ready=%t, restarts=%d", c.Name, c.Ready, c.RestartCount))
		}
	}
	return fmt.Sprintf("pod %s: %s", pod.Status.Phase, strings.Join(states, "; "))
}

// aProxyPodOf waits for a pod of the proxy group to bind a token to.
func aProxyPodOf(t *testing.T, namespace, group string) *corev1.Pod {
	t.Helper()
	var found corev1.Pod
	eventually(t, 2*time.Minute, "a pod of the "+group+" proxy group", func() (bool, string) {
		var pods corev1.PodList
		err := k8s.List(ctx, &pods,
			client.InNamespace(namespace),
			client.MatchingLabels{
				podspec.LabelGroup: group,
				podspec.LabelRole:  podspec.RoleProxy,
			})
		if err != nil {
			return false, err.Error()
		}
		if len(pods.Items) == 0 {
			return false, "no pods yet"
		}
		found = pods.Items[0]
		return true, ""
	})
	return &found
}

// writeMarker puts a file into the world of a running private server.
//
// kubectl exec rather than client-go, whose SPDY transport would add
// moby/spdystream to go.mod for a test. `test -d` first, because Paper names
// the world directory from level-name.
func writeMarker(t *testing.T, namespace, server, marker string) {
	t.Helper()
	out, err := kubectlExec(t, namespace, server, fmt.Sprintf(
		"set -e; test -d /data/world; printf %%s %q > %s", marker, markerPath))
	if err != nil {
		t.Fatalf("write the marker into %s's world: %v\n%s", server, err, out)
	}
}

func readMarker(t *testing.T, namespace, server string) string {
	t.Helper()
	out, err := kubectlExec(t, namespace, server, "cat "+markerPath)
	if err != nil {
		t.Fatalf("read the marker out of %s's world: %v\n%s\n\nThe world of a private server is "+
			"the claim, and a marker that is not there means the second start either got a "+
			"fresh claim or mounted none", server, err, out)
	}
	return strings.TrimSpace(out)
}

func kubectlExec(t *testing.T, namespace, pod, script string) (string, error) {
	t.Helper()
	cmd := exec.Command("kubectl", "-n", namespace, "exec", pod, "--", "sh", "-c", script)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// proxySession is one authenticated agent stream, opened per request: the
// test outlives --agent-session-deadline.
type proxySession struct {
	stream grpc.BidiStreamingClient[agentpb.ProxyMessage, agentpb.OperatorToProxy]
	close  func()
	nextID uint64
}

func openProxySession(t *testing.T, pod *corev1.Pod) *proxySession {
	t.Helper()

	addr, stopForward := forwardAgentPort(t)
	ca, serverName := operatorServingIdentity(t)

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		stopForward()
		t.Fatalf("the CA in secret %s/%s is unusable", operatorNamespace, certs.SecretName)
	}
	creds := credentials.NewTLS(&tls.Config{
		RootCAs:    pool,
		ServerName: serverName,
		MinVersion: tls.VersionTLS13,
	})
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		stopForward()
		t.Fatalf("dial the operator at %s: %v", addr, err)
	}

	streamCtx := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+proxyToken(t, pod))
	stream, err := agentpb.NewAgentServiceClient(conn).ProxySession(streamCtx)
	if err != nil {
		_ = conn.Close()
		stopForward()
		t.Fatalf("open a ProxySession as %s: %v", pod.Name, err)
	}

	s := &proxySession{
		stream: stream,
		close: func() {
			_ = conn.Close()
			stopForward()
		},
	}

	// The operator sends nothing before it has authenticated the token.
	if _, err := s.recv(t, 30*time.Second); err != nil {
		s.close()
		t.Fatalf("the operator sent nothing on the session of %s: %v", pod.Name, err)
	}
	return s
}

func startServer(t *testing.T, proxy *corev1.Pod, group, key string) *agentpb.StartServerResult {
	t.Helper()
	s := openProxySession(t, proxy)
	defer s.close()
	resp := s.ask(t, &agentpb.CloudRequest{
		Request: &agentpb.CloudRequest_StartServer{
			StartServer: &agentpb.StartServerRequest{Group: group, Key: key},
		},
	})
	result := resp.GetStartServer()
	if result == nil {
		t.Fatalf("asking for key %q of group %q was answered %v, not with a server",
			key, group, resp.GetError())
	}
	return result
}

func stopServer(t *testing.T, proxy *corev1.Pod, server string) {
	t.Helper()
	s := openProxySession(t, proxy)
	defer s.close()
	resp := s.ask(t, &agentpb.CloudRequest{
		Request: &agentpb.CloudRequest_StopServer{
			StopServer: &agentpb.StopServerRequest{Server: server},
		},
	})
	if resp.GetStopServer() == nil {
		t.Fatalf("stopping %s was answered %v, not with a stop", server, resp.GetError())
	}
}

func deleteServer(t *testing.T, proxy *corev1.Pod, group, key string) *agentpb.DeleteServerResult {
	t.Helper()
	s := openProxySession(t, proxy)
	defer s.close()
	resp := s.ask(t, &agentpb.CloudRequest{
		Request: &agentpb.CloudRequest_DeleteServer{
			DeleteServer: &agentpb.DeleteServerRequest{Group: group, Key: key},
		},
	})
	result := resp.GetDeleteServer()
	if result == nil {
		t.Fatalf("deleting key %q of group %q was answered %v, not with a deletion",
			key, group, resp.GetError())
	}
	return result
}

// ask sends one request and returns the answer that carries its id, reading
// past whatever else the operator sends.
func (s *proxySession) ask(t *testing.T, req *agentpb.CloudRequest) *agentpb.CloudResponse {
	t.Helper()
	s.nextID++
	req.Id = s.nextID
	if err := s.stream.Send(&agentpb.ProxyMessage{
		Message: &agentpb.ProxyMessage_CloudRequest{CloudRequest: req},
	}); err != nil {
		t.Fatalf("send request %d: %v", req.GetId(), err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		msg, err := s.recv(t, time.Until(deadline))
		if err != nil {
			t.Fatalf("waiting for the answer to request %d: %v", req.GetId(), err)
		}
		resp := msg.GetCloudResponse()
		if resp == nil {
			continue
		}
		if resp.GetId() != req.GetId() {
			t.Fatalf("the operator answered id %d, want the %d it was asked with",
				resp.GetId(), req.GetId())
		}
		return resp
	}
}

// recv bounds one Recv, so a silent stream fails the request that stalled.
func (s *proxySession) recv(t *testing.T, within time.Duration) (*agentpb.OperatorToProxy, error) {
	t.Helper()
	type received struct {
		msg *agentpb.OperatorToProxy
		err error
	}
	ch := make(chan received, 1)
	go func() {
		msg, err := s.stream.Recv()
		ch <- received{msg, err}
	}()
	select {
	case r := <-ch:
		return r.msg, r.err
	case <-time.After(within):
		return nil, fmt.Errorf("nothing arrived within %s", within)
	}
}

// proxyToken mints what the proxy's own projected volume would hold: a token
// for the proxy ServiceAccount, in the agent audience, bound to that pod.
func proxyToken(t *testing.T, pod *corev1.Pod) string {
	t.Helper()
	tr, err := clientset.CoreV1().ServiceAccounts(pod.Namespace).CreateToken(ctx,
		podspec.ProxyServiceAccountName,
		&authnv1.TokenRequest{Spec: authnv1.TokenRequestSpec{
			Audiences:         []string{podspec.AgentTokenAudience},
			ExpirationSeconds: ptr.To(int64(3600)),
			BoundObjectRef: &authnv1.BoundObjectReference{
				Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID,
			},
		}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("mint a proxy token bound to %s: %v", pod.Name, err)
	}
	return tr.Status.Token
}

// operatorServingIdentity returns the CA an agent verifies the operator with,
// and a name that certificate actually carries.
//
// The dial goes to a forwarded 127.0.0.1 port no SAN names, so verification
// is told a name read from the certificate itself.
func operatorServingIdentity(t *testing.T) ([]byte, string) {
	t.Helper()
	var secret corev1.Secret
	key := types.NamespacedName{Namespace: operatorNamespace, Name: certs.SecretName}
	if err := k8s.Get(ctx, key, &secret); err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	block, _ := pem.Decode(secret.Data["tls.crt"])
	if block == nil {
		t.Fatalf("the serving certificate in %s is not PEM", key)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse the serving certificate in %s: %v", key, err)
	}
	if len(cert.DNSNames) == 0 {
		t.Fatalf("the serving certificate in %s carries no DNS name, so nothing an agent "+
			"dials could ever verify against it", key)
	}
	return secret.Data["ca.crt"], cert.DNSNames[0]
}

// forwardAgentPort puts the operator's agent port on a local one, through
// kubectl for the reason writeMarker gives. kubectl picks the port.
func forwardAgentPort(t *testing.T) (string, func()) {
	t.Helper()
	pod := operatorPod(t, operatorNamespace)

	cmd := exec.Command("kubectl", "-n", operatorNamespace, "port-forward",
		"pod/"+pod.Name, ":"+strconv.Itoa(int(podspec.AgentPort)))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("pipe kubectl port-forward: %v", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start kubectl port-forward: %v", err)
	}
	stop := func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}

	// "Forwarding from 127.0.0.1:41235 -> 9443", printed once the listener
	// is up.
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		stop()
		t.Fatalf("kubectl port-forward said nothing: %v\n%s", err, stderr.String())
	}
	fields := strings.Fields(line)
	if len(fields) < 3 || !strings.Contains(fields[2], ":") {
		stop()
		t.Fatalf("kubectl port-forward opened with %q, which carries no local address", line)
	}
	return fields[2], stop
}
