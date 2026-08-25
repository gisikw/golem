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
"$state/golemd" --config "$repo/golemd.example.toml" --state "$state/state" --poll 100ms --linger 0 >"$state/golemd.log" 2>&1 &
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

job=$("$state/golem" --service "$endpoint" --json dispatch --harness fake --cwd /tmp 'standalone smoke')
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
echo 'standalone smoke: PASS'
