#!/usr/bin/env bash
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
state=$(mktemp -d)
pid=
cleanup() {
  if [[ -n "$pid" ]]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi
  rm -rf "$state"
}
trap cleanup EXIT

go build -o "$state/golemd" ./cmd/golemd
go build -o "$state/golem" ./cmd/golem
project="$state/project"
git init -q "$project"
git -C "$project" config user.email smoke@example.invalid
git -C "$project" config user.name Smoke
printf 'initial\n' >"$project/README"
git -C "$project" add README
git -C "$project" commit -qm initial
cat >"$state/golemd.toml" <<EOF
name = "local"
clone_enabled = false
[providers.openai]
base_url = "https://api.openai.com/v1"
api_key_env = ""
[harnesses.fake]
models = []
[harnesses.pi]
models = ["openai/gpt-5.6"]
[projects.scratch]
path = "$project"
description = "Smoke project"
EOF
"$state/golemd" --config "$state/golemd.toml" --state "$state/state" --poll 100ms --linger 0 >"$state/golemd.log" 2>&1 &
pid=$!
socket="$state/state/golemd.sock"
for _ in $(seq 1 100); do [[ -S "$socket" ]] && break; sleep 0.05; done
[[ -S "$socket" ]] || { cat "$state/golemd.log"; echo "golemd socket did not appear" >&2; exit 1; }
endpoint="unix://$socket"

caps=$("$state/golem" --service "$endpoint" capabilities)
echo '--- capabilities ---'
echo "$caps"
grep -q '"name": "local"' <<<"$caps"
grep -q 'openai/gpt-5.6' <<<"$caps"
grep -q '"name": "scratch"' <<<"$caps"

job=$("$state/golem" --service "$endpoint" --json dispatch --harness fake --project scratch --worktree resume-key 'standalone smoke')
id=$(sed -n 's/.*"id":"\([^"]*\)".*/\1/p' <<<"$job")
[[ -n "$id" ]]
echo '--- dispatch ---'
echo "$job"
echo '--- await ---'
"$state/golem" --service "$endpoint" await --timeout 15s "$id"
echo '--- list ---'
"$state/golem" --service "$endpoint" list
state_value=$("$state/golem" --service "$endpoint" --json status "$id" | sed -n 's/.*"state":"\([^"]*\)".*/\1/p')
[[ "$state_value" == done ]]
workspace="$project/.golem/worktrees/resume-key"
printf 'continued\n' >"$workspace/resume-proof"
second=$("$state/golem" --service "$endpoint" --json dispatch --harness fake --project scratch --worktree resume-key 'standalone smoke reuse')
second_id=$(sed -n 's/.*"id":"\([^"]*\)".*/\1/p' <<<"$second")
[[ -n "$second_id" ]]
"$state/golem" --service "$endpoint" await --timeout 15s "$second_id"
[[ -f "$workspace/resume-proof" ]]
grep -q '"worktree":"resume-key"' <<<"$second"
echo 'workspace reuse: PASS'
echo 'standalone smoke: PASS'
