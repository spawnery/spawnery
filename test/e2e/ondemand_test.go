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
	// onDemandGate keeps this out of hack/e2e.sh's run. That run's manifest
	// names images that never resolve, by its own decision, so nothing there
	// reaches Ready and no world is ever written; this scenario needs a real
	// game process and hack/e2e-ondemand.sh is the run that loads one.
	onDemandGate = "SPAWNERY_E2E_ONDEMAND"

	onDemandManifest   = "test/e2e/manifests/ondemand.yaml"
	onDemandNamespace  = "minecraft"
	onDemandGroup      = "private-servers"
	onDemandProxyGroup = "gateway"
	onDemandKey        = "c0ffee"

	// markerPath is under /data/world rather than /data. The claim is mounted
	// at /data and the world is a directory inside it, so a marker written at
	// the mount point would survive a claim that the server never put a world
	// on -- which is a weaker statement than the one this test makes.
	markerPath = "/data/world/spawnery-e2e-marker"
)

// TestAPrivateServersWorldOutlivesItsServer is what the feature is for.
//
// Every other test of the on-demand type -- the envtest suites of tasks 1 to 9
// -- reads objects back: a Server was created, a claim carries no owner
// reference, a second start is answered rather than refused. None of them can
// say whether a player who leaves and comes back finds their world, because
// none of them runs a server that has a world. This one asks for a private
// server over a real agent session, waits for the real game process behind it,
// writes a file into the world it generated, stops it, and looks for that file
// in the pod of the next start.
//
// The session is a proxy's, not a server's: the plugin that starts and stops
// private servers runs on a proxy, and members reach the proxies' network
// picture only. It is opened with a token minted for the proxy ServiceAccount
// and bound to a real proxy pod, which is what the plugin's own projected
// token is -- so what the operator authenticates here is what it would
// authenticate in production. The proxy pod does not have to be running for
// that: grpcauth checks the pod's UID and its managed-by and role labels.
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

	proxy := aProxyPodOf(t, onDemandProxyGroup)

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

	waitReady(t, server, "the first start")

	// Read before anything writes: a claim that appears only after the marker
	// is written would still pass the survival check below while meaning
	// something else entirely.
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: onDemandNamespace, Name: claim},
		&corev1.PersistentVolumeClaim{}); err != nil {
		t.Fatalf("get claim %s: %v. The world of a private server lives on this claim, and "+
			"without it there is nothing for the second start to find", claim, err)
	}

	// Unique per run, so a cluster kept with E2E_KEEP=1 and re-run cannot pass
	// on the marker its previous run left behind.
	marker := fmt.Sprintf("spawnery-e2e %d", time.Now().UnixNano())
	writeMarker(t, server, marker)

	stopServer(t, proxy, server)

	// The Server object going is what a plugin sees; the pod going is what
	// makes the read at the end mean anything, because a marker read out of
	// the pod that wrote it proves nothing about storage.
	//
	// The second of these two waits is where that property lives, and it is
	// the only place it can: once this pod has been observed NotFound, any pod
	// carrying this name afterwards is necessarily a later one. Nothing below
	// compares pod identities, because after this wait there is no same-pod
	// outcome left for such a comparison to rule out.
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

	// eventuallyStable rather than one read, for the reason
	// aPersistentGroupsClaimOutlivesItsServer gives: the Server going NotFound
	// says the API server has forgotten it, not that the garbage collector has
	// finished reacting. A single read cannot tell "nothing was reaped" from
	// "nothing has run yet"; holding the claim for a window can.
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

	waitReady(t, server, "the second start")

	got := readMarker(t, server)
	if got != marker {
		t.Fatalf("the world of %s came back with %q, want %q. This is the promise the on-demand "+
			"type exists for: the server is gone and made again, and the player's world is the "+
			"one they left", server, got, marker)
	}
}

// waitReady waits for the member to be playable, and says which start it is
// waiting on because this test waits twice and the two failures mean different
// things: the first is a private server that never came up at all, the second
// is one that cannot come up on a world it already has.
//
// Ready and not merely a running pod: it is the phase at which both halves of
// the gate are in -- the readiness probe's server-list ping answered, and the
// agent inside the server reported ready on its stream -- and only then is
// there a generated world to write into.
func waitReady(t *testing.T, server, which string) {
	t.Helper()
	eventually(t, 6*time.Minute, "the private server to reach Ready on "+which, func() (bool, string) {
		var srv spawneryv1alpha1.Server
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: onDemandNamespace, Name: server}, &srv); err != nil {
			return false, err.Error()
		}
		if srv.Status.Phase == string(phase.Ready) {
			return true, ""
		}
		return false, fmt.Sprintf("phase %s; %s", srv.Status.Phase, podTrouble(t, server))
	})
}

// podTrouble is what a wait that gave up adds about the pod. A server that
// does not reach Ready has almost always failed in its container, and the
// phase alone says nothing about that.
func podTrouble(t *testing.T, name string) string {
	t.Helper()
	var pod corev1.Pod
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: onDemandNamespace, Name: name}, &pod); err != nil {
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

// aProxyPodOf waits for a pod of the proxy group and returns it. The pod is
// only ever a name and a UID here: it is what a token can be bound to, and
// nothing in this test needs the proxy itself.
func aProxyPodOf(t *testing.T, group string) *corev1.Pod {
	t.Helper()
	var found corev1.Pod
	eventually(t, 2*time.Minute, "a pod of the "+group+" proxy group", func() (bool, string) {
		var pods corev1.PodList
		err := k8s.List(ctx, &pods,
			client.InNamespace(onDemandNamespace),
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
// Through kubectl exec rather than through client-go: the exec and
// port-forward subresources need an SPDY transport, which would pull
// moby/spdystream into go.mod and move the vendor hash every Nix build in this
// repository is pinned to -- for a test. kubectl is on the dev shell's PATH
// and already carries this run's KUBECONFIG, and test/e2e already shells out
// to a binary this way for the tutorial's join.
//
// `test -d` before the write, because the path is a guess about somebody
// else's software: Paper names its world directory from level-name, and if
// that default ever moves, a bare redirection would create a directory-less
// file somewhere and the test would pass having proven nothing.
func writeMarker(t *testing.T, server, marker string) {
	t.Helper()
	out, err := kubectlExec(t, server, fmt.Sprintf(
		"set -e; test -d /data/world; printf %%s %q > %s", marker, markerPath))
	if err != nil {
		t.Fatalf("write the marker into %s's world: %v\n%s", server, err, out)
	}
}

// readMarker reads the marker back out of whatever pod carries the member now.
func readMarker(t *testing.T, server string) string {
	t.Helper()
	out, err := kubectlExec(t, server, "cat "+markerPath)
	if err != nil {
		t.Fatalf("read the marker out of %s's world: %v\n%s\n\nThe world of a private server is "+
			"the claim, and a marker that is not there means the second start either got a "+
			"fresh claim or mounted none", server, err, out)
	}
	return strings.TrimSpace(out)
}

func kubectlExec(t *testing.T, pod, script string) (string, error) {
	t.Helper()
	cmd := exec.Command("kubectl", "-n", onDemandNamespace, "exec", pod, "--", "sh", "-c", script)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// proxySession is one authenticated agent stream.
//
// One per request, rather than one held open across the test, and the reason
// is the operator's own rule: it closes every stream at
// --agent-session-deadline, ten minutes by default, having told the agent to
// renew at eight. A real agent renews make-before-break; this test waits for a
// world to be generated twice and would outlive a single stream, so holding
// one would mean implementing an agent's renewal loop to prove something about
// storage. A fresh stream per request costs a second and re-walks the whole
// authentication path each time.
type proxySession struct {
	stream grpc.BidiStreamingClient[agentpb.ProxyMessage, agentpb.OperatorToProxy]
	close  func()
	nextID uint64
}

// openProxySession dials the operator's agent port as pod.
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

	// The opening message is the acknowledgement: the operator sends nothing
	// before it has authenticated the token, so a stream that never produces
	// one has been refused -- and the refusal arrives here rather than as a
	// confusing failure of the first request.
	if _, err := s.recv(t, 30*time.Second); err != nil {
		s.close()
		t.Fatalf("the operator sent nothing on the session of %s: %v", pod.Name, err)
	}
	return s
}

// startServer asks for the member of group that carries key, on a session of
// its own.
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

// ask sends one request and returns the answer that carries its id.
//
// It reads past anything else the operator sends, because a live proxy session
// carries the network picture too and a registration may land between the
// request and its answer.
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

// recv bounds one Recv. A stream the operator has accepted and then left
// silent would otherwise hang until the package's own timeout, which reports
// the whole run rather than the request that stalled.
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
// Bound, because the operator takes the identity from the token alone -- the
// pod name and UID in it are what it looks the pod up by, and an unbound token
// carries neither.
func proxyToken(t *testing.T, pod *corev1.Pod) string {
	t.Helper()
	tr, err := clientset.CoreV1().ServiceAccounts(onDemandNamespace).CreateToken(ctx,
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
// The name is read out of the certificate rather than composed from the
// Service and namespace this file knows, because the dial below goes to a
// forwarded 127.0.0.1 port that no SAN can name: verification has to be told
// which name to check, and taking it from what the operator serves means a run
// that installs the chart somewhere else still verifies instead of quietly
// skipping the check.
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

// forwardAgentPort puts the operator's agent port on a local one.
//
// Through kubectl for the reason writeMarker gives. The local port is left to
// kubectl and read back out of its first line rather than chosen here: a fixed
// port is a collision with whatever else is on the machine, and this run is
// meant to be startable twice in a row.
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

	// "Forwarding from 127.0.0.1:41235 -> 9443", and it is printed once the
	// listener is up -- which is the readiness signal, so nothing here polls
	// the port. A kubectl that fails instead closes this pipe, and the read
	// ends with that rather than hanging.
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
