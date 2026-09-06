#!/usr/bin/env bash
# Real end-to-end proof that a pi job runs through Herdr instead of tmux.
#
# Its disposable golemd starts and owns a private headless Herdr child, then
# dispatches a trivial pi job against the
# cheap local model, follows status through to done, then dispatches a second
# job and cancels it — asserting the agent and workspace are verifiably gone,
# that golemd's private tmux server has no sessions for either job, and that
# herdr shows exactly one workspace per live job.
#
# It never touches this host's running golemd (/var/lib/golem/golemd.sock) or
# any existing herdr session.
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
cd "$repo"

herdr_bin=${HERDR_BIN:-$(command -v herdr || true)}
if [[ -z "$herdr_bin" ]]; then
  herdr_bin=$(ls -d /nix/store/*herdr*/bin/herdr 2>/dev/null | head -1 || true)
fi
[[ -n "$herdr_bin" && -x "$herdr_bin" ]] || { echo "herdr binary not found; set HERDR_BIN" >&2; exit 1; }
command -v pi >/dev/null || { echo "pi binary not on PATH" >&2; exit 1; }

model=${GOLEM_SMOKE_MODEL:-tiamat-openai-llama-frankenstein/Qwen3.8-27B-UD-Q4_K_XL}
provider=${model%%/*}
state=$(mktemp -d)
# Keep the session socket below Unix sockaddr_un's ~104-108 byte limit even
# when nix develop gives $state a long prefix.
herdr_root=$(mktemp -d /tmp/golem-herdr-smoke-XXXX)
session=${HERDR_SMOKE_SESSION:-golem-smoke-$$}
socket="$herdr_root/config/herdr/sessions/$session/herdr.sock"
seed_extension="$herdr_root/pi-seed/extensions/herdr-agent-state.ts"
# Exercise the explicit profile-less shell override used by non-Nix operators.
cat >"$state/pane-shell" <<'SHELL'
#!/usr/bin/env bash
exec bash --norc --noprofile "$@"
SHELL
chmod +x "$state/pane-shell"
golemd_pid=

herdrctl() {
  HOME="$herdr_root/home" XDG_CONFIG_HOME="$herdr_root/config" XDG_STATE_HOME="$herdr_root/state" \
    HERDR_CONFIG_PATH="$herdr_root/config/herdr/config.toml" HERDR_SESSION="$session" \
    HERDR_SOCKET_PATH="$socket" "$herdr_bin" --session "$session" "$@"
}
cleanup() {
  [[ -n "$golemd_pid" ]] && { kill "$golemd_pid" 2>/dev/null || true; wait "$golemd_pid" 2>/dev/null || true; }
  tmux -S "$state/state/tmux.sock" kill-server 2>/dev/null || true
  rm -rf "$state" "$herdr_root"
}
trap cleanup EXIT

"$herdr_bin" --version

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
name = "herdr-smoke"
clone_enabled = false
api_bearer_tokens = []

[providers.$provider]
kind = "${GOLEM_SMOKE_PROVIDER_KIND:-tiamat}"

[harnesses.pi]
models = ["$model"]

# Configured but unsupported by this substrate: dispatch must be a clear 400.
[harnesses.claude]
models = ["anthropic/claude-sonnet-4"]

[projects.scratch]
path = "$project"
description = "Herdr smoke project"

[herdr]
binary = "$herdr_bin"
root = "$herdr_root"
session = "$session"
shell = "$state/pane-shell"
server_startup_timeout = "15s"
reconcile_interval = "15s"
startup_timeout_ms = 120000

[herdr.kinds]
pi = "pi"
EOF
echo "--- golemd config ---"
cat "$state/golemd.toml"

"$state/golemd" --config "$state/golemd.toml" --state "$state/state" --poll 500ms --linger 0 >"$state/golemd.log" 2>&1 &
golemd_pid=$!
gsocket="$state/state/golemd.sock"
for _ in $(seq 1 200); do [[ -S "$gsocket" ]] && break; sleep 0.05; done
[[ -S "$gsocket" ]] || { cat "$state/golemd.log"; echo "golemd socket did not appear" >&2; exit 1; }
grep -q 'backend=herdr' "$state/golemd.log" || { cat "$state/golemd.log"; echo "golemd did not select the herdr backend" >&2; exit 1; }
grep 'run backend selected' "$state/golemd.log"
[[ -S "$socket" ]] || { cat "$state/golemd.log"; echo "golemd-owned Herdr socket did not appear" >&2; exit 1; }
[[ -f "$seed_extension" ]] || { cat "$state/golemd.log"; echo "golemd did not install stable Pi lifecycle seed" >&2; exit 1; }
echo "owned socket: $socket"

cli=("$state/golem" --service "unix://$gsocket")

echo "--- dispatch ---"
job=$("${cli[@]}" --json dispatch --harness pi --model "$model" --project scratch --worktree mvp \
  'Reply with exactly the word ACK and nothing else. Do not use any tools.')
echo "$job"
id=$(sed -n 's/.*"id":"\([^"]*\)".*/\1/p' <<<"$job")
[[ -n "$id" ]]

echo "--- herdr workspace list (one live job) ---"
for _ in $(seq 1 120); do
  herdrctl workspace list | grep -q "golem/$id" && break
  sleep 1
done
herdrctl workspace list
herdrctl workspace list | grep -q "golem/$id" || { echo "no herdr workspace for the job" >&2; exit 1; }
echo "--- private Herdr Pi lifecycle extension ---"
worker_profile="$state/state/artifacts/$id/pi"
worker_extension="$worker_profile/extensions/herdr-agent-state.ts"
settings="$worker_profile/settings.json"
seed_hash=$(sha256sum "$seed_extension" | cut -d' ' -f1)
worker_hash=$(sha256sum "$worker_extension" | cut -d' ' -f1)
[[ "$seed_hash" == "$worker_hash" ]] || { echo "worker extension bytes differ from seed" >&2; exit 1; }
[[ $(stat -c %a "$worker_extension") == 600 ]] || { echo "worker extension is not mode 0600" >&2; exit 1; }
grep -Fq "\"$worker_extension\"" "$settings" || { cat "$settings"; echo "private extension absent from settings allowlist" >&2; exit 1; }
if grep -Fq "\"$seed_extension\"" "$settings"; then cat "$settings"; echo "settings references shared seed" >&2; exit 1; fi
echo "seed:    $seed_extension"
echo "private: $worker_extension"
echo "--- herdr agent list ---"
herdrctl agent list

echo "--- status until settled ---"
deadline=$((SECONDS + ${GOLEM_SMOKE_TIMEOUT:-420}))
final=""
while ((SECONDS < deadline)); do
  status=$("${cli[@]}" --json status "$id")
  state_value=$(sed -n 's/.*"state":"\([^"]*\)".*/\1/p' <<<"$status")
  echo "state=$state_value"
  case "$state_value" in
    done | failed | cancelled | timeout)
      final=$state_value
      echo "$status"
      break
      ;;
  esac
  sleep 3
done
[[ "$final" == done ]] || { echo "job did not settle done (state=${final:-timeout-waiting})" >&2; "${cli[@]}" --json status "$id"; exit 1; }
echo "first job settled: $final"

echo "--- second dispatch + cancel ---"
job2=$("${cli[@]}" --json dispatch --harness pi --model "$model" --project scratch --worktree mvp2 \
  'Count slowly from 1 to 500, one number per line.')
id2=$(sed -n 's/.*"id":"\([^"]*\)".*/\1/p' <<<"$job2")
[[ -n "$id2" ]]
for _ in $(seq 1 120); do
  herdrctl workspace list | grep -q "golem/$id2" && break
  sleep 1
done
herdrctl workspace list | grep -q "golem/$id2" || { echo "no herdr workspace for the second job" >&2; exit 1; }
agent2="job-${id2#job-}"
agent2=${agent2:0:32}
# Cancel a genuinely live agent, not one still launching: wait for Herdr to
# report it interactive-ready first.
for _ in $(seq 1 120); do
  herdrctl agent get "$agent2" 2>/dev/null | grep -q '"interactive_ready":true' && break
  sleep 1
done
herdrctl agent get "$agent2"
herdrctl agent get "$agent2" | grep -q '"interactive_ready":true' || { echo "second agent never became interactive-ready" >&2; exit 1; }
"${cli[@]}" cancel "$id2" >/dev/null
for _ in $(seq 1 60); do
  state2=$(sed -n 's/.*"state":"\([^"]*\)".*/\1/p' <<<"$("${cli[@]}" --json status "$id2")")
  [[ "$state2" == cancelled ]] && break
  sleep 1
done
[[ "$state2" == cancelled ]] || { echo "second job not cancelled (state=$state2)" >&2; exit 1; }
echo "second job state: $state2"

echo "--- cancel verified gone ---"
if herdrctl agent get "$agent2" >/dev/null 2>&1; then echo "cancelled agent still present" >&2; exit 1; fi
herdrctl agent get "$agent2" 2>&1 || true
herdrctl workspace list
herdrctl workspace list | grep -q "golem/$id2" && { echo "cancelled workspace still present" >&2; exit 1; }

echo "--- tmux is absent from this path ---"
if [[ -S "$state/state/tmux.sock" ]]; then
  sessions=$(tmux -S "$state/state/tmux.sock" ls 2>&1 || true)
  echo "tmux ls: $sessions"
  grep -q "worker-" <<<"$sessions" && { echo "a worker tmux session exists on the herdr path" >&2; exit 1; }
else
  echo "tmux ls: no private tmux server was ever started"
fi

echo "--- attach/steer are 501, claude dispatch is 400 ---"
set +e
attach_out=$("${cli[@]}" attach "$id" 2>&1)
steer_out=$("${cli[@]}" steer "$id" go left 2>&1)
claude_out=$("${cli[@]}" dispatch --harness claude --model anthropic/claude-sonnet-4 --project scratch --worktree mvp3 'hi' 2>&1)
set -e
echo "attach: $attach_out"
echo "steer:  $steer_out"
echo "claude: $claude_out"
grep -q '501' <<<"$attach_out" || { echo "attach did not report 501" >&2; exit 1; }
grep -q '501' <<<"$steer_out" || { echo "steer did not report 501" >&2; exit 1; }
grep -q '400' <<<"$claude_out" || { echo "claude dispatch did not report 400" >&2; exit 1; }
grep -q 'not supported on herdr backend' <<<"$claude_out" || { echo "claude rejection message unclear" >&2; exit 1; }

echo '--- golemd owns Herdr cleanup ---'
herdr_pid=
for _ in $(seq 1 100); do
  herdr_pid=$(sed -n 's/.*private Herdr started.*pid=\([0-9]*\).*/\1/p' "$state/golemd.log" | tail -1)
  [[ -n "$herdr_pid" ]] && break
  sleep 0.05
done
[[ -n "$herdr_pid" ]] || { cat "$state/golemd.log"; echo "private Herdr pid not logged" >&2; exit 1; }
kill "$golemd_pid"
wait "$golemd_pid"
golemd_pid=
[[ ! -S "$socket" ]] || { echo "private Herdr socket survived golemd" >&2; exit 1; }
if kill -0 "$herdr_pid" 2>/dev/null; then echo "private Herdr process survived golemd" >&2; exit 1; fi

echo 'herdr smoke: PASS'
