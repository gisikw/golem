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
import { appendEvent, blockedEvent, blockedResultText, finalAssistant, settledEvent, type AssistantLike } from "./events.ts";

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

  // Stash the last agent_end payload so agent_settled (which carries no data)
  // can emit the final assistant message + usage. A deliberate agents_block
  // ends an agent run too, but is not job completion; suppress that settlement
  // until the operator answer starts the next run.
  let lastFinal: AssistantLike | undefined;
  let awaitingAnswer = false;

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
    if (!awaitingAnswer) {
      emit(settledEvent(lastFinal, Date.now(), "done"));
      lastFinal = undefined;
    }
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
