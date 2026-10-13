# Answers follow the pod across a renewal

**Status:** design, decided 2026-10-14
**Date:** 2026-10-14

## 1. What goes wrong today

A plugin request travels on the stream that is current when the plugin asks,
and its answer comes back on that same stream. Two things end a stream while a
request is in flight:

- **A renewal.** The agent opens a replacement before the operator's deadline.
  The operator supersedes the old stream the moment the new one enters
  (`sessions.enter`), and the old handler returns. An answer it was about to
  send is lost; an answer computed off the loop (a restore, a restore-point
  listing) is dropped on `ctx.Done()`.
- **A broken stream.** The agent reconnects with backoff.

The agent does not wait to find out. `SessionLoop.connect()` calls
`CloudConnector.onStreamChanged()` as soon as it installs a new stream, before
the operator has answered it, and that fails every request in flight with "the
session was renewed while this request was in flight". During an outage every
reconnect attempt does it again, so a plugin that retries quickly loses each
retry to the next attempt.

Counted on one network, 2026-10-04 to 2026-10-13: one announcement lost to an
ordinary renewal, and during an outage of about seven minutes on 2026-10-08
three starts (each after three attempts), a stop and a delete.

## 2. Decision

**The operator answers the pod, not the stream.** An answer to a request is
delivered on whatever stream is current for the pod that asked, as long as the
agent process that asked is provably the one still listening.

**The agent keeps a request open until its own deadline.** A new stream no
longer fails anything. A request the operator received is answered on the new
stream; one lost on the wire times out at its deadline (10 s, longer for the
verbs that name one), which is what a request lost on a live stream does today.

Nothing is resent. Only the caller knows whether a verb is safe to repeat
(`Requests`' existing rule), and an operator that executed a request before the
stream ended would execute it twice.

## 3. Operator

`sessions` keeps one answer mailbox per pod beside the cancel function and the
generation:

- `enter` with `superseded == true` keeps the pod's mailbox: the displaced
  stream was still live, so the agent process never went away
  (`Agents.Supersede` relies on the same fact).
- `enter` with `superseded == false` makes a fresh mailbox. A stream that ended
  without being displaced may belong to a process that died; its request ids
  restart, and an old answer must never complete a new request.
- `leave` by the current generation drops the mailbox with the rest.

Every `CloudResponse` goes through the mailbox, synchronous answers included,
and each session loop drains its pod's mailbox where it drains `late` today.
An answer the loop took but could not send goes back into the mailbox before
the handler returns, so the successor sends it.

The mailbox is bounded (64 answers). A full mailbox drops the answer and
counts it in `spawnery_agent_answers_dropped_total`; the agent's deadline
covers the request. With the request rate limit, a pod cannot have more
than its burst plus a few seconds' refill outstanding, so 64 is not reached
by a pod that keeps to its budget.

## 4. Agent

- `CloudConnector.onStreamChanged()` no longer calls `requests.failAll`. It
  still restates the announcement and the door.
- `Requests` starts its id counter at a random value, so two processes in one
  pod (a restarted container) cannot share ids even if an answer outlived the
  rule in §3.
- `failAll` stays for `stop()`: a plugin being disabled gets its answer now.

## 5. Compatibility

- New agent, old operator: an answer lost to a renewal now times out at the
  request's deadline instead of failing at once. Nothing else changes.
- Old agent, new operator: the agent still fails its requests on a new stream;
  an answer that arrives later finds no id and is dropped, as a late answer is
  today.

This is a change in behaviour, so the release is a minor step.

## 6. Tests

- `sessions`: a superseding enter keeps the mailbox, a fresh enter replaces
  it, leave by a stale generation keeps it.
- An envtest over the wire: a restore in flight when the agent renews is
  answered on the new stream.
- Agent: a request in flight across `onStreamChanged()` completes from an
  answer that arrives afterwards; one that gets no answer times out.
