// Provider resolution for worker dispatch.
//
// The presence runs every model through its tiamat router (or a local llama, or
// a directly /login'd provider). A dispatched worker runs an ISOLATED pi with
// its own coding-agent dir, so it cannot see any of that unless we forward the
// connection descriptor. This module builds the single-provider descriptor the
// worker's pi boots with — EXACTLY the dispatched model+provider, nothing else.
//
// SECURITY: the descriptor rides the job payload, which transits the service DB.
// We forward apiKey ONLY as pi's *unresolved* config-value reference (a value
// starting with "!" runs a host-local command, "$ENV"/"${ENV}" interpolate env),
// never a resolved plaintext secret. Built-in/login providers (credentials in
// auth.json) carry no key here at all: they set builtin+copy_auth and the
// supervisor copies auth.json into the private per-job dir. See
// DECISIONS.md #20.

/** A pi model, as exposed on ExtensionContext.model / ModelRegistry.find(). */
export interface ModelDescriptor {
  id: string;
  name?: string;
  api?: string;
  baseUrl?: string;
  provider?: string;
  reasoning?: boolean;
  input?: ("text" | "image")[];
  cost?: { input: number; output: number; cacheRead: number; cacheWrite: number };
  contextWindow?: number;
  maxTokens?: number;
  compat?: unknown;
}

/** The ProviderConfigInput a custom/extension provider registered (tiamat, llama). */
export interface RegisteredProviderConfig {
  name?: string;
  baseUrl?: string;
  apiKey?: string; // unresolved reference (!cmd / $ENV / literal) — forwarded verbatim
  api?: string;
  authHeader?: boolean;
}

export type AuthSource =
  | "stored"
  | "runtime"
  | "environment"
  | "fallback"
  | "models_json_key"
  | "models_json_command"
  | undefined;

/** Opaque descriptor forwarded to the CLI as --provider-config (protocol.ProviderConfig). */
export interface ProviderConfigBlob {
  provider: string;
  model: string;
  builtin?: boolean;
  copy_auth?: boolean;
  // A complete pi models.json object: { providers: { "<id>": {...} } }. Written
  // verbatim by the supervisor into the worker dir. Absent for builtin providers.
  models_json?: { providers: Record<string, unknown> };
}

/** Fill a pi models.json model entry from a resolved Model, with safe defaults. */
export function modelEntry(m: ModelDescriptor): Record<string, unknown> {
  const entry: Record<string, unknown> = {
    id: m.id,
    name: m.name ?? m.id,
    reasoning: m.reasoning ?? false,
    input: m.input ?? ["text"],
    cost: m.cost ?? { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
    contextWindow: m.contextWindow ?? 128_000,
    maxTokens: m.maxTokens ?? 16_384,
  };
  if (m.api) entry.api = m.api;
  if (m.baseUrl) entry.baseUrl = m.baseUrl;
  if (m.compat) entry.compat = m.compat;
  return entry;
}

/**
 * Build the single-provider descriptor for a dispatched worker.
 *
 * Cases (all three required transports covered):
 *  - Registered custom/extension provider (tiamat-routed, llama-via-extension):
 *    emit a models.json entry using the provider's unresolved apiKey reference.
 *  - Keyless local provider (local llama.cpp in models-store.json, no auth):
 *    emit a models.json entry with no apiKey.
 *  - Native /login'd provider (credentials in auth.json): builtin + copy_auth,
 *    no models.json (pi knows the provider natively; the worker gets auth.json).
 */
export function buildProviderConfig(args: {
  provider: string;
  model: ModelDescriptor;
  registered?: RegisteredProviderConfig;
  authSource?: AuthSource;
  authConfigured?: boolean;
}): ProviderConfigBlob {
  const { provider, model, registered, authSource, authConfigured } = args;
  const blob: ProviderConfigBlob = { provider, model: model.id };

  if (registered) {
    // Custom/extension provider. Forward the unresolved apiKey reference so no
    // plaintext secret is written; the worker resolves it host-side at runtime.
    const providerObj: Record<string, unknown> = {
      name: registered.name ?? provider,
      models: [modelEntry(model)],
    };
    const baseUrl = registered.baseUrl ?? model.baseUrl;
    if (baseUrl) providerObj.baseUrl = baseUrl;
    if (registered.apiKey) providerObj.apiKey = registered.apiKey;
    if (registered.api ?? model.api) providerObj.api = registered.api ?? model.api;
    if (registered.authHeader !== undefined) providerObj.authHeader = registered.authHeader;
    blob.models_json = { providers: { [provider]: providerObj } };
    return blob;
  }

  // Native provider. Credentials live in auth.json (stored) or ambient env.
  if (authSource === "stored" || (authConfigured && authSource === undefined)) {
    blob.builtin = true;
    blob.copy_auth = true;
    return blob;
  }
  if (
    authSource === "environment" ||
    authSource === "runtime" ||
    authSource === "models_json_key" ||
    authSource === "models_json_command" ||
    authSource === "fallback"
  ) {
    // Key flows through ambient provider env (e.g. ANTHROPIC_API_KEY) the
    // supervisor passes through; pi knows the provider natively.
    blob.builtin = true;
    return blob;
  }

  // Not configured and not registered: a keyless local server addressed purely
  // by baseUrl (local llama.cpp cached in models-store.json). Emit a models.json
  // entry with no apiKey.
  if (model.baseUrl) {
    blob.models_json = {
      providers: {
        [provider]: {
          name: provider,
          baseUrl: model.baseUrl,
          ...(model.api ? { api: model.api } : {}),
          models: [modelEntry(model)],
        },
      },
    };
    return blob;
  }

  // Last resort: treat as a built-in and hope ambient env/auth suffices.
  blob.builtin = true;
  return blob;
}
