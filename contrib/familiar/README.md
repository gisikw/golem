# Familiar contribution

This directory is Golem's small, versioned integration surface for a Familiar
host. It is not a plugin SDK. `plugin.toml` requires exactly `familiar_api = 1`,
starts the existing Golem service and supervisor plus a nav projection adapter,
and loads the Presence-side `agents_*` Pi tools.

## Enrollment

For development, enroll an operator-trusted mutable checkout by local path:

```toml
[plugins.golem]
path = "/absolute/path/to/golem"
```

For a reproducible deployment, enroll a repository at one immutable 40-character
Git commit:

```toml
[plugins.golem]
git = "https://example.invalid/operator/golem.git"
rev = "0123456789abcdef0123456789abcdef01234567"
```

The Familiar host owns source checkout, exact API validation, service lifecycle,
and `${plugin_root}` expansion. No other manifest interpolation is required.
Plugin service environment is passed through verbatim. At minimum set
`GOLEM_DB`, `GOLEM_SOCKET`, `GOLEM_SUPERVISOR_STATE`, and `GOLEM_HOST` as needed.
Set `GOLEM_WEB_EXTENSION` to the host's generic Pi web extension path; Golem does
not copy or own that Familiar code. The host continues to load that extension
for Presence independently. `GOLEM_ENDPOINT`, `GOLEM_LISTEN`,
`GOLEM_NAV_LISTEN`, and other existing Golem environment variables retain their
normal meanings.

## Terminal boundary

Nav attach targets are direct `{socket, session}` tmux coordinates. Familiar and
Golem must run as the same trusted user on the same host, with `tmux` available.
The adapter verifies that a session is live before publishing `attach`; dead or
unpublished terminals remain non-clickable. Remote PTY transport is intentionally
out of scope.
