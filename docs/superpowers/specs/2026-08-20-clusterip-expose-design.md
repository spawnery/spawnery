# The ClusterIP expose strategy

## 1. What this is

`ProxyGroup.spec.expose` offers three strategies — `LoadBalancer`, `NodePort`,
`HostPort` — and every one of them assumes the Service the operator creates is
what the outside world reaches. The RKE2 rollout put a network behind Traefik's
TCP entryPoint, where that assumption does not hold, and there was no strategy
that says so. `docs/runbook-milestone-6-rollout.md`'s scenario 4 records the
workaround it used and what the workaround cost:

> `NodePort` is the nearest workaround — it produces a Service and asks nothing
> else, and 30565 is covered by the host firewall — but the group then reports
> `status.address: <node>:30565`, an address nobody plays on.

This adds the fourth strategy. `type: ClusterIP` creates a Service with no
external address of its own and publishes the address its operator supplies.

**What it does not do**, stated first, because the boundary is the design: it
creates no routing object, verifies no address, and takes nothing off the
fronting proxy's hands. §4 says why each of those is a refusal rather than an
omission.

## 2. The API

```yaml
expose:
  type: ClusterIP
  clusterIP:
    address: "mc.paul.wtf"
```

A fourth value in the `ExposeType` enum and a sub-block beside the existing
three:

```go
// ExposeClusterIP is for a network something else publishes: an ingress
// controller, a gateway, a tunnel. The operator creates the Service that
// thing routes to, and nothing else.
ExposeClusterIP ExposeType = "ClusterIP"

// ClusterIPSpec configures the ClusterIP strategy.
type ClusterIPSpec struct {
	// Address is what a player types.
	//
	// Required, because the operator cannot learn it: it lives in an
	// IngressRouteTCP, an HTTPRoute, a tunnel's configuration or a DNS
	// record — objects under APIs this operator does not read and cannot
	// know are installed. Optional would make "empty" and "forgotten"
	// the same state, which is the gap this strategy exists to close.
	//
	// No port is required and none should usually be given: Minecraft
	// clients default to 25565, so "mc.paul.wtf" is the whole of what a
	// player types. Give "host:port" only when the entry point really is
	// on another port.
	Address string `json:"address"`
}
```

`ExposeSpec` gains `ClusterIP *ClusterIPSpec`, and the enum marker becomes
`LoadBalancer;NodePort;HostPort;ClusterIP`.

**Validation**, following the pattern the other three already have — a
required sub-block for its own type, and forbidden for every other:

```
self.type != 'ClusterIP' || has(self.clusterIP)
self.type == 'ClusterIP' || !has(self.clusterIP)
```

and two checks on the address itself: `+kubebuilder:validation:MinLength=1`
for emptiness, and one CEL rule on `ClusterIPSpec` for the rest:

```
!self.address.contains(' ') && !self.address.contains('://')
```

These catch the two mistakes anyone actually makes — a URL pasted in, and a
value copied with whitespace — at admission rather than at the first player.
Nothing more is checked: see §4.

## 3. The controller, and the trap that closes with it

Two functions switch on `Expose.Type`, and in both the `default:` arm **means
NodePort**:

- `ProxyGroupReconciler.reconcileService` — the `default:` at
  `proxygroup_controller.go:1296` sets `Type: NodePort` and reads
  `Expose.NodePort.Port`;
- `proxyAddress` — the `default:` at `proxygroup_controller.go:1739`
  publishes `hostIP:NodePort.Port`.

A fourth enum value added without touching them would be silently treated as a
NodePort: a Service nobody asked for, on a port nobody covered, advertised at
an address nobody dials. **Both switches therefore become exhaustive**, with an
explicit `case` per strategy and a `default:` that returns an error naming the
unknown type.

That is worth more than this one strategy. It is what makes the *next* one
impossible to add wrongly, and it is the same class of defect this project has
now shipped and repaired three times: a guard whose fallthrough silently means
something.

**The `ClusterIP` arm of `reconcileService`:**

- `svc.Spec.Type = corev1.ServiceTypeClusterIP`;
- the same port, name, target port and selector every other strategy uses —
  the fronting proxy dials port 25565 on this Service;
- **no** `ExternalTrafficPolicy`. The field is meaningless on a ClusterIP
  Service and the API server rejects it;
- **no** `NodePort` on the port.

**The `ClusterIP` arm of `proxyAddress`** returns `Expose.ClusterIP.Address`
unchanged.

**The ready-pod gate stays.** `proxyAddress` returns `""` unless some pod of
the group is `Ready` and carries a `HostIP`, and that applies here too, even
though the address is a static string that needs no pod to compute. The column
keeps one meaning across all four strategies: *you can connect here now*. An
empty column says nothing is ready, and never "somebody forgot to look".

**No annotations.** `LoadBalancerSpec` carries an `Annotations` map because a
cloud controller reads it. Nothing reads annotations on a ClusterIP Service
that could change where traffic goes — and on the cluster this was designed
against it would actively mislead: external-dns runs with
`--publish-internal-services`, so a hostname annotation there would create a
record pointing at a ClusterIP, reachable from nowhere outside the cluster.
`applyExposeAnnotations` is already called with `nil` for every non-LoadBalancer
type, so this needs no code — only a test that says it is deliberate.

## 4. The boundary

Three refusals, each of them a decision:

**No routing object is created.** Not an `Ingress`, not an `IngressRouteTCP`,
not an `HTTPRoute`. The operator holds no RBAC for any of them, cannot know
which controller a cluster runs, and their shapes have nothing in common. It
builds the Service the fronting thing routes to and stops. The administrator
who set up that entry point is the one who knows how it is written.

**The address is not verified.** Whether it resolves, whether anything listens,
whether it leads to this Service at all — none of it is checked. It is a sign
on a door, not a test of the door. A wrong address is visible to a person and
invisible to the operator, and the field's documentation says so rather than
implying a guarantee that is not there.

**Nothing is taken off the fronting proxy's hands.** No PROXY protocol, no TLS,
no header rewriting. If what stands in front replaces the client's address, that
is between it and Velocity — `docs/known-issues.md`'s "From the RKE2 rollout"
records what that costs and how it is configured, and none of it belongs in the
operator.

## 5. Moving between strategies

The transitions are where the sharp edges are, and each is measured rather than
reasoned about:

| from → to | what must happen |
|---|---|
| `LoadBalancer` → `ClusterIP` | The Service type changes and the cloud load balancer is released. `applyExposeAnnotations(svc, nil)` removes exactly the keys the operator set, tracked in `podspec.AnnotationExposeAnnotations`, and leaves every foreign key alone. Milestone 6c built that mechanism; this is the first strategy that exercises it in this direction. |
| `NodePort` → `ClusterIP` | The allocated node port must be gone from the Service afterwards. The API server clears it when the type changes — that is what the documentation says, and this design does not take its word for it. |
| `HostPort` → `ClusterIP` | A Service must be *created* where `deleteServiceIfOurs` previously removed one, and the pods roll, because `hostPort` is part of the rendered pod and therefore of `DesiredProxyHash`. It is the only one of the four whose change rolls pods; 6c measured that and it does not change here. |
| `ClusterIP` → any other | Symmetric, and covered by the same tests read in the other direction. |

## 6. How it is proven

**The exhaustiveness test is the important one, and it fails today.** An
`ExposeType` the code does not know must produce an error, not a NodePort
Service. Written against the current code it goes green for the wrong reason,
so the plan mutates `default:` back to meaning NodePort and requires the test to
fail; a test that survives that mutation is testing nothing.

**Against a real API server**, because CEL runs at admission and envtest is
where this project checks admission:

- `type: ClusterIP` with no `clusterIP` block — refused;
- `clusterIP` with `type: NodePort` — refused;
- `address: "tcp://mc.paul.wtf"` — refused;
- `address: "mc.paul.wtf "` — refused;
- `address: "mc.paul.wtf"` — accepted, and `status.address` reads back exactly
  that once a proxy is ready, and empty before.

**The three transitions of §5**, each asserting against the Service read back
out of the cluster rather than against the patch that was sent.

**One end-to-end scenario** in `test/e2e/manifests/e2e.yaml` and
`test/e2e/expose_test.go`, alongside the three groups already there: a
`ClusterIP` group whose Service has type `ClusterIP`, no `nodePort` on its port,
and whose `status.address` equals the configured value.

## 7. Acceptance criteria

1. `expose.type: ClusterIP` with `clusterIP.address` produces a Service of type
   `ClusterIP` with no node port and no external traffic policy.
2. `status.address` carries the configured address once a proxy pod is ready,
   and is empty before.
3. Both switches are exhaustive: an unknown `ExposeType` produces an error
   naming it, and the test proving so fails when `default:` is mutated back to
   meaning NodePort.
4. The five admission cases of §6 behave as listed against a real API server.
5. Each of the three transitions in §5 is asserted against the object read back.
6. The E2E carries a `ClusterIP` group and asserts the Service's shape and the
   published address.
7. `docs/known-issues.md`'s entry recording this gap is replaced by what the
   strategy does, and `config/samples/network.yaml` shows it as an alternative
   beside the three it already documents.
