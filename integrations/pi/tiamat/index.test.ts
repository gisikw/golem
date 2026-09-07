import { afterEach, describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import tiamat from "./index.ts";
import type { TiamatCatalogRecord } from "./catalog.ts";

const dirs: string[] = [];
afterEach(() => {
  for (const dir of dirs.splice(0)) rmSync(dir, { recursive: true, force: true });
});

interface Registered { id: string; headers?: Record<string, string> }

/** Boots the extension over a one-row authorized snapshot and returns its wiring. */
async function boot(record: TiamatCatalogRecord) {
  const dir = mkdtempSync(join(tmpdir(), "golem-tiamat-"));
  dirs.push(dir);
  const snapshot = join(dir, "snapshot.json");
  const token = join(dir, "token");
  writeFileSync(snapshot, JSON.stringify([record]));
  writeFileSync(token, "not-a-real-token\n");
  process.env.GOLEM_TIAMAT_URL = "https://router.example/";
  process.env.GOLEM_TIAMAT_TOKEN_FILE = token;
  process.env.GOLEM_TIAMAT_SNAPSHOT_FILE = snapshot;

  const providers: Registered[] = [];
  const hooks: Array<(event: { payload: unknown }, ctx: { model?: { provider: string } }) => unknown> = [];
  const pi = {
    registerProvider(id: string, options: { headers?: Record<string, string> }) {
      providers.push({ id, headers: options.headers });
    },
    on(name: string, handler: (event: { payload: unknown }, ctx: { model?: { provider: string } }) => unknown) {
      if (name === "before_provider_request") hooks.push(handler);
    },
  };
  // deno-lint-ignore no-explicit-any -- the fake implements only what the extension uses.
  await tiamat(pi as any);
  const payload = { model: record.model, max_output_tokens: 16_384, stream: true };
  const applied = hooks.map((hook) => hook({ payload }, { model: { provider: providers[0].id } }));
  return { providers, applied };
}

describe("Codex Responses compatibility wiring", () => {
  test("strips max_output_tokens for the live codex-personal provider", async () => {
    const { providers, applied } = await boot({
      model: "gpt-5.6-sol", api: "/responses/v1/responses", provider: "codex-personal",
      fidelity: "native", availability: "available",
    });
    expect(providers[0].id).toBe("tiamat-responses-codex-personal");
    expect(providers[0].headers).toEqual({ "x-tiamat-provider": "codex-personal" });
    expect(applied).toEqual([{ model: "gpt-5.6-sol", stream: true }]);
  });

  test("strips max_output_tokens for a slash-scoped codex provider", async () => {
    const { applied } = await boot({
      model: "gpt-next", api: "/responses/v1/responses", provider: "codex/personal",
      fidelity: "native", availability: "available",
    });
    expect(applied).toEqual([{ model: "gpt-next", stream: true }]);
  });

  test("leaves a non-Codex Responses provider's token bound intact", async () => {
    const { providers, applied } = await boot({
      model: "gpt-5.6", api: "/responses/v1/responses", provider: "openai-personal",
      fidelity: "native", availability: "available",
    });
    expect(providers[0].id).toBe("tiamat-responses-openai-personal");
    expect(applied).toEqual([undefined]);
  });

  test("leaves non-Responses wires untouched", async () => {
    const { applied } = await boot({
      model: "claude-opus-5", api: "/anthropic/v1/messages", provider: "claude-code-personal",
      fidelity: "projected", availability: "available",
    });
    expect(applied).toEqual([undefined]);
  });
});
