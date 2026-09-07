import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { readFile } from "node:fs/promises";
import {
  catalogToProviderGroups,
  isCatalog,
  needsMaxOutputTokensShim,
  normalizeBaseUrl,
  withoutMaxOutputTokens,
} from "./catalog.ts";

function shellQuote(value: string): string {
  return `'${value.replaceAll("'", `'"'"'`)}'`;
}

/**
 * Registers Router-owned providers inside an isolated Golem pi worker.
 * The worker owns no OAuth state: it reads one shared, read-only Router client
 * token for catalog discovery and each inference request.
 */
export default async function tiamat(pi: ExtensionAPI) {
  const configuredUrl = process.env.GOLEM_TIAMAT_URL;
  const tokenFile = process.env.GOLEM_TIAMAT_TOKEN_FILE;
  const snapshotFile = process.env.GOLEM_TIAMAT_SNAPSHOT_FILE;
  if (!configuredUrl || !tokenFile || !snapshotFile) {
    console.error("[golem-tiamat] URL, token file, and authorized snapshot are required");
    return;
  }

  const baseUrl = normalizeBaseUrl(configuredUrl);
  // This is the exact credential-free row authorized with the durable job.
  // Never perform independent live discovery here: removal after acceptance
  // must not change provider construction or resume behavior.
  const catalog: unknown = JSON.parse(await readFile(snapshotFile, "utf8"));
  if (!isCatalog(catalog) || catalog.length !== 1) {
    throw new Error("Golem Tiamat authorized snapshot has an invalid shape");
  }

  // Pi resolves this command for each request. The token itself therefore
  // never enters settings.json, models.json, a job record, or an artifact.
  const apiKey = `!cat -- ${shellQuote(tokenFile)}`;
  const codexResponsesProviders = new Set<string>();
  for (const group of catalogToProviderGroups(catalog, baseUrl)) {
    if (needsMaxOutputTokensShim(group)) {
      codexResponsesProviders.add(group.id);
    }
    pi.registerProvider(group.id, {
      name: group.name,
      baseUrl: group.baseUrl,
      apiKey,
      authHeader: true,
      headers: { "x-tiamat-provider": group.tiamatProvider },
      api: group.api,
      models: group.models,
    });
  }

  // Codex's Router adapter rejects the standard Responses max_output_tokens
  // field. Preserve the same compatibility shim used by resident Familiar.
  pi.on("before_provider_request", (event, ctx) => {
    if (!ctx.model || !codexResponsesProviders.has(ctx.model.provider)) return;
    return withoutMaxOutputTokens(event.payload);
  });
}
