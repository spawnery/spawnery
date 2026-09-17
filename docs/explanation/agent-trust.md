# What a compromised game server can do

Assume a plugin on one Paper server has been taken over. It runs arbitrary
code in that JVM, so it has the agent's token and can send anything it likes
down the agent channel.

This page is what that buys an attacker, stated as limits rather than as
reassurance. It is about **authenticity** — who the operator believes is
speaking. The other half, how many connections a hostile pod can open and what
is still unbounded there, is in [What the NetworkPolicies buy, and what they
do not](network-boundaries.md). The shape these limits come from is in
[Why the game pods never read the Kubernetes API](architecture.md).

## The one thing it cannot do

**It cannot speak as another pod.**

The operator does not read the sender's claim about who it is. It takes the
bearer token, hands it to the API server's own authenticator, and reads the
pod name and pod UID out of the claims that come back. The message's opinion
of its own identity is not consulted at any point.

This is not defence in depth; it is the load-bearing wall. A compromised
server that could name itself would report zero players for a busy server and
watch the operator remove it — and "a server with players on it is never
simply deleted" is the promise the whole system exists to keep.

So the damage is confined to the compromised pod's own record. It can claim to
be empty when it is full, and lose its own server. It cannot do that to
anybody else's.

## What it also cannot do

**It cannot act as a proxy.** The role is checked against the pod's own
`spawnery.cloud/role` label, so a backend pod opening a session and asking to
be treated as a proxy is refused. That matters because proxies receive the
routing picture and drain orders; a backend that could pose as one would be
reading and acting on a different conversation.

**It cannot be a pod this operator did not create.** The lookup insists on the
`managed-by` label as well as the role, so a hand-built pod in the namespace
cannot open a session even holding a valid ServiceAccount token. The same two
labels decide what the orphan sweep considers "one of ours" — deliberately,
because a pod that could authenticate here but be swept as foreign there would
be a disagreement with consequences.

**It cannot reach the Kubernetes API.** There is no client in the agent and
nothing for the plugin to borrow. The token opens this stream and nothing
else.

**It cannot reach another network.** The credential names a pod in a
namespace, and a namespace is one `Network`. There is no wider request
available to make.

## What it can do

**It can lie about itself.** Player count, readiness, the description it
announces. The cost is its own server: an agent reporting zero players on a
full server will have that server scaled down under the people on it.

There is no defence against this and there is not meant to be. The alternative
is for the operator to count players some other way, and there isn't one — the
process running the game is the only thing that knows. What the design does
instead is make the lie's blast radius exactly the liar.

**It can stop talking.** A silent agent loses readiness and its server stops
taking joins. That is a denial of service against itself.

**It can consume operator resources.** How much, and what bounds it, is the
availability question — see [network-boundaries.md](network-boundaries.md#how-many-agents-may-reach-the-operator).

## Revocation is not instant, and the delay is chosen

An accepted token review is cached for **60 seconds**; a refusal for **10**.

The asymmetry is the interesting part. A refusal is cheap to re-check and you
want a pod that has just become legitimate — one whose label was fixed, or
whose token was just projected — to be admitted quickly. An acceptance is
expensive to re-check, because every session would otherwise put a
`TokenReview` on the API server on every message.

The consequence a reader should hold on to: **deleting a compromised pod does
not close its session within the same second.** For up to a minute the
operator may still accept a message on an already-authenticated stream.
Identity is keyed on the pod UID, so a replacement pod with the same name is a
different principal and cannot inherit the old one's session — but the old
session's last minute is real.

If a minute matters for your threat model, the pod's removal is not the
control you want; the network boundary is.

## Why the CR status is not the input

`status.onlinePlayers` on a `ServerGroup` is written for people reading the
cluster. The control loop reads the in-memory registry the agent channel
feeds, not the status it wrote a moment ago.

That is partly about echoes — a reconciler scaling on what it last wrote is
reacting to itself — and partly about trust. The status is a projection for
observers. Anything with authority over a server's life comes from a stream
whose identity the API server vouched for.

## What this page does not claim

It does not claim the JVM is a boundary. A plugin running beside the agent is
in the same process as the agent's token, and if it is hostile it has that
token. Every limit above is written on the assumption that it does.

It does not claim a compromised pod is harmless. It claims the harm is bounded
to that pod's own record, and names the two places — the lie about itself, and
the resources it consumes — where the bound is the only thing standing.
