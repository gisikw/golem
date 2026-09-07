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

Enable it with a `[herdr]` block (see [`golemd.example.toml`](golemd.example.toml)). Golemd starts its **own foreground child** in `$STATE/herdr` by default, with explicit `--session`, `HERDR_SESSION`, `HERDR_SOCKET_PATH`, `HERDR_CONFIG_PATH`, private `HOME`/XDG config/state, and a generated config. It never probes, attaches to, or stops the user's Herdr. A second live server in Golem's private namespace is refused rather than adopted. Startup then pings the private socket and requires protocol 20; any child, socket, or protocol failure logs `HERDR BACKEND UNAVAILABLE, FALLING BACK TO TMUX` and comes up on tmux. Graceful golemd shutdown signals only the exact child PID it started. The selected substrate is logged as `run backend selected backend=…`.

On the herdr backend, in this first cut:

- only `pi` runs; any other harness is `400 harness "…" not supported on herdr backend` at dispatch;
- `attach` and `steer` are `501` with the real route (`herdr agent attach job-<id>` over ordinary SSH); no `terminal`/`activation` is published and the SSH attach listener is not started;
- questions, answers, artifacts, and settlements are unchanged — they ride pi's existing side channel, not Herdr;
- state observations map `working`/`idle`/`done` → running, `blocked` → blocked, `unknown` → last known state flagged stale, and `as_of` is golemd's receipt time (Herdr events carry no timestamp and no resumable cursor);
- Golemd owns Herdr restart policy: generated config sets `[session] resume_agents_on_restore = false`, so Herdr cannot independently resurrect agents from persisted terminal state. Golem's durable registry and adapter resume path remain authoritative.

At first private-server startup, golemd invokes its selected Herdr binary as `integration install pi` against the stable `$STATE/herdr/pi-seed` profile. It never invokes the installer against a job. Existing seed bytes are deliberately retained across restarts (upgrades therefore require an explicit seed removal/reprovision rollout). On every Pi start or resume, Golem snapshots those bytes to `$ARTIFACT_DIR/pi/extensions/herdr-agent-state.ts` with mode `0600` and names only that private copy in the worker's explicit `settings.json` allowlist. Concurrent workers share neither profile nor extension destination. `pi_extension` remains available only as an absolute, validated expert override.

The generated config also sets `shell_mode = "non_login"`, disables version and detection-manifest background checks (`manifest_check = false`), and chooses a profile-less shell so workspace `PATH` survives. The Nix daemon package bundles pinned Herdr 0.8.2 plus a `bash --noprofile --norc` shell and supplies both explicitly; non-Nix runs default to `herdr` on `PATH` and `/bin/sh`, or can set `[herdr].binary` and `[herdr].shell`. `./test/herdr-smoke.sh` proves the owned startup, seed install, private worker copy, real Pi path, cancel, and shutdown cleanup.

Two lifecycle limits are intentional. A hard golemd crash may leave its child alive; the replacement refuses to attach or kill it and loudly falls back until an operator verifies that exact PID (see [`docs/fort-nix-herdr-rollout.md`](docs/fort-nix-herdr-rollout.md)). Also, Herdr's session-derived Unix socket must fit the platform's roughly 104-byte limit, so keep `[herdr].root` and `session` short; golemd rejects paths over 100 bytes before launch. A Herdr child that dies after successful startup is reflected as backend unavailability/job failure, not an automatic mid-run switch to tmux.

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
- `[harnesses.<name>] models = [...]`: static verbatim model IDs scoped to that harness
- `[projects.<name>]`: an absolute existing `path` and optional `description`
- `[providers.<name>]`: static pi `base_url` and optional `api_key_env`
- `[tiamat]`: optional dynamic Pi catalogue discovery, with optional `providers` and exact advertised `models` restrictions plus bounded cache/timeout/size controls
- `clone_enabled` (defaults false)
- `api_bearer_tokens`: bearer credentials enforced on every TCP request; Unix sockets are exempt
- `[attach_ssh]`: optional port, host key path, and authorized_keys path (port 0 disables it)
- `[herdr]`: optional golemd-owned private Herdr backend; `root`, `binary`, `session`, and profile-less `shell` have safe defaults and `pi_extension` is normally omitted

Project paths and pi provider/model references are validated at startup. Dispatch selects either `--project NAME` or `--repo URL` plus `--worktree NAME`; the resulting `.golem/worktrees/NAME` is reused as the resume key. Repository cloning requires `clone_enabled`. Direct absolute `--cwd` remains a low-level test/fake-harness escape hatch.

Provider descriptors and credentials are not accepted over the wire. For static pi providers, `<provider>/<model>` resolves against operator config and `api_key_env` is read from golemd's own environment only while its private per-job profile is written.

Static `[providers.*] kind = "tiamat"` entries are rejected with a migration error; remove those duplicated provider blocks and their Tiamat entries from `harnesses.pi.models` when enabling `[tiamat]`.

When `[tiamat]` is present, golemd reads the existing `GOLEM_TIAMAT_URL` and `GOLEM_TIAMAT_TOKEN_FILE` conventions and discovers compatible Anthropic Messages, OpenAI Completions, and OpenAI Responses records just in time. It ignores unsupported wires, rejects unavailable records, and advertises degraded records as usable. Advertisement and dispatch use the same bounded, single-flight resolver. A refresh failure serves an explicitly `stale` catalogue (and HTTP `Warning: 110`) only through `stale_ttl`. With no usable dynamic cache, `GET /v1/capabilities` remains 200 and reports `discovery.tiamat.status = "unavailable"`; all static Pi and non-Pi capabilities and dispatches continue to work, while unverified dynamic selections are rejected.

Dynamic authorization is a per-job snapshot. On acceptance, golemd durably stores only the selected credential-free catalogue row and workers provision and resume from exactly that row—even if the live catalogue later removes it or Router is down. This guarantees deterministic provider construction, not inference success: Router may still reject a request when the backing account has since become unavailable. A stale advertised model may be accepted and pinned until `stale_ttl`; after that it disappears from capabilities. Workers use unscoped Router API URLs and send the raw, validated provider ID in `x-tiamat-provider`, so provider IDs containing `/` work through Go Router path parsing. They receive the token *path* in their environment and resolve the token at inference-request time; token contents never enter config, protocol, store, logs, or artifacts. See `golemd.example.toml` for controls and restriction syntax.

For the Azula `fort-nix` migration, edit `hosts/azula/manifest.nix` as follows: preserve the existing `GOLEM_TIAMAT_URL`, `GOLEM_TIAMAT_TOKEN_FILE`, and secret ownership/mode; add an enabled `[tiamat]` table (empty restrictions unless an intentional allowlist is wanted); remove the three static `kind = "tiamat"` provider stanzas; remove every Router-backed static entry from `[harnesses.pi].models`; and retain the direct `llama/...` fallback plus its ordinary static provider if desired. Do not leave old Tiamat model IDs in the static model list, because that would keep removed/unavailable models advertised.

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
