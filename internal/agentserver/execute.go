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

package agentserver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spawneryv1alpha1 "github.com/spawnery/spawnery/api/v1alpha1"
	"github.com/spawnery/spawnery/internal/agent"
	"github.com/spawnery/spawnery/internal/agentpb"
	"github.com/spawnery/spawnery/internal/grpcauth"
	"github.com/spawnery/spawnery/internal/phase"
)

const (
	// ExecuteWait sits inside the ten seconds after which an agent gives up on
	// a request, so the proxy hears about the servers that did answer.
	ExecuteWait             = 8 * time.Second
	ExecuteMaxCommandLength = 256
	ExecuteMaxLines         = 20
	ExecuteMaxLineLength    = 256
)

var errNoTarget = errors.New("no server or group by that name")

type executionKey struct {
	pod string
	id  uint64
}

// executions pairs an outcome with the command it answers. The pod UID is
// part of the key, so a server can only answer for itself.
type executions struct {
	mu      sync.Mutex
	next    uint64
	waiting map[executionKey]chan *agentpb.ExecuteOutcome
}

func (e *executions) open(pod string) (uint64, <-chan *agentpb.ExecuteOutcome, func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.waiting == nil {
		e.waiting = map[executionKey]chan *agentpb.ExecuteOutcome{}
	}
	e.next++
	key := executionKey{pod: pod, id: e.next}
	answer := make(chan *agentpb.ExecuteOutcome, 1)
	e.waiting[key] = answer
	return key.id, answer, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		delete(e.waiting, key)
	}
}

// deliver drops an outcome nobody waits for: a late one, or one for another
// pod's command.
func (e *executions) deliver(pod string, outcome *agentpb.ExecuteOutcome) {
	e.mu.Lock()
	defer e.mu.Unlock()
	key := executionKey{pod: pod, id: outcome.GetId()}
	if answer, ok := e.waiting[key]; ok {
		delete(e.waiting, key)
		answer <- outcome
	}
}

// answerExecute is proxy-only, behind the network's switch: every other
// request on the channel may come from any agent, and a console command on
// every server must not.
func (s *Server) answerExecute(
	ctx context.Context,
	logger logr.Logger,
	id grpcauth.Identity,
	reqID uint64,
	req *agentpb.ExecuteRequest,
) *agentpb.CloudResponse {
	if id.Role != agent.RoleProxy {
		return refuse(reqID, agentpb.RequestError_REFUSED,
			"only a proxy may run a command on a server: one compromised game server must not reach the others' consoles")
	}
	command := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(req.GetCommand()), "/"))
	if command == "" {
		return refuse(reqID, agentpb.RequestError_REFUSED, "there is no command to run")
	}
	if n := len([]rune(command)); n > ExecuteMaxCommandLength {
		return refuse(reqID, agentpb.RequestError_REFUSED,
			fmt.Sprintf("that command is %d characters and the operator carries at most %d", n, ExecuteMaxCommandLength))
	}
	network, enabled, err := s.executeSwitch(ctx, id.Namespace)
	if err != nil {
		logger.V(1).Info("could not read the network for an execute request", "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE, "the operator could not read this network just now")
	}
	if !enabled {
		return refuse(reqID, agentpb.RequestError_REFUSED, "execute is not enabled on this network")
	}
	targets, single, err := s.executeTargets(ctx, id.Namespace, req.GetTarget())
	switch {
	case errors.Is(err, errNoTarget):
		return refuse(reqID, agentpb.RequestError_NOT_FOUND,
			"no server or group by that name is on this network, or the group has no Ready server")
	case err != nil:
		logger.V(1).Info("could not list servers for an execute request", "reason", err.Error())
		return refuse(reqID, agentpb.RequestError_UNAVAILABLE, "the operator could not read this network just now")
	}

	issuer := clip(req.GetIssuer(), issuerMaxLength)
	logger.Info("running a console command", "network", network, "proxy", id.PodName,
		"issuer", issuer, "target", req.GetTarget(), "command", command)

	type waiting struct {
		server string
		answer <-chan *agentpb.ExecuteOutcome
		forget func()
	}
	var waits []waiting
	for i := range targets {
		srv := &targets[i]
		execID, answer, forget := s.executions.open(srv.Status.PodUID)
		sent := srv.Status.PodUID != "" && s.opts.Servers.Send(srv.Status.PodUID, &agentpb.OperatorToServer{
			Message: &agentpb.OperatorToServer_ExecuteCommand{
				ExecuteCommand: &agentpb.ExecuteCommand{Id: execID, Command: command},
			},
		})
		if !sent {
			forget()
			continue
		}
		s.recordOn(ctx, id.Namespace, srv.Name, corev1.EventTypeNormal, "CommandExecuted", "Execute",
			fmt.Sprintf("%s ran %q from proxy %s", issuer, command, id.PodName))
		waits = append(waits, waiting{server: srv.Name, answer: answer, forget: forget})
	}
	if len(waits) == 0 {
		if single {
			return refuse(reqID, agentpb.RequestError_UNAVAILABLE,
				"that server's agent is not connected, so nothing can reach its console")
		}
		return refuse(reqID, agentpb.RequestError_NOT_FOUND, "no Ready server of that group has its agent connected")
	}

	wait := s.opts.ExecuteWait
	if wait <= 0 {
		wait = ExecuteWait
	}
	deadline, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	outcomes := make([]*agentpb.ExecuteOutcome, 0, len(waits))
	for _, w := range waits {
		select {
		case got := <-w.answer:
			outcomes = append(outcomes, bounded(w.server, got))
		case <-deadline.Done():
			w.forget()
			// select picks at random between ready cases, and an answer that
			// came in before the deadline must not be reported as missing.
			select {
			case got := <-w.answer:
				outcomes = append(outcomes, bounded(w.server, got))
			default:
				outcomes = append(outcomes, &agentpb.ExecuteOutcome{
					Server: w.server, Error: fmt.Sprintf("no answer within %s", wait),
				})
			}
		}
	}
	slices.SortFunc(outcomes, func(a, b *agentpb.ExecuteOutcome) int { return strings.Compare(a.GetServer(), b.GetServer()) })
	return &agentpb.CloudResponse{
		Id:     reqID,
		Result: &agentpb.CloudResponse_Execute{Execute: &agentpb.ExecuteResult{Outcomes: outcomes}},
	}
}

// executeSwitch reads the first Network, as netstate does: the Network
// controller refuses a second one per namespace.
func (s *Server) executeSwitch(ctx context.Context, namespace string) (string, bool, error) {
	var networks spawneryv1alpha1.NetworkList
	if err := s.opts.State.Reader.List(ctx, &networks, client.InNamespace(namespace)); err != nil {
		return "", false, err
	}
	if len(networks.Items) == 0 {
		return "", false, nil
	}
	return networks.Items[0].Name, networks.Items[0].ExecuteEnabled(), nil
}

// executeTargets is the server of that name, else the group's Ready servers.
// single reports the first case.
func (s *Server) executeTargets(ctx context.Context, namespace, target string) ([]spawneryv1alpha1.Server, bool, error) {
	var list spawneryv1alpha1.ServerList
	if err := s.opts.State.Reader.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, false, err
	}
	for i := range list.Items {
		if list.Items[i].Name == target {
			return list.Items[i : i+1], true, nil
		}
	}
	var members []spawneryv1alpha1.Server
	for _, srv := range list.Items {
		if srv.Spec.GroupRef.Name == target && phase.Phase(srv.Status.Phase) == phase.Ready {
			members = append(members, srv)
		}
	}
	if len(members) == 0 {
		return nil, false, errNoTarget
	}
	return members, false, nil
}

// bounded applies the agent's limits again: the operator does not trust a
// backend to have kept them.
func bounded(server string, got *agentpb.ExecuteOutcome) *agentpb.ExecuteOutcome {
	out := &agentpb.ExecuteOutcome{
		Server: server,
		Ok:     got.GetOk(),
		Error:  clip(got.GetError(), ExecuteMaxLineLength),
	}
	for i, line := range got.GetOutput() {
		if i == ExecuteMaxLines {
			break
		}
		out.Output = append(out.Output, clip(line, ExecuteMaxLineLength))
	}
	return out
}
