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

## Backends

The run substrate is behind a `Backend` interface (`backend/`): start a harness process for a job with cwd/env/prompt, observe it, deliver text, cancel, tear down.

- **tmux (default, unchanged).** `backend/tmux` is golemd's private tmux server, its pinned policy, keystroke steering, and the host-local `terminal` endpoint that `golem attach` and the SSH attach listener use. This is what runs when `[herdr]` is absent from the config, byte for byte as before.
- **herdr (opt-in).** `backend/herdr` drives one [Herdr](https://herdr.dev) session on this host over its newline-delimited JSON socket (protocol 20): `workspace.create` → `agent.start --kind pi` → `agent.prompt`, per-pane `events.subscribe` for state plus a 15 s `agent.list` reconcile as the safety net, restart adoption by agent name (`job-<id>`), and cancel as `esc`/`ctrl+c` → `workspace.close` → verified absence of both agent and workspace.

Enable it with a `[herdr]` block (see [`golemd.example.toml`](golemd.example.toml)). At startup golemd pings the socket and requires protocol 20; if the socket is unreachable or speaks anything else it logs `HERDR BACKEND UNAVAILABLE, FALLING BACK TO TMUX` and comes up on tmux anyway. The selected substrate is logged as `run backend selected backend=…`.

On the herdr backend, in this first cut:

- only `pi` runs; any other harness is `400 harness "…" not supported on herdr backend` at dispatch;
- `attach` and `steer` are `501` with the real route (`herdr agent attach job-<id>` over ordinary SSH); no `terminal`/`activation` is published and the SSH attach listener is not started;
- questions, answers, artifacts, and settlements are unchanged — they ride pi's existing side channel, not Herdr;
- state observations map `working`/`idle`/`done` → running, `blocked` → blocked, `unknown` → last known state flagged stale, and `as_of` is golemd's receipt time (Herdr events carry no timestamp and no resumable cursor);
- Herdr's server is not golemd's to stop, so shutdown leaves live agents alone and re-adopts them by name on the next start.

Herdr's Pi integration is a lifecycle extension, but Herdr installs it into the `PI_CODING_AGENT_DIR` visible to the install command. Golem workers intentionally use a different, private directory per job. Provision one stable, operator-owned **seed** and point `[herdr].pi_extension` at the installed file:

```sh
# fort-nix (golemd runs as familiar:users); adapt owner for other deployments:
sudo install -d -o familiar -g users -m 0700 /var/lib/golem/herdr-pi-seed
sudo -u familiar env PI_CODING_AGENT_DIR=/var/lib/golem/herdr-pi-seed \
  herdr integration install pi
# Installed source consumed by golemd:
test -f /var/lib/golem/herdr-pi-seed/extensions/herdr-agent-state.ts
```

```toml
[herdr]
pi_extension = "/var/lib/golem/herdr-pi-seed/extensions/herdr-agent-state.ts"
```

Run the install only against the stable seed, never against a job artifact directory. On every Herdr-backed Pi start, Golem snapshots those exact bytes to `$ARTIFACT_DIR/pi/extensions/herdr-agent-state.ts` with mode `0600` and names only that private copy in the worker's explicit `settings.json` extension allowlist. The seed is never a worker profile, concurrent jobs never share a mutable profile, and Golem never asks Herdr to write a profile after creating it. Config loading rejects a missing/non-regular/non-absolute source; if it disappears later, that worker start fails loudly before settings are written. The default tmux backend does not read or require this setting.

Other Herdr-side requirements are a pinned Herdr version and a pane shell that does not clobber `PATH` (`[terminal] default_shell` — Herdr resolves the harness executable through the pane's `PATH`, and an interactive login shell may replace it with the system default). `./test/herdr-smoke.sh` starts a disposable Herdr server, installs a disposable seed integration, and runs a real Pi job through it.

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
- `[herdr]`: optional Herdr backend; Herdr-backed Pi requires an absolute `pi_extension` seed path as described above

Project paths and pi provider/model references are validated at startup. Dispatch selects either `--project NAME` or `--repo URL` plus `--worktree NAME`; the resulting `.golem/worktrees/NAME` is reused as the resume key. Repository cloning requires `clone_enabled`. Direct absolute `--cwd` remains a low-level test/fake-harness escape hatch.

Provider descriptors and credentials are not accepted over the wire. For pi, `<provider>/<model>` resolves against operator config and `api_key_env` is read from golemd's own environment only while its private per-job profile is written.

Workers are host-native, not cosmetic sandboxes: they inherit golemd's Unix permissions, network, and development substrate. The Nix package wraps `golemd` with `nix`, `nix-shell`, and `nix-store` on `PATH`, so workers can use the host Nix daemon directly instead of rebuilding ad hoc tool environments. Deployments that require genuine isolation must use a separate namespace/VM boundary rather than hiding those commands from `PATH`.

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
./test/herdr-smoke.sh   # real herdr server + real pi job; needs herdr and pi on PATH
```

## API and architecture

The listener exposes:

- `GET /v1/capabilities`
- `POST /v1/jobs`, `GET /v1/jobs`, `GET /v1/jobs/{id}`
- `GET /v1/jobs/{id}/artifacts` and `GET /v1/jobs/{id}/artifacts/{path...}`
- `GET /v1/jobs/{id}/attach` (terminal descriptor, or `501` on a substrate that proxies none)
- `GET /v1/events?since=SEQ[&job=ID]` (durable replay + live SSE)
- `POST /v1/jobs/{id}/{cancel,reap,answer,steer}`
- internal local reconciliation via `POST /v1/jobs/poll` and `POST /v1/events`
- `GET /live`, `GET /ready`

See [`protocol/README.md`](protocol/README.md) for the wire contract and [`DECISIONS.md`](DECISIONS.md) for architecture and security boundaries.

## License

MIT
