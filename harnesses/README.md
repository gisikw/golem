# Harnesses

The interface in `harness.go` covers start, prompt, observe, answer, cancel, resume, and settlement collection. The supervisor supplies runtime callbacks and remains the sole process/tmux authority.

## The hook / side-channel pattern

A harness that runs as an **interactive TUI** has no machine-readable stdout
lifecycle stream. The documented path for such a harness is a sibling **hook
adapter** that instruments the harness to append durable JSON lifecycle records
to a well-known side-channel file (`Launch.Events`, one JSON object per line,
append-only). The adapter's `Observe` advances a durable byte cursor over that
file — never parsing TUI output — and maps records to lifecycle
(`running`/`progress` → progress, `settled` → `Observation.Settled` + verdict +
usage, `blocked` → a question). A crash boundary (pane death) is always the
supervisor's concern and needs no side-channel coverage. `Launch.Interactive`
tells the supervisor to give the harness the pane PTY directly (no tee-to-file
pipeline; the scrollback is the human record).

`pi` implements this pattern. It launches pi's normal interactive TUI
(`pi --session … --extension <agent-hooks> "<prompt>"`, the prompt delivered as
pi's initial message), and the bundled `agent-hooks` pi extension
(`integrations/pi/agent-hooks`) writes the side channel. Settlement
carries the final assistant message (verdict) and usage from the extension when
available, falling back to the cumulative pi session-JSONL usage sum (whose
per-operation records are authoritative). Blocked-question detection is a
documented stub: pi 0.84.x exposes no first-class "awaiting operator input"
event to extensions, so the schema reserves a `blocked` record but nothing emits
it yet.

## Minimal adapters

`TemplateAdapter` (used by Claude and Codex) is the honest minimum for harnesses
without a lifecycle API: it launches configured argv, keeps the tee-to-file
transcript, and infers only process exit/transcript operator. Unsupported native
operations (steering, blocked questions, usage, resume) return `ErrUnsupported`.
No adapter binary is required for unit tests; tests use shell fakes.
