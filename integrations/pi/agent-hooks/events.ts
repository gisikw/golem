/**
 * Side-channel event writer for the Golem agent-hooks pi extension.
 *
 * Pure, I/O-light core so the event-shaping logic is testable in isolation
 * (bun test) without a live pi runtime. The extension (index.ts) wires pi
 * lifecycle events to `appendEvent`, which serializes one JSON object per line
 * to the append-only side channel the Go pi adapter reads with a durable cursor.
 *
 * Schema (see harnesses/pi/pi.go sideEvent):
 *   {"type":"running","ts":<ms>}
 *   {"type":"progress","ts":<ms>,"turn":<n>,"message":"<short>"}
 *   {"type":"settled","ts":<ms>,"verdict":"done","summary":"<text>","usage":{...}}
 *   {"type":"blocked","ts":<ms>,"id":"<qid>","prompt":"<question>","options":["a","b"]}
 *   {"type":"compaction","ts":<ms>,"count":<n>,"reason":"threshold"}
 *   {"type":"exhausted","ts":<ms>,"count":4,"reason":"compaction limit reached"}
 *
 * The blocked record is emitted by the agent_block tool (index.ts): pi has no
 * native "ask the operator" mechanism, so blocking is an explicit agent action.
 * Field names mirror Go's sideEvent / protocol.BlockedQuestion exactly (`prompt`,
 * not `question`; `ts`, not `at`) so the adapter projects it without translation.
 */
import fs from "node:fs";

export type SideEvent =
  | { type: "running"; ts: number }
  | { type: "progress"; ts: number; turn?: number; message?: string }
  | {
      type: "settled";
      ts: number;
      verdict: "done" | "failed" | "cancelled" | "timeout";
      summary?: string;
      usage?: { input: number; output: number; cost: number };
    }
  | { type: "blocked"; ts: number; id: string; prompt: string; options?: string[] }
  | { type: "compaction"; ts: number; count: number; reason: string }
  | { type: "exhausted"; ts: number; count: number; reason: string };

export interface TaskContext {
  id?: string;
  harness?: string;
  model?: string;
  cwd?: string;
  prompt?: string;
  workspace?: {
    project?: string;
    repo?: string;
    ref?: string;
    worktree?: string;
    path?: string;
  } | null;
}

/** Number this compaction will become on the active branch. */
export function nextCompactionCount(branchEntries: unknown[]): number {
  return branchEntries.filter(
    (entry) => entry && typeof entry === "object" && (entry as { type?: unknown }).type === "compaction",
  ).length + 1;
}

/**
 * Build the durable reorientation packet injected immediately after a worker
 * compaction. The original dispatch is repeated verbatim; live Git state is
 * sampled after compaction. The compacted summary immediately preceding this
 * packet remains authoritative for edits/tests/failures already encountered.
 */
export function compactionResteer(task: TaskContext, count: number, gitState: string): string {
  const ws = task.workspace ?? undefined;
  const workspace = ws
    ? [
        ws.project ? `project=${ws.project}` : "",
        ws.repo ? `repo=${ws.repo}` : "",
        ws.ref ? `ref=${ws.ref}` : "",
        ws.worktree ? `worktree=${ws.worktree}` : "",
        ws.path ? `path=${ws.path}` : "",
      ].filter(Boolean).join(" ")
    : "(unmanaged cwd)";
  const urgency = count >= 3
    ? "FINAL COMPACTION BUDGET: finish the assignment now, or call agents_block with the precise blocker. Do not broaden scope."
    : "Resume the delegated assignment without broadening scope.";
  return [
    `[GOLEM COMPACTION RE-STEER ${count}/3]`,
    urgency,
    "",
    "Original dispatch (verbatim):",
    task.prompt ?? "(missing dispatch)",
    "",
    `Job: ${task.id ?? "?"}  harness/model: ${task.harness ?? "?"}/${task.model ?? "?"}`,
    `Workspace: ${workspace}`,
    `CWD: ${task.cwd ?? "?"}`,
    "Live Git state:",
    gitState || "(unavailable)",
    "",
    "Before the next tool call, explicitly reorient against the compaction summary and this packet:",
    "- acceptance criteria and prohibitions from the original dispatch;",
    "- edits made and commands/tests already run;",
    "- unresolved failures or questions;",
    "- the precise next action;",
    "- whether pushing remains authorized (never infer authorization).",
  ].join("\n");
}

/** Minimal shape of a pi assistant message we read for settlement content. */
export interface AssistantLike {
  role: string;
  content?: unknown;
  usage?: {
    input?: number;
    output?: number;
    cost?: { total?: number };
  };
}

/** Extract the plain-text of an assistant message's content blocks. */
export function assistantText(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  const parts: string[] = [];
  for (const block of content) {
    if (block && typeof block === "object" && (block as { type?: string }).type === "text") {
      const t = (block as { text?: unknown }).text;
      if (typeof t === "string") parts.push(t);
    }
  }
  return parts.join("").trim();
}

/** The final assistant message in an agent_end messages array, if any. */
export function finalAssistant(messages: unknown[]): AssistantLike | undefined {
  for (let i = messages.length - 1; i >= 0; i--) {
    const m = messages[i] as AssistantLike | undefined;
    if (m && m.role === "assistant") return m;
  }
  return undefined;
}

export type SettledVerdict = "done" | "failed" | "cancelled" | "timeout";

/** Build a settled event from the stashed final assistant message. */
export function settledEvent(
  final: AssistantLike | undefined,
  now: number,
  verdict: SettledVerdict = "done",
): Extract<SideEvent, { type: "settled" }> {
  const ev: Extract<SideEvent, { type: "settled" }> = { type: "settled", ts: now, verdict };
  if (final) {
    const summary = assistantText(final.content);
    if (summary) ev.summary = summary;
    const u = final.usage;
    if (u && (typeof u.input === "number" || typeof u.output === "number")) {
      ev.usage = {
        input: u.input ?? 0,
        output: u.output ?? 0,
        cost: u.cost?.total ?? 0,
      };
    }
  }
  return ev;
}

/**
 * Build a blocked event from a worker's agent_block invocation. `question`
 * becomes the `prompt` field (Go/protocol name); suggested `options` are carried
 * verbatim so the operator sees them alongside the question.
 */
export function blockedEvent(
  question: string,
  options: string[] | undefined,
  now: number,
  id: string,
): Extract<SideEvent, { type: "blocked" }> {
  const ev: Extract<SideEvent, { type: "blocked" }> = { type: "blocked", ts: now, id, prompt: question };
  const opts = (options ?? []).filter((o) => typeof o === "string" && o.length > 0);
  if (opts.length) ev.options = opts;
  return ev;
}

/**
 * Tool-result text returned to a worker that just called agent_block: its
 * question is now durable on the side channel; it must end the turn and wait,
 * because the operator's answer arrives as the next user message (the supervisor
 * bracketed-paste send-keys path).
 */
export function blockedResultText(): string {
  return (
    "Your question was delivered to the operator via the side channel. " +
    "End your turn now and wait — the operator's answer will arrive as your next message. " +
    "Do not call agent_block again for the same question."
  );
}

/**
 * Append one event as a single line. Uses a synchronous append so a crash
 * mid-settlement cannot interleave a torn record with the next writer; the Go
 * cursor only advances past a complete newline-terminated line regardless.
 */
export function appendEvent(path: string, event: SideEvent): void {
  fs.appendFileSync(path, JSON.stringify(event) + "\n");
}
