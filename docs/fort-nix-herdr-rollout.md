# fort-nix rollout: Golem-owned private Herdr

This repository now contains the Golem side. Apply the service wiring in
`fort-nix` separately; do not restart the live `golemd` while sibling jobs are
running.

## Package and service changes

1. Track/build `.#full` (or at minimum `.#daemon`) from the Golem commit listed
   in the delivery. The daemon closure now contains pinned Herdr 0.8.2 and a
   profile-less `golem-herdr-shell`; its wrapper sets `GOLEM_HERDR` and
   `GOLEM_HERDR_SHELL` to immutable store paths.
2. Keep the existing service user, state path, Golem config path, provider
   credentials, and PATH additions. No separate Herdr systemd unit is wanted:
   Herdr is a foreground child of `golemd`.
3. Add the opt-in block to golemd's operator TOML:

   ```toml
   [herdr]
   # Optional when --state is /var/lib/golem; explicit here for auditability.
   root = "/var/lib/golem/herdr"
   session = "golem"
   server_startup_timeout = "15s"
   startup_timeout_ms = 60000
   reconcile_interval = "15s"

   [herdr.kinds]
   pi = "pi"
   ```

   Do not set `socket`, point at `~/.config/herdr`, run `herdr server` in a
   sibling unit, or use `herdr server stop` in `ExecStop`. Golemd derives the
   socket from its private root and signals only its own child PID.
4. Do not manually provision `pi_extension` for the normal Nix deployment.
   On first startup golemd runs its bundled binary's `integration install pi`
   against `/var/lib/golem/herdr/pi-seed`, then copies the stable source into
   every job-private Pi profile. Existing seed bytes are intentionally not
   overwritten on restart.
5. The generated private Herdr config is
   `/var/lib/golem/herdr/config/herdr/config.toml`; do not manage it from Nix.
   Golemd regenerates it with `shell_mode = "non_login"`, the bundled no-rc
   shell, `resume_agents_on_restore = false`, and both update checks disabled.

## Safe rollout

Schedule a maintenance window when `golem list` has no running/blocked jobs.
Build and inspect first, without switching or restarting:

```sh
nix build /path/to/golem#full
./result/bin/golemd --help >/dev/null
./result/bin/herdr --version
```

Then update the tracked package/config and perform one ordinary service
restart. Verify:

```sh
systemctl status golemd
journalctl -u golemd -b | grep -E 'private Herdr started|run backend selected'
test -S /var/lib/golem/herdr/config/herdr/sessions/golem/herdr.sock
test -f /var/lib/golem/herdr/pi-seed/extensions/herdr-agent-state.ts
```

Dispatch a low-cost Pi canary, confirm its settings reference only
`.../artifacts/<job>/pi/extensions/herdr-agent-state.ts` (not the seed), then
cancel a second canary and verify its Herdr workspace disappears.

## Rollback and upgrade notes

Removing `[herdr]` and restarting returns to the unchanged tmux fallback.
Leave `/var/lib/golem/herdr` in place for a reversible rollback; remove it only
when no Herdr child is running and its persisted panes are no longer needed.

A hard golemd crash can leave the private Herdr child alive. The next daemon
will **refuse to attach to or kill it** and will fall back to tmux loudly.
During a maintenance window, identify the child from the prior golemd journal
(`private Herdr started pid=...`), verify its executable and socket are under
the Golem deployment/root, terminate that exact PID, and restart golemd. Never
use a broad process-name kill or an ambient `herdr server stop`.

When intentionally changing bundled Herdr lifecycle-extension bytes, drain
jobs, stop golemd, remove only `/var/lib/golem/herdr/pi-seed`, and start golemd
to reprovision it. This prevents extension behavior from changing silently
between concurrent jobs.
