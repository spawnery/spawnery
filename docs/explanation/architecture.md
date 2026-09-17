# Why the game pods never read the Kubernetes API

Every other design for this problem starts the same way: the game server is in
a pod, the pod has a ServiceAccount, the plugin uses the Kubernetes client to
list its siblings. It is the obvious route and Spawnery does not take it.

This page is why, and what the shape that replaces it costs. It does not tell
you how to configure anything — [Guides](../guides/expose-strategies.md) do
that — and it does not enumerate fields, which the [custom resource
reference](../reference/crds.md) generates.

## The problem with the obvious route

A Minecraft network runs untrusted code. Not maliciously untrusted, usually,
but a game server is a place where plugins from the internet execute, where an
administrator installs something on a Friday to try it out, and where a
remote-code-execution bug in a popular plugin is an ordinary event rather than
a crisis.

Give that pod a Kubernetes client and you have given every plugin on it one.
The blast radius of a compromised game server stops being the game server. It
becomes whatever its ServiceAccount can reach — and what it needs to reach, to
do the job, is the list of its siblings, which means read access across the
namespace at least.

So the question is not "how does a game server learn about the network" but
"how does it learn without holding a credential that is worth stealing".

## One stream, and identity that does not come from the message

Each agent opens a single authenticated gRPC stream to the operator, and that
stream carries both directions: player counts and readiness go up, the server
list and drain orders come down.

The pod authenticates with a bearer token, and the operator hands that token
to the API server's own authenticator rather than interpreting it. What comes
back names exactly one pod — and it is the API server, not the message, that
names it.

That last point is the whole design in one sentence. `internal/grpcauth` says
it in its own package comment: if a compromised server could name itself in
its opening message, it could report zero players for a full server and have
it deleted. The invariant this entire system exists to protect — a server with
players on it is never simply removed — would be one forged field away from
gone.

So identity is taken from the token's pod claims and the message's opinion of
who it is has no effect at all. A compromised pod can lie about its own player
count, which costs it its own server. It cannot lie about *whose* player count
it is reporting.

## What a pod ends up holding

A pod-bound ServiceAccount token that is good for one thing: opening this
stream. It grants nothing in the Kubernetes API, because the agent never calls
the Kubernetes API. There is no client, no informer, no list permission, and
nothing for a plugin to borrow.

The same boundary holds sideways. Everything an agent can ask about is scoped
to its own namespace, which is one `Network`, and that is structural rather
than a check somewhere: the credential names a pod in a namespace, so there is
no wider request to make.

## The operator is its own certificate authority

The stream is TLS, and the operator issues its own certificate rather than
asking cert-manager for one. That looks like reinvention until you see what it
buys: the operator creates the agent pods anyway, so it can pin its own CA
bundle into them at the moment it builds them. Nothing has to be installed
first, and nothing has to agree out of band.

The practical consequence is that `helm install` is the whole installation.
A cluster with no cert-manager, no issuer and no webhook still gets a mutually
authenticated channel, because both halves of the trust are things this
operator already controls.

Rotating that CA is a procedure rather than an event, and it has [its own
guide](../guides/rotating-the-ca.md).

## Two directions, two registries, one picture

The code is arranged along the same split as the stream.

What comes **up** lands in `internal/agent`: in-memory player counts and
readiness, written by the gRPC server and read by the controllers. Note what
that is not — the custom resource's `status` is for people watching, not for
the control loop. A reconciler that scaled on what it had last written to
`status` would be reading its own echo.

What goes **down** is built by `internal/proxyreg` for proxies and
`internal/serverreg` for backends, written by the controllers and sent by the
gRPC server. Both build their view through `internal/netstate`, so a proxy and
a backend see one identical picture of the network rather than two that are
meant to agree.

## Pure cores, thin controllers

The decisions live in packages that hold no client and reach no cluster:
`internal/phase` is the `Server` state machine as one pure function,
`internal/podspec` turns API objects into pod specs, `internal/render` writes
the files Paper and Velocity read.

This is not tidiness for its own sake. A rule that lives in a pure function is
testable without a control plane, and this repository leans on that heavily —
the expensive envtest packages exist for the wiring, and the rules that
actually decide whether a player gets disconnected are checked in
milliseconds. It also means the interesting questions have one answer each:
there is exactly one place that decides whether a server may be deleted.

## What this shape costs

It is not free, and the costs are the reason most systems do not do it.

**There is more machinery.** A gRPC service, a certificate authority, a token
interceptor and two registries are all things a Kubernetes client would have
given for nothing.

**The agent is a plugin, so it shares a process with untrusted code.** The
boundary is real at the Kubernetes API and softer inside the JVM: a plugin
running beside the agent is in the same address space as the agent's token.
What the design buys is that the token is worth almost nothing if taken.

**A plugin cannot do things this API has not been taught.** A Kubernetes
client would let a plugin author reach anything. Here the surface is
[what the API offers](../plugin-api/index.md), and widening it is a change to
this project rather than a permission grant. That is a feature from the
operator's side and a limitation from the plugin author's, and both readings
are correct.
