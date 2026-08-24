import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";

const manifest = readFileSync(new URL("./plugin.toml", import.meta.url), "utf8");

test("manifest is exact v1 and points only into the Golem contribution", () => {
  expect(manifest.match(/^familiar_api\s*=\s*1$/gm)?.length).toBe(1);
  expect(manifest).not.toMatch(/familiar_api\s*=\s*["']/);
  expect([...manifest.matchAll(/\$\{([^}]+)\}/g)].map((m) => m[1]).every((x) => x === "plugin_root")).toBe(true);
  for (const app of ["golem-service", "golem-supervisor", "golem-familiar-nav"])
    expect(manifest).toContain(`\${plugin_root}#${app}`);
  expect(manifest).toContain('"${plugin_root}/contrib/familiar/pi/agents"');
  expect(manifest).toContain('snapshot_url = "http://127.0.0.1:7340/v1/nav"');
});
