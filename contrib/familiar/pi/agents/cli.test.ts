import { describe, expect, test } from "bun:test";
import { appendGolemToolArgs, parseGolemCliArgv } from "./cli.ts";

describe("host-owned Golem CLI argv", () => {
  test("rejects missing and malformed configuration without echoing it", () => {
    for (const raw of [undefined, "not json", "[]", "[1]", "[\"  \"]"]) {
      const result = parseGolemCliArgv(raw);
      expect("error" in result).toBe(true);
      if ("error" in result && raw === undefined) {
        expect(result.error).toBe("Golem CLI configuration is missing (GOLEM_CLI_ARGV_JSON)");
      } else if ("error" in result) {
        expect(result.error).not.toContain(raw);
      }
    }
  });

  test("accepts relative-looking executable text for the host to validate", () => {
    expect(parseGolemCliArgv('["./golem", "arg with spaces"]')).toEqual({
      argv: ["./golem", "arg with spaces"],
    });
  });

  test("preserves whitespace-safe argv and maps tool arguments explicitly", () => {
    const argv = ["nix", "run", "/enrolled source#golem", "--"];
    expect(appendGolemToolArgs(argv, "http://127.0.0.1:7337", [
      "dispatch", "--host", "host with spaces", "prompt with spaces",
    ])).toEqual([
      "nix", "run", "/enrolled source#golem", "--", "--service",
      "http://127.0.0.1:7337", "--json", "dispatch", "--host",
      "host with spaces", "prompt with spaces",
    ]);
  });
});
