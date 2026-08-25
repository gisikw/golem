# Golem protocol

`protocol.go` is the canonical JSON contract shared by golemd's service, supervisor, harness adapters, and clients. Go types serve as the schema; harness-specific evidence is confined to opaque `detail` JSON.

## Reconciliation

The local service sends `PollResponse.assignments` downward to its in-process supervisor. The supervisor sends idempotent `ObservedEvent` batches upward through the retained HTTP seam. There is no host claim or host-filtered assignment: every accepted job belongs to this golemd. Legacy `host` fields remain as daemon identity/display metadata to avoid a destructive schema migration.

Event IDs, create keys, answer keys, and settlement IDs are stable idempotency keys. An event and its resulting job mutation commit in one SQLite transaction.

Lifecycle: `pending → assigned → starting → running ↔ blocked → cancelling`, ending in `done`, `failed`, `cancelled`, or `timeout`. Terminal transitions require a settlement in the same durable transaction. Repeated nonterminal observations and duplicate deliveries are harmless.

`artifacts.id` is a logical service-assigned identifier, not a path. The supervisor resolves it beneath the daemon's local artifact root. Direct `cwd` remains request data and is checked against local allowed roots before launch.

`GET /v1/capabilities` reports the daemon identity, version, configured harness/model offerings, path-free project catalog, clone flag, and future attach port. Dispatches cannot expand that operator-owned catalog.
