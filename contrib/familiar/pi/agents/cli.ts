export type GolemCliConfig =
  | { argv: string[] }
  | { error: string };

/** Parse the host-owned executable argv without ever consulting PATH or a shell. */
export function parseGolemCliArgv(raw: string | undefined = process.env.GOLEM_CLI_ARGV_JSON): GolemCliConfig {
  if (raw === undefined || raw.trim() === "") {
    return { error: "Golem CLI configuration is missing (GOLEM_CLI_ARGV_JSON)" };
  }

  let value: unknown;
  try {
    value = JSON.parse(raw);
  } catch {
    return { error: "Golem CLI configuration is malformed (GOLEM_CLI_ARGV_JSON must be JSON)" };
  }
  if (!Array.isArray(value) || value.length === 0 || value.some((part) => typeof part !== "string" || part.trim() === "")) {
    return { error: "Golem CLI configuration is malformed (GOLEM_CLI_ARGV_JSON must be a nonempty argv array)" };
  }
  // Do not trim or split: whitespace in an argument is part of that argument.
  return { argv: value };
}

export function appendGolemToolArgs(argv: string[], endpoint: string, toolArgs: string[]): string[] {
  return [...argv, "--service", endpoint, "--json", ...toolArgs];
}
