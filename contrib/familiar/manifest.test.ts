import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

const manifest = readFileSync(new URL("./plugin.toml", import.meta.url), "utf8");
const flake = readFileSync(new URL("../../flake.nix", import.meta.url), "utf8");

test("manifest is exact v1 and points only into the Golem contribution", () => {
  expect(manifest.match(/^familiar_api\s*=\s*1$/gm)?.length).toBe(1);
  expect(manifest).not.toMatch(/familiar_api\s*=\s*["']/);
  expect([...manifest.matchAll(/\$\{([^}]+)\}/g)].map((m) => m[1]).every((x) => x === "plugin_root")).toBe(true);
  for (const app of ["golem-service", "golem-supervisor", "golem-familiar-render"])
    expect(manifest).toContain(`\${plugin_root}#${app}`);
  expect(manifest).toContain('"${plugin_root}/contrib/familiar/pi/agents"');
  expect(manifest).toContain('[chrome]');
  expect(manifest).toContain('render_url = "http://127.0.0.1:7340/v1/render"');
  expect(manifest).toContain('[pi.env]');
  expect(manifest).toContain('GOLEM_CLI_ARGV_JSON = "[\\"nix\\", \\"run\\", \\"${plugin_root}#golem\\", \\"--\\"]"');
  expect(manifest).not.toContain('[render]');
  expect(manifest).not.toContain('[nav]');
  expect(manifest).not.toContain('snapshot_url');
  expect(manifest).not.toContain('updates_url');
});

test("every ${plugin_root}#<app> in the manifest is an exposed flake app output", () => {
  // Attribute names under the flake `apps = { ... }` block that are directly
  // exposed as `nix run .#<name>`. We scope to the apps block to avoid matching
  // packages/checks with the same identifiers.
  const appsBlock = flake.match(/apps\s*=\s*\{([\s\S]*?)\n\s*\};/);
  expect(appsBlock).not.toBeNull();
  const exposedApps = new Set(
    [...appsBlock![1].matchAll(/^\s*([A-Za-z0-9_-]+)\s*=\s*\{/gm)].map((m) => m[1]),
  );
  const referenced = [...manifest.matchAll(/\$\{plugin_root\}#([A-Za-z0-9_-]+)/g)].map((m) => m[1]);
  expect(referenced.length).toBeGreaterThan(0);
  for (const app of referenced) expect(exposedApps.has(app)).toBe(true);
});
