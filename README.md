# Golem

Golem is a standalone delegated-worker system. A SQLite service owns durable jobs, one supervisor per host owns worker processes, a private tmux server owns attachable PTYs, and harness adapters translate worker lifecycle events. `pi` is one supported harness alongside minimal Claude, Codex, and configurable fake/argv workers.

## Features

- Durable, idempotent jobs, events, answers, and settlements
- Explicit host assignment and bounded offline recovery
- Interactive, writable tmux sessions with direct attach hints
- Lifecycle states: pending, assigned, starting, running, blocked, cancelling, done, failed, cancelled, timeout
- Optional detached Git worktrees and host-local artifact retention
- Blocking questions, steering, cancellation, settlement callbacks, and usage accounting where the harness supports them
- Unix-socket or loopback HTTP JSON API

TCP authentication and portable artifact storage are intentionally deferred. Do not expose the HTTP listener beyond loopback or a trusted tunnel.

## Requirements

- Go 1.23+
- `tmux`, `bash`, and `git` for supervised workers
- Nix is optional; the flake provides builds, checks, and a development shell

## Quick start

```sh
state=$(mktemp -d)
go run ./cmd/golem-service \
  --db "$state/service.db" --unix "$state/service.sock" --listen ''
```

In a second shell:

```sh
go run ./cmd/golem-supervisor \
  --service "unix://$state/service.sock" \
  --host local --state "$state/host" \
  --allowed-cwd-roots "$HOME"
```

Then dispatch the dependency-free fake harness:

```sh
go run ./cmd/golem --service "unix://$state/service.sock" \
  dispatch --host local --harness fake --cwd "$PWD" \
  'exercise the worker lifecycle'
go run ./cmd/golem --service "unix://$state/service.sock" list
go run ./cmd/golem --service "unix://$state/service.sock" attach-hint JOB_ID
```

Each shell must receive the same `state` value; using a fixed path instead of `mktemp` is often simpler.

## Build and test

```sh
go test ./...
go build ./cmd/...
# or
nix flake check
nix develop
```

The bundled pi event-shaping tests can also be run with `bun test integrations/pi/agent-hooks/events.test.ts` (Bun is included in the Nix development shell).

## Commands

The `golem` CLI supports:

- `dispatch` (`--host` is required; `--worktree` requests detached-worktree isolation)
- `status`, `list [--state]`, and `await`
- `attach-hint`
- `answer`, `cancel`, and `reap`
- `gc --root DIR --older-than DURATION`

Use global `--json` for machine-readable output.

## Configuration

Service variables:

- `GOLEM_DB`, `GOLEM_SOCKET`, `GOLEM_LISTEN` (defaults: `golem.db`, `golem.sock`, and `127.0.0.1:7337`)

Supervisor variables:

- `GOLEM_ENDPOINT`, `GOLEM_HOST`, `GOLEM_SUPERVISOR_STATE`
- `GOLEM_ARTIFACT_ROOT`, `GOLEM_ALLOWED_CWD_ROOTS`, `GOLEM_LINGER_SECONDS`
- `GOLEM_INTERACTIVE_SHELL`, `GOLEM_TMUX_THEME_CONFIG`
- `GOLEM_CLAUDE_ARGV`, `GOLEM_CODEX_ARGV` (JSON argv arrays)
- `GOLEM_SETTLEMENT_WEBHOOK`, `GOLEM_WORKLIST_DIR`

Pi harness variables:

- `GOLEM_PI`
- `GOLEM_HOOK_EXTENSION` (normally `integrations/pi/agent-hooks/index.ts`)
- `GOLEM_WEB_EXTENSION` (optional external extension)
- `GOLEM_PI_SOURCE_PROFILE` (optional model/default/theme source)
- `GOLEM_PI_DEFAULT_PROVIDER`, `GOLEM_PI_DEFAULT_MODEL`
- `GOLEM_COPY_AUTH=1` (explicitly opts into copying `auth.json` to private job state)

The supervisor defaults state to `~/.local/state/golem/supervisor`; artifacts live below it unless overridden. Allowed working-directory roots default to the user's home and are separated with the OS path-list separator.

## Harness behavior

Interactive harnesses own their tmux pane directly; tmux scrollback is the human-readable record and lifecycle is observed through a side channel. Minimal argv harnesses preserve output in both the pane and an artifact transcript while Bash `pipefail` retains the worker exit status.

The pi adapter creates an isolated `PI_CODING_AGENT_DIR` under each job's artifact directory. It loads only explicitly configured worker extensions, not the operator's extension list. The bundled `agent-hooks` extension emits lifecycle events and provides `agents_block` for explicit operator questions. A `ProviderConfig` can scope a worker to one provider/model; credential fields must remain unresolved host-side references. Copying stored pi authentication is disabled by default.

## API and architecture

Both listeners expose:

- `POST /v1/jobs`, `GET /v1/jobs`, `GET /v1/jobs/{id}`
- `POST /v1/jobs/{id}/{cancel,reap,answer}`
- `POST /v1/hosts/{host}/poll`
- `POST /v1/events`
- `GET /live`, `GET /ready`

See [`protocol/README.md`](protocol/README.md) for the wire contract and [`DECISIONS.md`](DECISIONS.md) for architecture and security boundaries.

## License

MIT
