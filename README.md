# Golem

Golem is a standalone delegated-worker system. One `golemd` daemon per host owns its SQLite job store, supervisor, private tmux server, harness configuration, and durable worker registry. Fleet-aware clients choose which daemon to contact; there is no server-side host scheduling.

## Features

- Durable, idempotent jobs, events, answers, and settlements
- Interactive, writable tmux sessions with direct attach hints
- Lifecycle states from `assigned` through running, blocked, and terminal outcomes
- Optional detached Git worktrees and host-local artifact retention
- Pi, minimal Claude/Codex, and dependency-free fake harness adapters
- Operator-advertised harness/model and project capabilities
- Unix-socket or loopback HTTP JSON API

TCP authentication, SSH attach, project/clone workspace resolution, and portable artifact storage are deferred. Do not expose the HTTP listener beyond loopback or a trusted tunnel.

## Requirements

Go 1.23+, plus `tmux`, `bash`, and `git` for supervised workers. Nix is optional.

## Quick start

The checked-in [`golemd.example.toml`](golemd.example.toml) is runnable as-is (its example project is `/tmp`):

```sh
state=$(mktemp -d)
go run ./cmd/golemd --config ./golemd.example.toml --state "$state"
```

In another shell, address that daemon's Unix socket:

```sh
endpoint="unix://$state/golemd.sock"
go run ./cmd/golem --service "$endpoint" capabilities
go run ./cmd/golem --service "$endpoint" \
  dispatch --harness fake --cwd /tmp 'exercise the worker lifecycle'
go run ./cmd/golem --service "$endpoint" list
go run ./cmd/golem --service "$endpoint" await JOB_ID
go run ./cmd/golem --service "$endpoint" attach-hint JOB_ID
```

With no `--service`, the CLI uses `unix://~/.local/state/golem/golemd.sock`, matching golemd's default state directory. The smoke test automates the complete flow:

```sh
./test/standalone-smoke.sh
```

## Operator configuration

`golemd --config PATH` requires TOML. It defines:

- `name`: this daemon's identity
- `[harnesses.<name>] models = [...]`: verbatim model IDs scoped to that harness
- `[projects.<name>]`: an absolute existing `path` and optional `description`
- `clone_enabled` (defaults false)
- `api_bearer_tokens` and `[attach_ssh] port` placeholders for later phases

Project paths are validated at startup. Dispatch is rejected immediately if its harness is not configured or its non-empty model is not listed for that harness. A model-less dispatch remains legal. Phase 1 still accepts direct absolute `--cwd`; project selection and clone behavior come later.

Existing process/provider environment settings remain available, including `GOLEM_PI`, `GOLEM_HOOK_EXTENSION`, `GOLEM_WEB_EXTENSION`, `GOLEM_PI_SOURCE_PROFILE`, `GOLEM_PI_DEFAULT_PROVIDER`, `GOLEM_PI_DEFAULT_MODEL`, `GOLEM_COPY_AUTH`, `GOLEM_CLAUDE_ARGV`, and `GOLEM_CODEX_ARGV`. Provider descriptors and credential-reference forwarding remain in the dispatch protocol until Phase 2.

## CLI commands

- `capabilities`
- `dispatch` (`--worktree` requests detached-worktree isolation)
- `status`, `list [--state]`, and `await`
- `attach-hint`, `answer`, `cancel`, and `reap`
- `gc --root DIR --older-than DURATION`

Use global `--json` for machine-readable output. `--service` accepts `unix:///path` or an HTTP loopback URL.

## Build and test

```sh
go test ./...
go build ./...
go vet ./...
./test/standalone-smoke.sh
```

## API and architecture

The listener exposes:

- `GET /v1/capabilities`
- `POST /v1/jobs`, `GET /v1/jobs`, `GET /v1/jobs/{id}`
- `POST /v1/jobs/{id}/{cancel,reap,answer}`
- internal local reconciliation via `POST /v1/jobs/poll` and `POST /v1/events`
- `GET /live`, `GET /ready`

See [`protocol/README.md`](protocol/README.md) for the wire contract and [`DECISIONS.md`](DECISIONS.md) for architecture and security boundaries.

## License

MIT
