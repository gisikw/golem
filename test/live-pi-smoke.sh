#!/usr/bin/env bash
set -euo pipefail
if [[ "${GOLEM_LIVE_SMOKE:-}" != 1 ]]; then
  echo 'live pi smoke: SKIP (set GOLEM_LIVE_SMOKE=1)'
  exit 0
fi
: "${GOLEM_LIVE_BASE_URL:?GOLEM_LIVE_BASE_URL is required}"
model=${GOLEM_LIVE_MODEL:-llama/Qwen3.8-27B-UD-Q4_K_XL}
provider=${model%%/*}
state=$(mktemp -d)
pid=
cleanup() { [[ -z "$pid" ]] || { kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; }; rm -rf "$state"; }
trap cleanup EXIT
project="$state/project"
git init -q "$project"
git -C "$project" config user.email smoke@example.invalid
git -C "$project" config user.name Smoke
echo live >"$project/README"; git -C "$project" add README; git -C "$project" commit -qm initial
key_env=""
if [[ -n "${GOLEM_LIVE_API_KEY:-}" ]]; then export GOLEM_LIVE_PROVISION_KEY="$GOLEM_LIVE_API_KEY"; key_env=GOLEM_LIVE_PROVISION_KEY; fi
cat >"$state/config.toml" <<EOF
name = "live-smoke"
[providers.$provider]
base_url = "$GOLEM_LIVE_BASE_URL"
api_key_env = "$key_env"
[harnesses.pi]
models = ["$model"]
[projects.live]
path = "$project"
EOF
nix develop -c go build -o "$state/golemd" ./cmd/golemd
nix develop -c go build -o "$state/golem" ./cmd/golem
"$state/golemd" --config "$state/config.toml" --state "$state/state" --poll 250ms >"$state/daemon.log" 2>&1 & pid=$!
for _ in $(seq 1 200); do [[ -S "$state/state/golemd.sock" ]] && break; sleep .05; done
endpoint="unix://$state/state/golemd.sock"
job=$("$state/golem" --service "$endpoint" --json dispatch --harness pi --model "$model" --project live --worktree smoke 'Reply with exactly: GOLEM_LIVE_OK')
id=$(sed -n 's/.*"id":"\([^"]*\)".*/\1/p' <<<"$job")
"$state/golem" --service "$endpoint" --json await --timeout 5m "$id" | tee "$state/result.json"
grep -Eq 'GOLEM_LIVE_OK|"last_progress"|"settlement"' "$state/result.json" || { cat "$state/daemon.log"; echo 'worker produced no observable output' >&2; exit 1; }
echo 'live pi smoke: PASS'
