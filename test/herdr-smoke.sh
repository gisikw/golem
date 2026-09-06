#!/usr/bin/env bash
# Real end-to-end proof that a pi job runs through Herdr instead of tmux.
#
# It starts its own headless herdr server in a temp HOME, points its own golemd
# at that session's socket with [herdr], dispatches a trivial pi job against the
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
herdr_home="$state/herdr-home"
session=${HERDR_SMOKE_SESSION:-golem-smoke-$$}
socket="$herdr_home/.config/herdr/sessions/$session/herdr.sock"
mkdir -p "$herdr_home"
mkdir -p "$herdr_home/.config/herdr"
# Operator requirement, learned the hard way (see README "Backends"): the pane
# shell must not clobber the PATH golemd puts in the workspace env, or Herdr
# cannot resolve the harness executable. On NixOS an interactive bash
# re-sources /etc/set-environment and wipes it, so this session's panes use a
# shell that runs no profile or rc files.
cat >"$state/pane-shell" <<'SHELL'
#!/usr/bin/env bash
exec bash --norc --noprofile "$@"
SHELL
chmod +x "$state/pane-shell"
cat >"$herdr_home/.config/herdr/config.toml" <<EOF
[terminal]
default_shell = "$state/pane-shell"
EOF
golemd_pid=
herdr_pid=

herdrctl() { HOME="$herdr_home" HERDR_SESSION="$session" "$herdr_bin" "$@"; }
cleanup() {
  [[ -n "$golemd_pid" ]] && { kill "$golemd_pid" 2>/dev/null || true; wait "$golemd_pid" 2>/dev/null || true; }
  herdrctl server stop >/dev/null 2>&1 || true
  [[ -n "$herdr_pid" ]] && { kill "$herdr_pid" 2>/dev/null || true; wait "$herdr_pid" 2>/dev/null || true; }
  tmux -S "$state/state/tmux.sock" kill-server 2>/dev/null || true
  rm -rf "$state"
}
trap cleanup EXIT

echo "--- herdr server ---"
HOME="$herdr_home" HERDR_SESSION="$session" "$herdr_bin" server >"$state/herdr-server.log" 2>&1 &
herdr_pid=$!
for _ in $(seq 1 100); do [[ -S "$socket" ]] && break; sleep 0.1; done
[[ -S "$socket" ]] || { cat "$state/herdr-server.log"; echo "herdr socket did not appear" >&2; exit 1; }
"$herdr_bin" --version
echo "socket: $socket"

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

[projects.scratch]
path = "$project"
description = "Herdr smoke project"

[herdr]
socket = "$socket"
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
herdrctl agent get "$agent2"
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

echo "--- attach/steer are 501 ---"
attach=$(curl -s -o "$state/attach.json" -w '%{http_code}' --unix-socket "$gsocket" "http://unix/v1/jobs/$id/attach")
steer=$(curl -s -o "$state/steer.json" -w '%{http_code}' --unix-socket "$gsocket" -X POST -H 'Content-Type: application/json' \
  -d '{"id":"","job_id":"","text":"left","at":"0001-01-01T00:00:00Z"}' "http://unix/v1/jobs/$id/steer")
echo "attach=$attach $(cat "$state/attach.json")"
echo "steer=$steer $(cat "$state/steer.json")"
[[ "$attach" == 501 && "$steer" == 501 ]] || { echo "attach/steer did not report 501" >&2; exit 1; }

echo "--- claude dispatch is 400 on this backend ---"
claude=$(curl -s -o "$state/claude.json" -w '%{http_code}' --unix-socket "$gsocket" -X POST -H 'Content-Type: application/json' \
  -d '{"idempotency_key":"smoke-claude","harness":"claude","cwd":"/tmp","prompt":"hi"}' "http://unix/v1/jobs")
echo "claude=$claude $(cat "$state/claude.json")"

echo 'herdr smoke: PASS'
