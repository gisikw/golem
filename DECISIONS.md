# Golem design decisions

This document records the extracted system's intentional boundaries. It is not a roadmap for a plugin framework.

1. **Durable authority is a SQLite-backed Go service.** Job and event updates are transactional. The pure-Go SQLite driver keeps deployment simple.
2. **Transport is Unix socket or loopback HTTP JSON.** Unix permissions are the local trust boundary. Authenticated network transport is deferred.
3. **Assignment is explicit by host.** Capability scheduling is deferred; deterministic ownership makes partition behavior understandable.
4. **Process reality is host-local.** A supervisor maintains a durable worker registry and owns a private tmux server. Already-running workers survive service outages; bounded offline restart applies only to missing workers.
5. **Lifecycle is common, evidence is harness-specific.** Jobs share stable states while adapters may attach opaque details, progress, questions, usage, and settlements.
6. **PTY access is direct.** Same-host users receive a tmux socket and target. Remote terminal proxying and authentication are deferred.
7. **Artifacts are logical globally and paths locally.** The service assigns an opaque artifact ID. The supervisor resolves it below an allowed root, validates working directories, and may create detached worktrees there. Portable artifact storage is deferred.
8. **Start failures are bounded and durable.** Transient pre-registration failures use persisted attempts and backoff; permanent configuration or authorization failures settle immediately.
9. **Interactive harnesses use lifecycle side channels.** A TUI owns its pane without a tee wrapper. Hook adapters append durable JSONL events and observation advances a persisted byte cursor. Process death remains the crash boundary.
10. **Minimal argv harnesses remain minimal.** Claude and Codex wrappers provide argv templating, transcript, and exit status only; they do not claim resume, usage, native blocking, or steering semantics.
11. **Steering uses tmux keystrokes.** Multi-line input uses bracketed paste followed by Enter. Cancellation remains process-level.
12. **Blocked questions are explicit adapter actions.** The pi hook registers `agents_block`; an answer is delivered as the next TUI message. This avoids guessing from rendered terminal output.
13. **Settlement callbacks are best-effort.** Webhook and atomic worklist drop-box notifications run after durable settlement and can never invalidate it.
14. **Worker tmux policy is private and complete.** It is supplied at server birth and re-applied after starts and supervisor restarts. Worker panes retain exit status; unrelated human-opened panes do not remain after exit.
15. **Pi is one adapter, not the system architecture.** Its profile, event schema, usage fallback, and tool integration live under `harnesses/pi` and `integrations/pi`.
16. **Pi profiles are isolated per job.** The adapter always sets a private `PI_CODING_AGENT_DIR` and writes an explicit extension list. It reads only selected defaults/catalog files from an optional source profile.
17. **Provider descriptors are single-provider and secret-reference-only.** A dispatched pi worker may receive one provider/model descriptor. Any API-key field must be an unresolved host-side config reference, never plaintext. Stored authentication is copied only by explicit opt-in into 0700/0600 job state. Cross-host credential provisioning is deferred.
18. **Builds remain one Go module and one flake.** The CLI, service, and supervisor are separate outputs from the same source and lock files.
