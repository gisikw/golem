/**
 * Golem agent-hooks — the pi "hook adapter" for the Golem agent system.
 *
 * Loaded into an interactive worker's pi instance with `--extension`. Because an
 * interactive TUI has no JSON stdout lifecycle stream, this extension reports
 * lifecycle out-of-band: it appends durable records to the side-channel path
 * named by GOLEM_EVENTS. The Go pi adapter (harnesses/pi)
 * advances a durable byte cursor over that file to project lifecycle and build
 * the settlement.
 *
 * Happy path: agent_start -> "running"/"progress" -> agent_settled -> "settled".
 * The settlement carries the final assistant message (verdict text) and usage
 * from the preceding agent_end, when the pi API exposes them.
 *
 * Blocked questions: pi 0.84.x exposes no first-class "awaiting operator input"
 * event, so blocking is an EXPLICIT AGENT ACTION. This extension registers the
 * `agents_block` tool; when the worker calls it we append a "blocked" record to
 * the side channel and tell the agent to end its turn and wait. The operator's
 * answer arrives as the next user message (supervisor bracketed-paste send-keys).
 * The tool IS the ask mechanism — no polling of TUI output is required.
 */
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import fs from "node:fs";
import {
  appendEvent,
  blockedEvent,
  blockedResultText,
  compactionResteer,
  finalAssistant,
  nextCompactionCount,
  settledEvent,
  type AssistantLike,
  type TaskContext,
} from "./events.ts";

export default function (pi: ExtensionAPI) {
  const path = process.env.GOLEM_EVENTS;
  if (!path) {
    // Not launched by the agent supervisor: do nothing rather than spam.
    return;
  }

  const emit = (event: Parameters<typeof appendEvent>[1]) => {
    try {
      appendEvent(path, event);
    } catch (err) {
      // The side channel is best-effort telemetry; a write failure must not
      // crash the worker's interactive session. The supervisor's process/exit
      // observation remains the crash boundary.
      try {
        process.stderr.write(`[agent-hooks] side-channel write failed: ${String(err)}\n`);
      } catch {
        /* ignore */
      }
    }
  };

  const task: TaskContext = (() => {
    const taskPath = process.env.GOLEM_TASK_CONTEXT;
    if (!taskPath) return {};
    try {
      return JSON.parse(fs.readFileSync(taskPath, "utf8")) as TaskContext;
    } catch (err) {
      try { process.stderr.write(`[agent-hooks] task context read failed: ${String(err)}\n`); } catch { /* ignore */ }
      return {};
    }
  })();

  // Stash the last agent_end payload so agent_settled (which carries no data)
  // can emit the final assistant message + usage. A deliberate agents_block
  // ends an agent run too, but is not job completion; suppress that settlement
  // until the operator answer starts the next run. Compaction exhaustion is a
  // harness failure and likewise suppresses a later ordinary "done" event.
  let lastFinal: AssistantLike | undefined;
  let awaitingAnswer = false;
  let exhausted = false;
  let pendingCompactionCount = 0;

  pi.on("agent_start", async () => {
    awaitingAnswer = false;
    emit({ type: "running", ts: Date.now() });
  });

  pi.on("turn_end", async (event) => {
    const turn = (event as { turnIndex?: number }).turnIndex;
    emit({ type: "progress", ts: Date.now(), ...(typeof turn === "number" ? { turn } : {}) });
  });

  pi.on("agent_end", async (event) => {
    const messages = (event as { messages?: unknown[] }).messages ?? [];
    lastFinal = finalAssistant(messages);
  });

  pi.on("agent_settled", async () => {
    if (!awaitingAnswer && !exhausted) {
      emit(settledEvent(lastFinal, Date.now(), "done"));
      lastFinal = undefined;
    }
  });

  // Compaction is bounded job-control state, not unbounded conversational
  // memory. Three compactions are allowed. Each successful compaction receives
  // an explicit re-steer containing the verbatim dispatch and live Git state;
  // the third orders finish-or-block. A fourth attempt fails the job and asks
  // pi to shut down. The supervisor treats the exhausted side event as an
  // immediate kill boundary, so graceful shutdown is only a backstop.
  pi.on("session_before_compact", async (event, ctx) => {
    const count = nextCompactionCount(event.branchEntries);
    pendingCompactionCount = count;
    if (count < 4) return;
    exhausted = true;
    emit({ type: "exhausted", ts: Date.now(), count, reason: "compaction limit reached" });
    ctx.shutdown();
    return { cancel: true };
  });

  pi.on("session_compact", async (event) => {
    const count = pendingCompactionCount || 1;
    pendingCompactionCount = 0;
    emit({ type: "compaction", ts: Date.now(), count, reason: event.reason });

    const git = async (): Promise<string> => {
      const status = await pi.exec("git", ["status", "--short", "--branch"], { timeout: 10_000 });
      const head = await pi.exec("git", ["rev-parse", "HEAD"], { timeout: 10_000 });
      const parts = [
        status.code === 0 ? status.stdout.trim() : `git status unavailable: ${status.stderr.trim()}`,
        head.code === 0 ? `HEAD ${head.stdout.trim()}` : "",
      ].filter(Boolean);
      return parts.join("\n") || "(unavailable)";
    };

    let gitState = "(unavailable)";
    try { gitState = await git(); } catch { /* bounded best effort */ }
    pi.sendMessage(
      { customType: "golem-compaction-resteer", content: compactionResteer(task, count, gitState), display: true },
      { deliverAs: "steer", triggerTurn: true },
    );
  });

  // Monotonic question id: distinguishes successive blocks within one worker so
  // the supervisor/operator idempotency keys never collide on the job id alone.
  let blockSeq = 0;

  pi.registerTool({
    name: "agents_block",
    label: "Block on Operator",
    description:
      "Ask the dispatching operator for input when genuinely blocked (missing " +
      "credentials, ambiguous requirements, destructive-action confirmation). " +
      "Delivers your question, then you MUST end your turn; the answer arrives " +
      "as your next message.",
    promptSnippet: "Ask the operator a blocking question and wait for the answer",
    parameters: Type.Object({
      question: Type.String({ description: "What you need from the operator to proceed" }),
      options: Type.Optional(
        Type.Array(Type.String(), { description: "Suggested answers the operator can pick from" }),
      ),
    }),
    async execute(_id, p: { question: string; options?: string[] }) {
      const id = `${blockSeq++}-${Date.now()}`;
      awaitingAnswer = true;
      emit(blockedEvent(p.question, p.options, Date.now(), id));
      return { content: [{ type: "text" as const, text: blockedResultText() }] };
    },
  });
}
