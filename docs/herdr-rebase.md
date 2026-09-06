# Rebasing Golem onto Herdr

**Status:** design brief, not a plan of record. **Date:** 2026-09-06.
**Golem at:** `6767023` (this worktree). **Herdr evidence:** binary `herdr 0.8.1`
(`/nix/store/rcdrb6snphhhlj1s8wqsc0m5w98sb3fn-herdr-0.8.1/bin/herdr`, `herdr api schema --json`,
protocol version 20) plus the published docs for v0.8.2 fetched from
`https://raw.githubusercontent.com/herdrdev/herdr/v0.8.2/docs/next/website/src/content/docs/`
(`socket-api.mdx`, `agents.mdx`, `integrations.mdx`, `persistence-remote.mdx`) and `herdr --skill`
from the installed binary. Herdr is *not* at `github.com/gisikw/herdr` (404); upstream is
`herdrdev/herdr`, home `herdr.dev`. Every capability claim below is from the schema or those docs;
anything I could not verify is called out as unverified rather than assumed.

## 0. The thesis

Golem today is two products welded together: a durable job control plane, and a hand-rolled
terminal substrate (private tmux server, pane policy, keystroke steering, restricted SSH attach —
`supervisor/tmux.go`, `attachssh/`, decisions 4, 6, 11, 14, 21). The Familiar contract no longer
asks for the second one: deep observability is Herdr's job, reached by a deeplink from a surface.
Herdr 0.8 already ships persistent panes, agent detection with `idle/working/blocked/done/unknown`,
`agent.start` for `pi` and `claude` (and 20+ others), a newline-delimited JSON socket API with
event subscriptions, and remote attach over SSH.

So: **delete Golem's terminal substrate, keep Golem's ledger.** Golem becomes a capability that owns
job identity, idempotency, workspace contracts, questions, settlement and artifacts, and drives one
Herdr session per host as its executor. This is the same boundary Wings drew when it moved onto
Herdr (see `hoard/primers/wings-herdr-general-purpose-runtime`, which proved the seam live); Golem
should not re-derive it from scratch, and should not repeat Wings' mistake of treating Herdr state
as flight state.

## 1. What Golem stops doing

| Stops | Herdr affordance that replaces it |
| --- | --- |
| Owning a private tmux server, its policy file, birth/reassert/kill logic (`supervisor/tmux.go`, decisions 4, 14) | `workspace.create --cwd --env` → root pane; `pane.split`; the Herdr server owns persistence and restore (`session-state`, `[session] resume_agents_on_restore`). |
| Launching harness argv into a pane and polling `pane_dead` for exit | `agent.start <name> --kind pi|claude --pane <id>`, which returns only after Herdr detects the expected agent and considers it interactive-ready (default 30 s startup timeout; `timeout_ms` 3 000–300 000). |
| Inferring "blocked" and "running" itself | `pane.agent_status_changed` events + `agent.get`. Pi has *lifecycle-hook* authority when its integration is installed; Claude is screen-manifest detection only (`agents.mdx` status-authority table). |
| Steering by tmux `send-keys` with bracketed paste (decision 11) | `agent.prompt <target> <text> [--wait]`, which submits text and Enter atomically, honours the pane's live bracketed-paste mode, and refuses (`agent_blocked`) if the agent sits at an approval dialog. `agent.send-keys` for `esc`/`ctrl+c`. |
| Its own restricted SSH attach listener, host keys, authorized_keys, username-selects-job (`attachssh/`, decision 21) | **No replacement needed inside Golem.** The contract no longer asks for it. Humans reach a job via ordinary SSH plus `herdr --remote host --session golem` or `herdr agent attach <name>`. Golem returns a *deeplink descriptor*, not a terminal. |
| `TerminalEndpoint{socket,target}` and `Activation{ssh,port,user}` in the wire (decisions 6, 21) | One opaque `deeplink` object (§5). Golem never returns UI; a surface renders it. |
| Reap/GC of tmux sessions | `workspace.close` (verify absence, §3.4) plus Golem's existing artifact GC. |

Steering deserves a note: `steer` is not one of the four contract verbs. It survives only as an
internal mechanism — the same delivery path `answer` uses — and stops being a public verb. If a
surface wants mid-flight direction it is `answer` to an open question or a new `dispatch`.

## 2. What Golem keeps

Everything that makes Golem a *capability* rather than a terminal:

- **The ledger.** SQLite store, durable events with a global sequence, atomic first-settlement-wins
  (`service/store.go`, decisions 1, 20). This stays authoritative. Herdr states are observations.
- **Idempotency keys.** `CreateJob.IdempotencyKey` and `Answer.IdempotencyKey` are unchanged and
  become more important: `dispatch` retries must not create a second Herdr workspace. Golem binds
  `job_id → {workspace_id, tab_id, pane_id, agent_name}` durably before or immediately after
  `workspace.create`, and adopts on restart. Use `agent_name = job-<id-prefix>` (Herdr names must
  match `[a-z][a-z0-9_-]{0,31}` and be unique among live agents) so a crashed golemd can re-find its
  agent by name via `agent.list`.
- **Workspaces/worktrees and artifacts.** `service/workspace.go` keeps resolving
  `project|repo + worktree → .golem/worktrees/NAME`, and `artifacts/` keeps its bounded listing and
  path-sanitising retrieval (decision 22). Golem stays the correctness boundary for Git; Herdr's
  `worktree.create` is not idempotent and returns no base/HEAD provenance (documented in
  `socket-api.mdx`, independently confirmed by the Wings investigation). Optionally call
  `worktree.open` afterwards so the checkout shows native worktree provenance in Herdr's UI.
- **The four verbs.** `dispatch` ← `POST /v1/jobs`; `status` ← `GET /v1/jobs/{id}` and `GET /v1/jobs`;
  `answer` ← `POST /v1/jobs/{id}/answer`; `cancel` ← `POST /v1/jobs/{id}/cancel`. Declared shapes
  come from `protocol/` unchanged except for the deleted terminal/activation fields.
- **The two streams.** `GET /v1/events` (durable replay + SSE) already carries `job.created`,
  `job.state`, `job.progress`, `job.settled`; it is projected into `jobs`. Blocked events with a
  `BlockedQuestion` payload project into `questions`. Both keep golemd's `Seq` cursor — Herdr has no
  resumable cursor of its own (§3.3), so Golem's must remain the durable one.
- **Registration.** `name, version, verbs, streams, liveness` comes from `config.Capabilities`.
  Liveness should be **`ping {interval: 30s}`**, not `assumed`: golemd is a daemon with a hard
  dependency on a live Herdr server, and readiness that ignores that dependency is a lie.
  `GET /ready` gains a `ping` on the Herdr socket.
- **Multi-host.** Unchanged: golemd ×N, explicit per-host addressing, no server-side scheduling
  (decisions 3, 19). Each golemd owns exactly one Herdr session on its own host
  (`HERDR_SESSION=golem`, socket `~/.config/herdr/sessions/golem/herdr.sock`), reached over the
  local Unix socket only. Golem must never talk to a Herdr on another host: Herdr's socket has no
  authentication, and exposing it on the network (as `fort-nix`'s `herdr-fleet-socket` does with
  socat) delegates the entire trust boundary to the tailnet. One session per golemd — not per job —
  keeps restore policy, config, and version pinning under one operator control.

## 3. How Golem drives Herdr

Transport: newline-delimited JSON over the session's Unix socket (`socket-api.mdx`, "Socket
transport"). Golem should speak the socket directly rather than shelling out to the CLI: it needs
long-lived subscriptions, and one process per state read is silly at fleet scale. The CLI stays as
the operator/debug surface and as the fallback while the client is written.

### 3.1 dispatch

1. Resolve the workspace (Golem's own Git code) → absolute path.
2. `workspace.create {cwd, label: "golem/<job>", env: {…}}` — env carries the pi profile dir,
   provider credentials from golemd's own environment, and `GOLEM_JOB_ID`. Env on process-launching
   methods is documented and applies to the launched process only; Herdr-owned `HERDR_*` variables
   win on conflict. **Secrets in env, never in argv** — pane argv is visible in `layout.export` and
   in `pane.process_info`.
3. `agent.start {name: "job-<id>", kind: "pi"|"claude", pane_id: <root pane>, args: [...]}`.
   Note the schema: `agent.start` takes **no cwd and no env** — they must come from the workspace or
   pane that already exists. That is why step 2 is a `workspace.create`, not a bare `agent.start`.
4. `agent.prompt {target: "job-<id>", text: <prompt>}` without `wait` (Golem waits on events, not on
   a blocking RPC).
5. Persist the binding, emit `job.state: running` with `as_of = now`.

### 3.2 status

Golem's store answers `status`. It is refreshed from two sources: the event subscription (§3.3) and
a low-frequency reconcile (`agent.get`/`agent.list`, every ~15 s and always on reconnect), which is
also how a restarted golemd re-adopts live jobs. Mapping, with Golem's ledger as the authority:

| Herdr | Golem | Note |
| --- | --- | --- |
| `working` | `running` | |
| `idle` | `running` | Idle is "ready for input", not "finished". |
| `done` | `running` | `done` is only "idle after unseen work". **It never settles a job.** |
| `blocked` | `blocked` + `questions` event | §4. |
| `unknown` | last known state, flagged stale | Herdr says explicitly that `unknown` does not prove completion. |
| `pane_exited` / agent absent | terminal, via settlement | For pi, the side-channel settlement is authoritative; for claude, process exit is. |

Terminal states keep requiring a durable settlement (`protocol.ValidateTransition`). Golem's pi hook
adapter (`harnesses/pi`, `integrations/pi/agent-hooks`) continues to supply verdict and usage over
its JSONL side channel; Herdr's own pi integration reports lifecycle state and session identity and
**does not** produce a verdict.

### 3.3 jobs events and an honest `as_of`

`events.subscribe` takes a list of subscription filters (e.g.
`{"type":"pane.agent_status_changed","pane_id":"w1:p1"}`), acknowledges, then pushes event lines on
the same connection. Verified from the schema: those event envelopes carry **no timestamp, no
sequence number, and there is no `since`/resume parameter**. `session.snapshot` is the documented
bootstrap, "not a subscription"; after a reconnect you re-snapshot.

Therefore `as_of` must be Golem's own clock, and Golem must say which kind of observation it was:

- event-driven: `as_of` = golemd's receipt time of the pushed line;
- reconcile/poll: `as_of` = the time the `agent.get` response was read;
- resync after a dropped subscription: `as_of` = the `session.snapshot` read time, with the whole
  window between disconnect and resync treated as unobserved.

Golem must not backdate an event to when it thinks the transition happened. Honest `as_of` means
"Golem knew this at T", nothing more. Because there is no gapless resume, a dropped socket is a
correctness event: reconnect → `session.snapshot` → diff against the ledger → emit whatever changed
with the resync `as_of`.

**Hook to add to Herdr — PR sketch A: monotonic, timestamped, resumable events.**
Add to every subscription-event envelope `seq` (server-monotonic `u64`, one counter per server run)
and `emitted_unix_ms`; add `server_run_id` to the `events.subscribe` acknowledgement and to
`session.snapshot`; accept `since_seq` on `events.subscribe`, replaying from a bounded in-memory
ring when `server_run_id` matches and returning `resume_unavailable` (client falls back to snapshot)
when it does not. ~1 file for the envelope type, one bounded `VecDeque` in the event bus, one new
param; strictly additive to protocol 20. This is the single highest-value change: it turns Golem's
`as_of` from "when I noticed" into "when it happened", and removes a whole class of silent gaps.

**PR sketch B: pane exit status.** `pane_exited` carries only `pane_id` and `workspace_id`, and
`exit_code` exists nowhere in the API except plugin command logs. Add `exit_code` and `signal`
(both nullable) to the `pane.exited` event and to `PaneInfo` for a pane whose process has exited.
Golem needs this for `claude`/`codex`-style minimal harnesses, where process exit *is* the verdict
(`harnesses.BasicSettlement`). Without it, Golem must scrape a transcript or shell out to a wrapper,
which is exactly the workaround this brief refuses to write.

**PR sketch C: readable block message.** `pane.report_agent --message TEXT` accepts a message, but
`AgentInfo`/`PaneInfo` and `pane_agent_status_changed` expose no `message` field, so a reporter
cannot read back what it reported. Add `message` (last accepted lifecycle report, source-scoped) to
`AgentInfo`, `PaneInfo`, and the status-changed event. Until then Golem carries question text on its
own side channel (§4) and uses pane metadata *tokens* (readable, ≤32 keys, ≤80 chars) only for the
question ID.

Everything else Golem needs exists today; no other hook should be added.

### 3.4 cancel

1. `agent.send-keys job-<id> esc` then `ctrl+c` (Herdr validates keys before writing bytes).
2. Wait briefly for `working → idle` or agent absence.
3. `workspace.close <workspace_id>`, then **verify**: `agent.get` and `workspace.get` must both
   report absence. A successful close response is not by itself evidence that descendants died —
   Herdr documents no process-group termination contract, and Wings hit exactly this.
4. Settle `cancelled` in the ledger; `cancel` is idempotent and safe to repeat.

Escalation beyond that (SIGKILL of the process group) is not available through Herdr's API today.
If verification fails, Golem should settle `failed` with an explicit "cancel unverified" summary
rather than lie — and that is the case for PR sketch B's sibling, a future `pane.kill --signal`.

### 3.5 answer

`agent.prompt <target> <text>`. Pi delivers it as the next TUI message (decision 12 unchanged).
Note that `agent.prompt` returns `agent_blocked` when the agent is at a *screen-detected* approval
dialog: for Claude permission prompts, an answer must therefore go through
`agent.send-keys` (`1`/`enter`/`esc`) rather than `agent.prompt`. That asymmetry is real and should
be encoded per harness adapter, not hidden.

## 4. Questions

Two shapes, one stream.

**Pi (structured).** Unchanged from today: Golem's own pi extension registers `agents_block`, the
harness calls it, the hook writes a JSONL record with prompt, options, and question ID, Golem
observes its cursor and emits a `questions` event with a real payload. Herdr independently reports
`blocked` for that pane via its pi lifecycle integration; Golem uses that as corroboration and as
the *fast* trigger to read its side channel, but the payload comes from Golem's channel. This keeps
decision 12 ("blocked questions are explicit adapter actions") intact.

**Claude (unstructured).** Herdr classifies `blocked` from a screen manifest — strictly, and only
for known approval UI; unknown prompts fall back to `idle`. There is no question text. On
`agent_status → blocked`, Golem emits a `questions` event whose prompt is a bounded
`agent.read --source detection` snapshot, marked `detail.source = "screen"` and
`options = []`. That is honest: it says "the harness is waiting on this screen", not "the harness
asked this question". The consumer's agent triages it; `answer` routes back as `send-keys`.

Both paths end in the same contract event: `questions` → consumer State, never a pager. `answer`
carries an idempotency key; delivery is at-least-once and the adapter is responsible for not
double-sending (existing answer-delivery machinery in `supervisor/reconciler.go` survives, minus
tmux).

## 5. Migration

**Survives** (roughly 70% of the Go): `protocol/`, `service/` (store, http, events, artifacts,
workspace), `client/`, `cmd/golem`, `cmd/golemd`, `artifacts/`, `config/` minus tmux/ssh keys,
`harnesses/harness.go` + `harnesses/pi` + `integrations/pi` (the side channel is the reason pi
settles honestly), the reconciler's *policy* half.

**Deleted**: `supervisor/tmux.go` and `supervisor/tmux_test.go`; `supervisor/terminal_reassert_test.go`;
`attachssh/` entirely (server, tests, host-key and authorized-keys handling); `[attach_ssh]` config;
`protocol.TerminalEndpoint` and `protocol.Activation`; `golem attach` / `attach-hint` /`steer` CLI
verbs; the tmux-specific parts of `harnesses.Runtime` (`SendText`, `Cancel`, `Alive` become Herdr
calls). Decisions 6, 11, 14, and 21 are superseded; 4 is rewritten as "process reality is Herdr's".

**New**: `herdr/` — a small nd-JSON socket client (request/response, subscription, reconnect,
snapshot resync); `supervisor/herdr_runtime.go` implementing the same `harnesses.Runtime` shape
against `agent.*`; a durable `job_bindings` table.

**Config after** (`golemd.toml`):

```toml
name = "local"
clone_enabled = false
api_bearer_tokens = []

[herdr]
session = "golem"                 # → ~/.config/herdr/sessions/golem/herdr.sock
socket_path = ""                  # optional explicit override
min_version = "0.8.1"             # checked via ping/status at startup, fail closed
startup_timeout_ms = 60000        # agent.start bound
reconcile_interval = "15s"
pi_extension = "/var/lib/golem/herdr-pi-seed/extensions/herdr-agent-state.ts"

[herdr.kinds]                     # golem harness → herdr agent kind
pi = "pi"
claude = "claude"

[harnesses.pi]
models = ["openai/gpt-5.6"]

[projects.scratch]
path = "/tmp"

# [attach_ssh] is gone.
```

Herdr-side operator requirements, stated once: create the stable seed and install Pi's lifecycle
extension on fort-nix with `sudo install -d -o familiar -g users -m 0700 /var/lib/golem/herdr-pi-seed`, then
`sudo -u familiar env PI_CODING_AGENT_DIR=/var/lib/golem/herdr-pi-seed herdr integration install pi`,
and configure `pi_extension = "/var/lib/golem/herdr-pi-seed/extensions/herdr-agent-state.ts"`.
Never install into a job-private profile. Also set
`[session] resume_agents_on_restore = false` for the golem session (Golem, not Herdr, decides
whether a job resumes after a cold restart — reviving an agent whose job was cancelled is the
failure mode Wings documented), and a pinned Herdr version.

**`attachssh` becomes a deeplink.** `GET /v1/jobs/{id}` gains:

```json
"deeplink": {"kind":"herdr","host":"joker","session":"golem","agent":"job-9f3c1a",
             "hint":"ssh joker -t herdr --session golem agent attach job-9f3c1a"}
```

Golem asserts identifiers, not UI; the surface decides whether to render a terminal, an
`herdr --remote joker --session golem` invocation, or nothing. Access control moves to ordinary SSH
on the host — which is what it always effectively was, minus a bespoke listener Golem had to
maintain.

## 6. Minimum viable cut

**Goal:** one `dispatch` of a `pi` job runs end to end through Herdr, `status` is honest, `cancel`
works and is verified, and tmux is absent from that path.

In scope: pi only; one workspace per job; events by subscription with `as_of = receipt time`;
reconcile poll as the safety net; questions and artifacts unchanged (they already work off the pi
side channel and the filesystem); `attach`/`steer` return `501` rather than being fully removed.
Out of scope: claude/codex, PR sketches A–C (assume none of them land), worktree provenance,
deleting `attachssh` from the tree (just stop starting it).

| Step | Hours |
| --- | --- |
| `herdr/` socket client: connect, request/response, subscribe, reconnect + `session.snapshot` resync | 4 |
| Start path: `workspace.create` (cwd/env) → `agent.start` → `agent.prompt`; persist binding | 4 |
| Status: subscription → state mapping → `job.state` events with `as_of`; 15 s reconcile; restart adoption by agent name | 5 |
| Cancel: `esc`/`ctrl+c` → `workspace.close` → absence verification → settlement | 3 |
| Config `[herdr]`, capability/version check at startup, stop launching tmux and attachssh for this path | 2 |
| Smoke test (`test/standalone-smoke.sh` variant), doc updates, decision-record edits | 4 |
| **Total** | **~22 h** (3 focused days, one person, no Herdr changes required) |

The proof is a single command sequence: `golem dispatch --harness pi --project scratch --worktree mvp`,
then `golem status`, `golem await`, and `golem cancel` on a second job — with `tmux ls` empty and
`herdr workspace list` showing exactly one workspace per live job.

## 7. Risks / unknowns

- No resumable event cursor in Herdr: every socket drop is an observation gap until PR sketch A lands.
- No timestamps on Herdr events: `as_of` is receipt time, so latency shows up as apparent staleness.
- No exit status anywhere in the pane API: minimal (claude/codex) harnesses cannot settle honestly yet.
- `workspace.close` gives no documented descendant-termination guarantee; cancel can be unverifiable.
- Claude `blocked` is screen-manifest only and deliberately conservative: novel prompts read as `idle`, so a job can look running while it waits forever.
- Herdr's socket is unauthenticated and user-scoped; any network exposure moves the trust boundary entirely onto the tailnet.
- **Provisioning boundary implemented:** Herdr writes `herdr-agent-state.ts` only once into an operator-owned seed; Golem snapshots its bytes into each per-job `PI_CODING_AGENT_DIR` with mode `0600` before writing the explicit extension allowlist. Herdr is never a second writer to job profiles, and concurrent workers share neither settings nor extension destinations. **Remaining risk:** the operator can replace the mutable seed between dispatches, so workers may intentionally run different Herdr integration bytes over time; pin and update the seed as deployment configuration. If the configured source disappears or becomes unreadable, Golem now fails the affected start loudly rather than silently losing lifecycle authority.
- One Herdr server per host is a shared fate: `herdr server stop` or a bad config reload takes every job on that host with it.
- Herdr version drift: detection manifests update themselves remotely by default, so state classification can change under a running golemd unless `[update] manifest_check = false`.
- Agent names are capped at 32 chars and must be unique among *live* agents; a reused job-id prefix after a crash could bind to the wrong pane if adoption does not also check workspace and cwd.
- Unverified: whether `agent.start` can resume a native pi/claude session for an adopted job under Golem's control rather than Herdr's restore policy.

## 8. What the minimum viable cut actually learned (2026-09-06)

Implemented on branch `herdr-mvp` against the same installed `herdr 0.8.1`, proven by
`test/herdr-smoke.sh` (a real headless herdr server, a real pi job on the cheap local model, a
real verified cancel). Additions to §7, all verified live rather than read off the schema:

- **One request per connection.** herdr 0.8.1 closes a socket connection as soon as it has
  answered one request; a second write gets `EPIPE`. Only a subscription keeps its connection.
  There is no long-lived control connection to hold, so "one process per state read" was never the
  cost the brief assumed — but one *connection* per read is.
- **`pane.agent_status_changed` subscriptions are per pane** (`pane_id` is required), so it is one
  subscription connection per live job, not one per session. A session-wide agent-status
  subscription would be a small, strictly additive PR (sketch D).
- **`agent.start` succeeds before the agent is a named live agent.** `agent.prompt` immediately
  afterwards fails `agent_not_ready: … is not an active named agent`, and `agent.list` shows
  `launch_pending: true`. Golem must poll `agent.get` for `interactive_ready` before prompting.
  Failing that prompt while leaving the workspace open is worse than failing the job: the retry
  adopts an idle pi that was never given its task, and the job runs forever at `running`.
- **The pane shell can silently discard the workspace `env`.** `workspace.create --env` reaches the
  shell, but an interactive shell that re-sources the system profile (NixOS bash re-runs
  `/etc/set-environment`) replaces `PATH` — and `PATH` is exactly how Herdr resolves the agent
  kind's executable. `bash: pi: command not found` is the failure. Operator requirement: set
  `[terminal] default_shell` for the golem session to a shell that runs no profile/rc files (or
  put the harness binary on the login `PATH`). Secrets in env are unaffected; only `PATH` is
  clobbered, but the job cannot start without it.
- **`agent.read`/`pane.read --source recent` returned empty for freshly created panes** in our
  runs while `--source visible` worked. Anything reading pane text for evidence (the claude
  question path in §4) should use `visible` or handle empty `recent`.
- **Cancel verification works as designed.** `esc` → `ctrl+c` → `workspace.close` → `agent.get`
  and `workspace.get` both reporting absence was reliable for a live pi; nothing was left behind
  and `tmux ls` stayed empty for the whole run.
- **No exit status is still the sharpest missing edge.** When an agent vanishes, Golem's only
  observable is absence, so a pi worker that dies without a side-channel settlement settles
  `failed` with no exit code. PR sketch B remains necessary before claude/codex.
- Two `TestPrivateTmux*` tests fail on this host both before and after the change (a tmux version
  difference in the PageUp/copy-mode binding assertion), so "the existing suite is green" means
  "unchanged", not "all green".


## 9. Private-server shipping update (implemented)

The earlier host-shared-server and manual-seed assumptions in §§2, 5, 7, and
8 are superseded by the shipping implementation. A configured `[herdr]` now
makes golemd start and own a foreground Herdr child below `STATE/herdr` (or an
absolute configured root). It passes both `--session` and isolated
`HERDR_SESSION`, `HERDR_SOCKET_PATH`, `HERDR_CONFIG_PATH`, `HOME`,
`XDG_CONFIG_HOME`, and `XDG_STATE_HOME`; inherited Herdr selectors are removed.
A live socket in that namespace is refused, never attached. Shutdown signals
only the recorded child process and never executes an ambient `herdr server
stop`.

The flake pins the official Herdr 0.8.2 artifact and a no-profile/no-rc bash
pane shell. Generated config fixes `shell_mode = "non_login"`,
`resume_agents_on_restore = false`, `version_check = false`, and
`manifest_check = false`. Golemd installs the bundled Pi lifecycle integration
once into `ROOT/pi-seed` and retains those stable bytes; the adapter's existing
atomic per-job copy remains the only path named by worker settings. The opt-in
and loud tmux fallback remain unchanged.

The unavoidable hard-crash edge is explicit: an orphan in the private
namespace is not adopted or killed automatically. The replacement daemon
falls back to tmux until an operator verifies and terminates that exact old
child. This trades automatic recovery for the stronger guarantee that Golem
cannot attach to or kill a user's Herdr.
