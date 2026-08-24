import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";

const source = readFileSync(new URL("./index.ts", import.meta.url), "utf8");

describe("Familiar Presence extension", () => {
  test("registers the stable tool names exactly once", () => {
    const names = [...source.matchAll(/name:\s*"(agents_[^"]+)"/g)].map((m) => m[1]);
    expect(names).toEqual([
      "agents_dispatch", "agents_status", "agents_await", "agents_respond", "agents_reap", "agents_cancel",
    ]);
    expect(new Set(names).size).toBe(names.length);
  });
  test("uses the host-owned argv transport and retains provider/artifact job output", () => {
    expect(source).toContain("parseGolemCliArgv");
    expect(source).toContain("process.env.GOLEM_ENDPOINT");
    expect(source).toContain("execFile(");
    expect(source).toContain("CLI.argv[0]");
    expect(source).not.toContain('"golem"');
    expect(source).not.toContain("FAMILIAR_PLUGIN_ROOT");
    expect(source).not.toContain("nix");
    expect(source).toContain("--provider-config");
    expect(source).toContain("details: value");
  });
});
