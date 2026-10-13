# Answers follow the pod: plan

Spec: `docs/superpowers/specs/2026-10-14-answers-follow-the-pod-design.md`.

Each task is test first: write the test, watch it fail for the stated reason,
then the code.

## Task 1: a mailbox per pod in `sessions`

- Test (`streams_test.go`): after a superseding `enter`, the answer put into
  the first generation's mailbox comes out of the second's.
- Test: after a non-superseding `enter` (the first stream left), the old
  mailbox's content is gone.
- Test: `leave` by a stale generation keeps the mailbox; by the current one
  drops it.
- Code: `enter` returns the pod's mailbox (a buffered
  `chan *agentpb.CloudResponse`, capacity 64), kept on supersede and
  replaced otherwise; `leave` deletes it with the generation.

## Task 2: session loops answer through the mailbox

- Test (envtest, `worldhistory` or `server_envtest`): a restore-point listing
  held open by the writer, the agent opens a replacement stream, the writer
  releases; the answer arrives on the replacement.
- Code: `handle` and `handleProxy` push answers into the mailbox instead of
  returning them or writing `late`; both loops drain the mailbox; an answer
  whose send fails goes back into the mailbox (non-blocking) before the
  handler returns. A full mailbox drops and counts
  `spawnery_agent_answers_dropped_total`; `make manifests` regenerates
  `docs/reference/metrics-and-alerts.md`.

## Task 3: the agent keeps requests across a new stream

- Test (`CloudConnectorTest`): a request started, then `onStreamChanged()`,
  then the answer for its id: the stage completes with the answer.
- Test (`RequestsTest`): two `Requests` instances do not start at the same id.
- Code: drop `requests.failAll` from `onStreamChanged()`; start the counter at
  a random positive value; update the class comment of `Requests`.

## Task 4: docs and checks

- `docs/explanation` (where the agent channel is described): a renewal no
  longer fails requests.
- `make test`, `make lint`, `make agent`.
