import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import { execFile } from "node:child_process";
import {
  buildProviderConfig,
  type ModelDescriptor,
  type ProviderConfigBlob,
  type RegisteredProviderConfig,
} from "./resolve.ts";

// Thin transport bridge to the independently deployable Golem.
// supervised service endpoint is supplied through the
// same GOLEM_ENDPOINT consumed by the CLI and this extension.
const pluginRoot = process.env.FAMILIAR_PLUGIN_ROOT;
const CLI = process.env.GOLEM_CLI || (pluginRoot ? "nix" : "golem");
const CLI_PREFIX = !process.env.GOLEM_CLI && pluginRoot
  ? ["run", `${pluginRoot}#golem`, "--"]
  : [];
const ENDPOINT = process.env.GOLEM_ENDPOINT || "http://127.0.0.1:7337";
const DEFAULT_HOST = process.env.GOLEM_HOST;
const MAX_OUTPUT = 2 * 1024 * 1024;

type ToolResult = {
  content: { type: "text"; text: string }[];
  details?: unknown;
  isError?: boolean;
};

const invoke = (args: string[], signal?: AbortSignal): Promise<ToolResult> =>
  new Promise((resolve) => {
    execFile(
      CLI,
      [...CLI_PREFIX, "--service", ENDPOINT, "--json", ...args],
      { encoding: "utf8", maxBuffer: MAX_OUTPUT, signal },
      (error, stdout, stderr) => {
        if (error) {
          const message = (stderr || error.message).trim();
          resolve({
            content: [{ type: "text", text: JSON.stringify({ ok: false, error: message }) }],
            details: { ok: false, error: message },
            isError: true,
          });
          return;
        }
        try {
          const value = JSON.parse(stdout);
          resolve({
            content: [{ type: "text", text: JSON.stringify(value, null, 2) }],
            details: value,
          });
        } catch {
          resolve({
            content: [{ type: "text", text: JSON.stringify({ ok: false, error: "golem returned invalid JSON" }) }],
            isError: true,
          });
        }
      },
    );
  });

// resolveProviderConfig inspects live pi model state to build the worker's
// single-provider descriptor. Default: the presence's currently-running model
// (ctx.model). Override: the requested `model` looked up in the registry
// (canonical "provider/model" or a bare id resolved via find). Returns undefined
// when nothing resolves (e.g. no UI/model context) so dispatch proceeds without
// a descriptor (back-compat: worker falls back to the copied catalog).
function resolveProviderConfig(ctx: ExtensionContext | undefined, requested?: string): ProviderConfigBlob | undefined {
  if (!ctx) return undefined;
  const registry = ctx.modelRegistry;
  let model = ctx.model as ModelDescriptor | undefined;
  if (requested) {
    const slash = requested.indexOf("/");
    const found = slash > 0
      ? registry?.find(requested.slice(0, slash), requested.slice(slash + 1))
      : registry?.getAll().find((m) => m.id === requested);
    if (found) model = found as ModelDescriptor;
    else if (!model) return undefined; // requested an unknown model and no current one
  }
  if (!model?.provider || !model.id) return undefined;
  const provider = model.provider;
  let registered: RegisteredProviderConfig | undefined;
  try {
    registered = registry?.getRegisteredProviderConfig(provider) as RegisteredProviderConfig | undefined;
  } catch { registered = undefined; }
  let authConfigured = false;
  let source: import("./resolve.ts").AuthSource;
  try {
    const status = registry?.getProviderAuthStatus(provider);
    authConfigured = Boolean(status?.configured);
    source = status?.source;
  } catch { /* status is best-effort */ }
  return buildProviderConfig({ provider, model, registered, authSource: source, authConfigured });
}

export default function (pi: ExtensionAPI) {
  pi.registerTool({
    name: "agents_dispatch",
    label: "Dispatch Agent",
    description: "Dispatch bounded work to the external Golem. Returns the durable job record immediately.",
    promptSnippet: "Dispatch async work to an independently supervised agent worker",
    parameters: Type.Object({
      prompt: Type.String({ description: "Complete task description; the worker starts cold" }),
      host: Type.Optional(Type.String({ description: "Worker host (defaults to GOLEM_HOST)" })),
      harness: Type.Optional(Type.String({ description: "Harness: pi, claude, codex, or fake (default pi)" })),
      model: Type.Optional(Type.String({ description: "Harness model override" })),
      cwd: Type.Optional(Type.String({ description: "Worker directory (default current directory)" })),
      worktree: Type.Optional(Type.Boolean({ description: "Request detached git-worktree isolation" })),
      key: Type.Optional(Type.String({ description: "Creation idempotency key" })),
    }),
    async execute(_id, p: { prompt: string; host?: string; harness?: string; model?: string; cwd?: string; worktree?: boolean; key?: string }, signal, _onUpdate, ctx: ExtensionContext) {
      const host = p.host || DEFAULT_HOST;
      if (!host) return invoke(["dispatch", "--host", "", p.prompt], signal);
      const args = ["dispatch", "--host", host];
      if (p.harness) args.push("--harness", p.harness);
      if (p.model) args.push("--model", p.model);
      if (p.cwd) args.push("--cwd", p.cwd);
      if (p.worktree) args.push("--worktree");
      if (p.key) args.push("--key", p.key);
      // A pi worker gets EXACTLY the dispatched (or, by default, the presence's
      // currently-running) model+provider. Resolve its single-provider
      // connection descriptor from live pi state and forward it opaquely; the
      // supervisor writes the worker's models.json/settings from it. Non-pi
      // harnesses and resolution failures degrade to no descriptor.
      if ((p.harness ?? "pi") === "pi") {
        const blob = resolveProviderConfig(ctx, p.model);
        if (blob) args.push("--provider-config", JSON.stringify(blob));
      }
      args.push(p.prompt);
      return invoke(args, signal);
    },
  });

  pi.registerTool({
    name: "agents_status",
    label: "Agent Status",
    description: "Inspect one delegated job, or list jobs. This does not block.",
    promptSnippet: "Inspect delegated-agent lifecycle state",
    parameters: Type.Object({
      id: Type.Optional(Type.String({ description: "Job id; omit to list jobs" })),
      state: Type.Optional(Type.String({ description: "Lifecycle filter when listing" })),
    }),
    async execute(_id, p: { id?: string; state?: string }, signal) {
      return p.id
        ? invoke(["status", p.id], signal)
        : invoke(["list", ...(p.state ? ["--state", p.state] : [])], signal);
    },
  });

  pi.registerTool({
    name: "agents_await",
    label: "Await Agent",
    description: "Block without polling in the conversation until a job settles or asks a blocked question. Timeout does not cancel it.",
    promptSnippet: "Join a delegated job and return its durable status or settlement",
    parameters: Type.Object({
      id: Type.String({ description: "Job id" }),
      timeout: Type.Optional(Type.Integer({ description: "Maximum seconds to wait (default 600)" })),
    }),
    async execute(_id, p: { id: string; timeout?: number }, signal) {
      return invoke(["await", "--timeout", `${p.timeout ?? 600}s`, p.id], signal);
    },
  });

  pi.registerTool({
    name: "agents_respond",
    label: "Respond to Agent",
    description: "Answer the currently blocked question for a delegated job.",
    promptSnippet: "Answer a blocked delegated agent",
    parameters: Type.Object({
      id: Type.String({ description: "Job id" }),
      text: Type.String({ description: "Answer text" }),
    }),
    async execute(_id, p: { id: string; text: string }, signal) {
      return invoke(["answer", p.id, p.text], signal);
    },
  });

  pi.registerTool({
    name: "agents_reap",
    label: "Reap Agent Session",
    description: "Explicitly remove a settled agent job's lingering tmux session. Running jobs are refused.",
    promptSnippet: "Remove retained terminal scrollback for a settled agent job",
    parameters: Type.Object({ id: Type.String({ description: "Settled job id" }) }),
    async execute(_id, p: { id: string }, signal) {
      return invoke(["reap", p.id], signal);
    },
  });

  pi.registerTool({
    name: "agents_cancel",
    label: "Cancel Agent",
    description: "Request durable cancellation of a delegated job.",
    promptSnippet: "Cancel a delegated agent job",
    parameters: Type.Object({ id: Type.String({ description: "Job id" }) }),
    async execute(_id, p: { id: string }, signal) {
      return invoke(["cancel", p.id], signal);
    },
  });
}
