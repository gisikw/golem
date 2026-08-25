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
token="standalone-smoke-token"
# Isolated smoke invocations use a high per-process loopback port.
port=$((20000 + $$ % 20000))
cat >"$state/golemd.toml" <<EOF
name = "local"
clone_enabled = false
api_bearer_tokens = ["$token"]
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
"$state/golemd" --config "$state/golemd.toml" --state "$state/state" --listen "127.0.0.1:$port" --poll 100ms --linger 0 >"$state/golemd.log" 2>&1 &
pid=$!
socket="$state/state/golemd.sock"
for _ in $(seq 1 100); do [[ -S "$socket" ]] && break; sleep 0.05; done
[[ -S "$socket" ]] || { cat "$state/golemd.log"; echo "golemd socket did not appear" >&2; exit 1; }
"$state/golem" --service "unix://$socket" capabilities >/dev/null
echo 'tokenless Unix request exempt: PASS'
endpoint="http://127.0.0.1:$port"
for _ in $(seq 1 100); do curl -sf -H "Authorization: Bearer $token" "$endpoint/live" >/dev/null && break; sleep 0.05; done
status_code=$(curl -s -o "$state/unauthorized.json" -w '%{http_code}' "$endpoint/live")
[[ "$status_code" == 401 ]]
grep -q 'bearer token required' "$state/unauthorized.json"
echo 'tokenless TCP request rejected: PASS'

cli=("$state/golem" --service "$endpoint" --token "$token")
caps=$("${cli[@]}" capabilities)
echo '--- capabilities ---'
echo "$caps"
grep -q '"name": "local"' <<<"$caps"
grep -q 'openai/gpt-5.6' <<<"$caps"
grep -q '"name": "scratch"' <<<"$caps"

job=$("${cli[@]}" --json dispatch --harness fake --project scratch --worktree resume-key 'standalone smoke')
id=$(sed -n 's/.*"id":"\([^"]*\)".*/\1/p' <<<"$job")
[[ -n "$id" ]]
echo '--- dispatch ---'
echo "$job"
echo '--- await ---'
"${cli[@]}" await --timeout 15s "$id"
echo '--- list ---'
"${cli[@]}" list
status=$("${cli[@]}" --json status "$id")
state_value=$(sed -n 's/.*"state":"\([^"]*\)".*/\1/p' <<<"$status")
[[ "$state_value" == done ]]
grep -q '"settlement"' <<<"$status"
grep -q '"exit_status":0' <<<"$status"
set +e
timeout 2 "${cli[@]}" events --since 0 --job "$id" >"$state/events.jsonl"
events_rc=$?
set -e
[[ "$events_rc" == 124 ]]
grep -q '"kind":"job.created"' "$state/events.jsonl"
grep -q '"kind":"job.state"' "$state/events.jsonl"
grep -q '"kind":"job.settled"' "$state/events.jsonl"
echo 'settlement + lifecycle replay: PASS'
"${cli[@]}" artifacts "$id" >"$state/artifacts.list"
grep -q 'result.txt' "$state/artifacts.list"
"${cli[@]}" artifacts "$id" result.txt -o "$state/fetched.txt"
grep -qx 'fake-artifact' "$state/fetched.txt"
echo 'authenticated artifact list + fetch: PASS'
workspace="$project/.golem/worktrees/resume-key"
printf 'continued\n' >"$workspace/resume-proof"
second=$("${cli[@]}" --json dispatch --harness fake --project scratch --worktree resume-key 'standalone smoke reuse')
second_id=$(sed -n 's/.*"id":"\([^"]*\)".*/\1/p' <<<"$second")
[[ -n "$second_id" ]]
"${cli[@]}" await --timeout 15s "$second_id"
[[ -f "$workspace/resume-proof" ]]
grep -q '"worktree":"resume-key"' <<<"$second"
echo 'workspace reuse: PASS'
echo 'standalone smoke: PASS'
