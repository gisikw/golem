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
ssh-keygen -q -t ed25519 -N '' -f "$state/client"
cp "$state/client.pub" "$state/authorized_keys"
for _ in $(seq 1 20); do
  port=$(shuf -i 20000-50000 -n1)
  ss -ltn | grep -q ":$port " || break
done
cat >"$state/golemd.toml" <<EOF
name = "127.0.0.1"
[harnesses.claude]
models = []
[attach_ssh]
port = $port
host_key_path = "$state/host_key"
authorized_keys_path = "$state/authorized_keys"
EOF
GOLEM_CLAUDE_ARGV='["sh","-c","printf '\''SSH-SMOKE-MARKER\\n'\''; sleep 20"]' \
  "$state/golemd" --config "$state/golemd.toml" --state "$state/daemon" --allowed-cwd-roots "$state" --poll 100ms >"$state/golemd.log" 2>&1 &
pid=$!
socket="$state/daemon/golemd.sock"
for _ in $(seq 1 100); do [[ -S "$socket" && -f "$state/host_key" ]] && break; sleep 0.05; done
[[ -S "$socket" ]] || { cat "$state/golemd.log"; exit 1; }
endpoint="unix://$socket"
job=$("$state/golem" --service "$endpoint" --json dispatch --harness claude --cwd "$state" 'attach smoke')
id=$(sed -n 's/.*"id":"\([^"]*\)".*/\1/p' <<<"$job")
for _ in $(seq 1 100); do
  before=$("$state/golem" --service "$endpoint" --json status "$id")
  grep -q '"activation"' <<<"$before" && break
  sleep 0.05
done
grep -q '"state":"running"' <<<"$before"
grep -q '"terminal"' <<<"$before"
grep -q '"activation"' <<<"$before"
set +e
timeout 5 ssh -tt -p "$port" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -i "$state/client" "$id@127.0.0.1" </dev/null >"$state/attach.out" 2>&1
rc=$?
set -e
[[ "$rc" == 124 || "$rc" == 0 ]] || { cat "$state/attach.out"; cat "$state/golemd.log"; exit 1; }
grep -q 'SSH-SMOKE-MARKER' "$state/attach.out"
after=$("$state/golem" --service "$endpoint" --json status "$id")
grep -q '"state":"running"' <<<"$after"
[[ "$before" == "$after" ]] || {
  # Updated timestamps may move during idempotent observation; semantic state
  # and absence of settlement are the invariants attach must preserve.
  ! grep -q '"settlement"' <<<"$after"
}
echo 'SSH attach pane output: PASS'
echo 'attach preserved running/unsettled state: PASS'
echo 'attach smoke: PASS'
