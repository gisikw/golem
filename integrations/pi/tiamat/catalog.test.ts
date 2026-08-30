import { describe, expect, test } from "bun:test";
import {
  catalogToProviderGroups,
  etagRequiresFetch,
  withoutMaxOutputTokens,
  type TiamatCatalogRecord,
} from "./catalog.ts";

const records: TiamatCatalogRecord[] = [
  { model: "claude-sonnet", api: "/anthropic/v1/messages", provider: "personal", fidelity: "native", availability: "available" },
  { model: "claude-sonnet", api: "/anthropic/v1/messages", provider: "work", fidelity: "native", availability: "degraded" },
  {
    model: "gpt-next", api: "/responses/v1/responses", provider: "codex/personal", fidelity: "native", availability: "available",
    context_window: 272_000, max_output_tokens: 128_000, reasoning: true, input: ["text", "image"],
    thinking_level_map: { minimal: "low", xhigh: "xhigh", max: "max" },
  },
  { model: "gone", api: "/openai/v1/chat/completions", provider: "metered", fidelity: "native", availability: "unavailable" },
];

describe("Tiamat catalog mapping", () => {
  test("keeps wire model ids clean by splitting duplicate models into account providers", () => {
    const groups = catalogToProviderGroups(records, "https://router.example/");
    expect(groups.map((group) => group.id)).toEqual([
      "tiamat-anthropic-personal",
      "tiamat-anthropic-work",
      "tiamat-responses-codex%2Fpersonal",
    ]);
    expect(groups[0].models[0].id).toBe("claude-sonnet");
    expect(groups[0].models[0].baseUrl).toBe("https://router.example/anthropic/personal");
    expect(groups[2].baseUrl).toBe("https://router.example/responses/codex%2Fpersonal/v1");
    expect(groups[2].models[0]).toMatchObject({
      reasoning: true,
      input: ["text", "image"],
      contextWindow: 272_000,
      maxTokens: 128_000,
      thinkingLevelMap: { minimal: "low", xhigh: "xhigh", max: "max" },
    });
  });

  test("shapes each base URL for the path appended by its pi API client", () => {
    const allWires: TiamatCatalogRecord[] = [
      { model: "claude", api: "/anthropic/v1/messages", provider: "account", fidelity: "native", availability: "available" },
      { model: "chat", api: "/openai/v1/chat/completions", provider: "account", fidelity: "native", availability: "available" },
      { model: "response", api: "/responses/v1/responses", provider: "account", fidelity: "native", availability: "available" },
    ];
    const byFamily = new Map(catalogToProviderGroups(allWires, "https://router.example/")
      .map((group) => [group.family, group.baseUrl]));

    // Anthropic appends /v1/messages; OpenAI appends /chat/completions or /responses.
    expect(byFamily.get("anthropic")).toBe("https://router.example/anthropic/account");
    expect(byFamily.get("openai")).toBe("https://router.example/openai/account/v1");
    expect(byFamily.get("responses")).toBe("https://router.example/responses/account/v1");
  });

  test("filters unavailable and labels degraded records", () => {
    const groups = catalogToProviderGroups(records, "https://router.example");
    expect(groups.flatMap((group) => group.models).some((model) => model.id === "gone")).toBe(false);
    expect(groups.find((group) => group.id === "tiamat-anthropic-work")?.models[0].name)
      .toBe("claude-sonnet via work (degraded)");
  });

  test("uses optional catalog token limits while retaining defaults", () => {
    const catalog: TiamatCatalogRecord[] = [{
      ...records[0], context_window: 200_000, max_output_tokens: 32_000,
    }, { ...records[1], model: "defaulted" }];
    const models = catalogToProviderGroups(catalog, "https://router.example").flatMap((group) => group.models);
    expect(models.find((model) => model.id === "claude-sonnet")).toMatchObject({ contextWindow: 200_000, maxTokens: 32_000 });
    expect(models.find((model) => model.id === "defaulted")).toMatchObject({ contextWindow: 128_000, maxTokens: 16_384 });
  });
});

describe("Responses compatibility", () => {
  test("removes max_output_tokens without mutating the request payload", () => {
    const payload = { model: "gpt-next", max_output_tokens: 16_384, stream: true };
    expect(withoutMaxOutputTokens(payload)).toEqual({ model: "gpt-next", stream: true });
    expect(payload.max_output_tokens).toBe(16_384);
  });

  test("leaves unrelated payloads untouched", () => {
    const payload = { model: "gpt-next", stream: true };
    expect(withoutMaxOutputTokens(payload)).toBe(payload);
  });
});

describe("ETag polling", () => {
  test("fetches only when a successful HEAD indicates possible change", () => {
    expect(etagRequiresFetch(304, '"old"', '"old"')).toBe(false);
    expect(etagRequiresFetch(200, '"old"', '"old"')).toBe(false);
    expect(etagRequiresFetch(200, '"old"', '"new"')).toBe(true);
    expect(etagRequiresFetch(200, undefined, '"new"')).toBe(true);
    expect(etagRequiresFetch(200, '"old"', null)).toBe(true);
    expect(etagRequiresFetch(500, '"old"', '"new"')).toBe(false);
  });
});
