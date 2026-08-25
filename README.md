# Golem

Golem is a standalone delegated-worker system. One `golemd` daemon per host owns its SQLite job store, supervisor, private tmux server, harness configuration, and durable worker registry. Fleet-aware clients choose which daemon to contact; there is no server-side host scheduling.

## Features

- Durable, idempotent jobs, events, answers, and settlements
- Interactive, writable tmux sessions via local tmux or restricted SSH attach
- Lifecycle states from `assigned` through running, blocked, and terminal outcomes
- Persistent named Git workspaces and host-local artifact retention
- Pi, minimal Claude/Codex, and dependency-free fake harness adapters
- Operator-advertised harness/model and project capabilities
- Unix-socket or bearer-authenticated TCP HTTP JSON API
- Bounded artifact listings and secure byte/range retrieval

Portable artifact storage remains deferred. The separately configured SSH attach listener is public-key-only and terminal-only.

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
go run ./cmd/golem --service "$endpoint" attach JOB_ID
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
- `[providers.<name>]`: pi `base_url` and optional `api_key_env`
- `clone_enabled` (defaults false)
- `api_bearer_tokens`: bearer credentials enforced on every TCP request; Unix sockets are exempt
- `[attach_ssh]`: optional port, host key path, and authorized_keys path (port 0 disables it)

Project paths and pi provider/model references are validated at startup. Dispatch selects either `--project NAME` or `--repo URL` plus `--worktree NAME`; the resulting `.golem/worktrees/NAME` is reused as the resume key. Repository cloning requires `clone_enabled`. Direct absolute `--cwd` remains a low-level test/fake-harness escape hatch.

Provider descriptors and credentials are not accepted over the wire. For pi, `<provider>/<model>` resolves against operator config and `api_key_env` is read from golemd's own environment only while its private per-job profile is written.

## CLI commands

- `capabilities`
- `dispatch` (`--project` or `--repo`, plus a named `--worktree`; low-level `--cwd`)
- `status`, `list [--state]`, and `await`
- `artifacts JOB-ID` and `artifacts JOB-ID PATH [-o FILE]`
- `attach` (local tmux fast path, otherwise SSH), `attach-hint`, `answer`, `steer`, `cancel`, and `reap`
- `gc --root DIR --older-than DURATION`

Use global `--json` for machine-readable output. `--service` accepts `unix:///path` or an HTTP URL. For TCP, pass `--token TOKEN` or set `GOLEM_TOKEN`.

## Remote HTTP access

Configure one or more `api_bearer_tokens`, then bind explicitly, for example `golemd --listen 100.64.0.10:7341`. TCP requests, including SSE, require `Authorization: Bearer TOKEN`; the Unix socket remains protected by filesystem permissions instead. An empty token list is accepted only on a loopback bind and produces a development warning. Golemd does not terminate TLS: use a trusted tailnet or tunnel as the transport-security layer.

For a machine outside the tailnet, keep golemd on loopback and tunnel it over SSH:

```sh
ssh -N -L 7341:127.0.0.1:7341 worker-host
GOLEM_ENDPOINT=http://127.0.0.1:7341 GOLEM_TOKEN="$TOKEN" golem capabilities
```

The bearer token still authenticates the tunneled request; SSH supplies confidentiality in transit.

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
- `GET /v1/jobs/{id}/artifacts` and `GET /v1/jobs/{id}/artifacts/{path...}`
- `GET /v1/events?since=SEQ[&job=ID]` (durable replay + live SSE)
- `POST /v1/jobs/{id}/{cancel,reap,answer,steer}`
- internal local reconciliation via `POST /v1/jobs/poll` and `POST /v1/events`
- `GET /live`, `GET /ready`

See [`protocol/README.md`](protocol/README.md) for the wire contract and [`DECISIONS.md`](DECISIONS.md) for architecture and security boundaries.

## License

MIT
