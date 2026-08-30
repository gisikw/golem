import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { readFile } from "node:fs/promises";
import {
  catalogToProviderGroups,
  isCatalog,
  normalizeBaseUrl,
  withoutMaxOutputTokens,
} from "./catalog.ts";

const CATALOG_PATH = "/tiamat/v1/models";

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
  if (!configuredUrl || !tokenFile) {
    console.error("[golem-tiamat] GOLEM_TIAMAT_URL and GOLEM_TIAMAT_TOKEN_FILE are required");
    return;
  }

  const baseUrl = normalizeBaseUrl(configuredUrl);
  const token = (await readFile(tokenFile, "utf8")).trim();
  if (!token) throw new Error("Golem Tiamat token file is empty");
  const response = await fetch(`${baseUrl}${CATALOG_PATH}`, {
    headers: { Authorization: `Bearer ${token}` },
    signal: AbortSignal.timeout(10_000),
  });
  if (!response.ok) throw new Error(`Golem Tiamat catalog returned HTTP ${response.status}`);
  const catalog: unknown = await response.json();
  if (!isCatalog(catalog)) throw new Error("Golem Tiamat catalog response has an invalid shape");

  // Pi resolves this command for each request. The token itself therefore
  // never enters settings.json, models.json, a job record, or an artifact.
  const apiKey = `!cat -- ${shellQuote(tokenFile)}`;
  for (const group of catalogToProviderGroups(catalog, baseUrl)) {
    pi.registerProvider(group.id, {
      name: group.name,
      baseUrl: group.baseUrl,
      apiKey,
      authHeader: true,
      api: group.api,
      models: group.models,
    });
  }

  // Codex's Router adapter rejects the standard Responses max_output_tokens
  // field. Preserve the same compatibility shim used by resident Familiar.
  pi.on("before_provider_request", (event, ctx) => {
    if (!ctx.model?.provider.startsWith("tiamat-responses-")) return;
    return withoutMaxOutputTokens(event.payload);
  });
}
