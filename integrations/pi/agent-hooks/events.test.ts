/* agent-hooks side-channel event tests — headless, no pi runtime required.
 * Run with:  nix develop .#stt -c bun test integrations/pi/agent-hooks/events.test.ts
 *   (bun lives in the .#stt dev shell; there is no node in .#pi)
 */
import { expect, test, describe } from "bun:test";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import {
  appendEvent,
  assistantText,
  blockedEvent,
  blockedResultText,
  compactionResteer,
  finalAssistant,
  nextCompactionCount,
  settledEvent,
} from "./events.ts";

describe("assistantText", () => {
  test("joins text blocks and trims", () => {
    expect(assistantText([{ type: "text", text: " hello " }, { type: "text", text: "world" }])).toBe(
      "hello world",
    );
  });
  test("passes through a string", () => {
    expect(assistantText("verdict")).toBe("verdict");
  });
  test("ignores non-text blocks", () => {
    expect(assistantText([{ type: "toolCall", name: "x" }, { type: "text", text: "ok" }])).toBe("ok");
  });
});

describe("finalAssistant", () => {
  test("returns the last assistant message", () => {
    const msgs = [
      { role: "user", content: "go" },
      { role: "assistant", content: [{ type: "text", text: "first" }] },
      { role: "toolResult", content: [] },
      { role: "assistant", content: [{ type: "text", text: "final" }] },
    ];
    expect(assistantText(finalAssistant(msgs)?.content)).toBe("final");
  });
  test("undefined when no assistant present", () => {
    expect(finalAssistant([{ role: "user", content: "go" }])).toBeUndefined();
  });
});

describe("blockedEvent", () => {
  test("maps question to prompt and carries options", () => {
    expect(blockedEvent("which db?", ["postgres", "sqlite"], 7, "0-7")).toEqual({
      type: "blocked",
      ts: 7,
      id: "0-7",
      prompt: "which db?",
      options: ["postgres", "sqlite"],
    });
  });
  test("omits options when empty or absent, drops blanks", () => {
    expect(blockedEvent("go?", undefined, 1, "a")).toEqual({ type: "blocked", ts: 1, id: "a", prompt: "go?" });
    expect(blockedEvent("go?", ["", "ok"], 1, "a")).toEqual({ type: "blocked", ts: 1, id: "a", prompt: "go?", options: ["ok"] });
  });
  test("result text tells the agent to end its turn and wait", () => {
    const t = blockedResultText();
    expect(t).toContain("End your turn");
    expect(t).toContain("next message");
  });
});

describe("compaction policy", () => {
  test("counts active-branch compactions and ignores other entries", () => {
    expect(nextCompactionCount([])).toBe(1);
    expect(nextCompactionCount([{ type: "message" }, { type: "compaction" }, { type: "custom" }])).toBe(2);
    expect(nextCompactionCount([{ type: "compaction" }, { type: "compaction" }, { type: "compaction" }])).toBe(4);
  });

  test("re-steer repeats dispatch and state checklist; third is finish-or-block", () => {
    const task = {
      id: "job-1",
      harness: "pi",
      model: "op/model",
      cwd: "/work",
      prompt: "Implement X. Do not push.",
      workspace: { project: "p", worktree: "wt", path: "/work" },
    };
    const normal = compactionResteer(task, 1, "## main\nHEAD abc");
    expect(normal).toContain("Original dispatch (verbatim):\nImplement X. Do not push.");
    expect(normal).toContain("edits made and commands/tests already run");
    expect(normal).toContain("whether pushing remains authorized");
    expect(normal).toContain("worktree=wt");
    const final = compactionResteer(task, 3, "clean");
    expect(final).toContain("FINAL COMPACTION BUDGET");
    expect(final).toContain("agents_block");
    expect(final).toContain("Do not broaden scope");
  });
});

describe("settledEvent", () => {
  test("carries verdict, summary, and usage", () => {
    const ev = settledEvent(
      { role: "assistant", content: [{ type: "text", text: "done it" }], usage: { input: 10, output: 4, cost: { total: 0.002 } } },
      1000,
      "done",
    );
    expect(ev).toEqual({ type: "settled", ts: 1000, verdict: "done", summary: "done it", usage: { input: 10, output: 4, cost: 0.002 } });
  });
  test("omits usage/summary when absent", () => {
    expect(settledEvent(undefined, 5, "failed")).toEqual({ type: "settled", ts: 5, verdict: "failed" });
  });
});

describe("appendEvent", () => {
  test("writes one newline-terminated JSON object per call", () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "agent-hooks-"));
    const p = path.join(dir, "events.jsonl");
    appendEvent(p, { type: "running", ts: 1 });
    appendEvent(p, { type: "settled", ts: 2, verdict: "done", summary: "ok" });
    const lines = fs.readFileSync(p, "utf8").split("\n").filter(Boolean);
    expect(lines).toHaveLength(2);
    expect(JSON.parse(lines[0])).toEqual({ type: "running", ts: 1 });
    expect(JSON.parse(lines[1]).verdict).toBe("done");
    fs.rmSync(dir, { recursive: true, force: true });
  });
});
