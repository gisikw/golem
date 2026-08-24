# Familiar contribution

This directory is Golem's small, versioned integration surface for a Familiar
host. It is not a plugin SDK. `plugin.toml` requires exactly `familiar_api = 1`,
starts the existing Golem service and supervisor plus a semantic render adapter,
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
`GOLEM_RENDER_LISTEN`, and other existing Golem environment variables retain
their normal meanings.

## Host-rendered tree and invalidation

`GET /v1/render` returns render API 1, a revision, TTL, the exact v1 placement
target `left-nav`, and a semantic `content` tree made only of `tree`, `branch`,
and `item` nodes. Golem supplies stable ids, labels, open status strings, and
optional typed terminal activations. Familiar exclusively owns pixels, layout,
colors, and input handling. V1 implements no other target or target negotiation;
a host rejects unknown targets.

```json
{"render_api":1,"revision":7,"ttl_ms":30000,"target":"left-nav","content":{"kind":"tree","id":"golem:jobs","children":[{"kind":"branch","id":"workspace:alpha","label":"alpha","children":[{"kind":"item","id":"job:job-123","label":"Fix sidebar","status":"running","activation":{"type":"terminal","socket":"/state/golem/tmux.sock","session":"worker-job-123"}}]}]}}
```

The `ttl_ms` value is the anticipated cache expiry; Familiar refetches the
render at expiry. At plugin boot Familiar supplies a scoped callback URL in
`FAMILIAR_RENDER_INVALIDATE_URL`. If the semantic tree changes while Familiar
may still hold a cached render, Golem sends one empty `POST` to that URL.
Familiar marks the Golem render stale and immediately refetches `/v1/render`,
fanning it out to active renderers as needed. Changes coalesce until that
refetch. Callback failure is non-fatal and TTL expiry remains the fallback. The
adapter may internally poll the existing Golem job service because that service
has no cross-process notification hook.

## Terminal boundary

Terminal activation targets are direct `{type, socket, session}` tmux
coordinates. Familiar and Golem must run as the same trusted user on the same
host, with `tmux` available. The adapter verifies that a session is live before
publishing `activation`; dead or unpublished terminals remain non-selectable.
Remote PTY transport is intentionally out of scope.
