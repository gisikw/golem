# Agent protocol

`protocol.go` is the canonical, versioned JSON contract for service, supervisor,
harness, and client. Go types intentionally serve as the schema: every field has
an explicit JSON name and harness-specific data is confined to opaque `detail`
JSON. Consumers must ignore unknown detail fields.

## Reconciliation

The service sends `PollResponse.assignments` (desired semantic state) downward.
A host sends idempotent `ObservedEvent` batches upward. Event IDs, create keys,
answer keys, and settlement IDs are stable idempotency keys. An event and its
resulting job mutation are committed in one SQLite transaction.

Lifecycle: `pending → assigned → starting → running ↔ blocked → cancelling`,
ending in `done`, `failed`, `cancelled`, or `timeout`. Failure settlements may
terminate any nonterminal state. A terminal transition is invalid without a
settlement committed in the same durable transaction. Repeated nonterminal
observations and exact duplicate event deliveries are harmless.

The global registry owns semantic state. `artifacts.id` is a logical,
service-assigned job artifact identifier—not a path. Each supervisor resolves it
beneath its configured host-local artifact root; artifact directories never
cross the wire, and caller-supplied directory fields are rejected. Requested
`cwd` remains semantic assignment data but is authorized against host-local
allowed roots before launch.

A supervisor may report process facts, but cannot overwrite immutable job
identity/request fields or another host's assignment. Harness detail is
evidence, not a second common state vocabulary. Pre-worker start attempts and
observation cursors are host-local recovery state and are intentionally absent
from this global protocol.
