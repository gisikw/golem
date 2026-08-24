import { describe, expect, test } from "bun:test";
import { buildProviderConfig, modelEntry, type ModelDescriptor } from "./resolve.ts";

const tiamatModel: ModelDescriptor = {
  id: "claude-fable-5",
  name: "claude-fable-5 via work",
  api: "anthropic-messages",
  baseUrl: "https://router.example/anthropic/work",
  provider: "tiamat-anthropic-work",
  reasoning: false,
  input: ["text"],
  cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
  contextWindow: 200_000,
  maxTokens: 16_384,
};

const llamaModel: ModelDescriptor = {
  id: "gemma-4-E4B-it-Q4_K_M",
  name: "gemma-4-E4B-it-Q4_K_M",
  api: "openai-completions",
  baseUrl: "http://localhost:9931/v1",
  provider: "llama.cpp",
  reasoning: false,
  input: ["text"],
  contextWindow: 32_768,
  maxTokens: 32_768,
};

describe("modelEntry", () => {
  test("fills safe defaults and carries geometry", () => {
    const e = modelEntry(tiamatModel);
    expect(e.id).toBe("claude-fable-5");
    expect(e.baseUrl).toBe("https://router.example/anthropic/work");
    expect(e.api).toBe("anthropic-messages");
    expect(e.contextWindow).toBe(200_000);
  });
  test("supplies defaults when fields are absent", () => {
    const e = modelEntry({ id: "m" });
    expect(e.reasoning).toBe(false);
    expect(e.input).toEqual(["text"]);
    expect(e.cost).toEqual({ input: 0, output: 0, cacheRead: 0, cacheWrite: 0 });
    expect(e.contextWindow).toBe(128_000);
  });
});

describe("buildProviderConfig — tiamat-routed (registered extension provider)", () => {
  test("forwards the unresolved apiKey reference, never a plaintext secret", () => {
    const blob = buildProviderConfig({
      provider: "tiamat-anthropic-work",
      model: tiamatModel,
      registered: {
        name: "Tiamat anthropic (work)",
        baseUrl: "https://router.example/anthropic/work",
        apiKey: "!cat -- /run/familiar/tiamat.token",
        authHeader: true,
        api: "anthropic-messages",
      },
    });
    expect(blob.provider).toBe("tiamat-anthropic-work");
    expect(blob.model).toBe("claude-fable-5");
    expect(blob.builtin).toBeUndefined();
    const p = blob.models_json?.providers["tiamat-anthropic-work"] as Record<string, unknown>;
    expect(p.apiKey).toBe("!cat -- /run/familiar/tiamat.token");
    // Reference form, not a resolved secret.
    expect(String(p.apiKey).startsWith("!")).toBe(true);
    expect(p.baseUrl).toBe("https://router.example/anthropic/work");
    expect(p.authHeader).toBe(true);
    expect((p.models as unknown[]).length).toBe(1);
  });
});

describe("buildProviderConfig — local llama.cpp (keyless, models-store cached)", () => {
  test("emits a models.json entry with baseUrl and no apiKey", () => {
    const blob = buildProviderConfig({ provider: "llama.cpp", model: llamaModel, authConfigured: false });
    expect(blob.builtin).toBeUndefined();
    const p = blob.models_json?.providers["llama.cpp"] as Record<string, unknown>;
    expect(p.baseUrl).toBe("http://localhost:9931/v1");
    expect(p.apiKey).toBeUndefined();
    expect(p.api).toBe("openai-completions");
  });
});

describe("buildProviderConfig — direct /login'd provider (auth.json)", () => {
  test("stored credentials → builtin + copy_auth, no models.json", () => {
    const blob = buildProviderConfig({
      provider: "anthropic",
      model: { id: "claude-fable-5", provider: "anthropic" },
      authSource: "stored",
      authConfigured: true,
    });
    expect(blob.builtin).toBe(true);
    expect(blob.copy_auth).toBe(true);
    expect(blob.models_json).toBeUndefined();
  });
  test("environment-sourced key → builtin without copy_auth", () => {
    const blob = buildProviderConfig({
      provider: "anthropic",
      model: { id: "claude-fable-5", provider: "anthropic" },
      authSource: "environment",
      authConfigured: true,
    });
    expect(blob.builtin).toBe(true);
    expect(blob.copy_auth).toBeUndefined();
    expect(blob.models_json).toBeUndefined();
  });
});
