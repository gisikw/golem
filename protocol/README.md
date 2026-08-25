# Golem protocol

`protocol.go` is the canonical JSON contract shared by golemd's service, supervisor, harness adapters, and clients. Go types serve as the schema; harness-specific evidence is confined to opaque `detail` JSON.

## Reconciliation

The local service sends `PollResponse.assignments` downward to its in-process supervisor. The supervisor sends idempotent `ObservedEvent` batches upward through the retained HTTP seam. There is no host claim or host-filtered assignment: every accepted job belongs to this golemd. Legacy `host` fields remain as daemon identity/display metadata to avoid a destructive schema migration.

Observed-event IDs, create keys, answer keys, and settlement IDs are stable idempotency keys. An observation and its resulting job mutation commit in one SQLite transaction. Public lifecycle events receive a separate SQLite-assigned global sequence and are available as replay-then-tail SSE from `GET /v1/events?since=SEQ`; kinds are `job.created`, `job.state`, `job.progress`, and `job.settled`.

Lifecycle: `pending → assigned → starting → running ↔ blocked → cancelling`, ending in `done`, `failed`, `cancelled`, or `timeout`. Terminal transitions require a structured settlement in the same durable transaction. It records state, bounded harness verdict, optional exit status and usage, a capped relative artifact listing, and resolved-worktree status. The first settlement wins; repeated observations and later settlement attempts are harmless no-ops.

`artifacts.id` is a logical service-assigned identifier, not a path. `GET /v1/jobs/{id}/artifacts` returns `{artifacts:[{path,size,modified_at}],artifacts_truncated}` for running or settled jobs; `GET /v1/jobs/{id}/artifacts/{path...}` serves bytes with standard range support. Listings and settlement use the same 100-regular-file enumerator. Normal dispatch supplies `workspace`: either `{project,worktree}` or `{repo,ref,worktree}`. golemd resolves and stores the persistent named worktree and its path before assignment. Direct `cwd` remains a low-level escape hatch and all resolved paths are checked against local allowed roots before launch.

Dispatch contains only harness and model identity. Provider connection descriptors and credential references are not protocol fields; golemd provisions pi from operator config and its own environment.

`GET /v1/capabilities` reports the daemon identity, version, configured harness/model offerings, path-free project catalog, clone flag, and configured SSH attach port (zero when disabled). Dispatches cannot expand that operator-owned catalog. A live job may carry both a host-local `terminal` tmux endpoint and a location-independent `activation` SSH endpoint; activation is removed at settlement.
