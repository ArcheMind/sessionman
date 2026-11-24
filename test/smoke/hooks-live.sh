#!/bin/sh
# Live check that portable hooks from SM_CONFIG_DIR reach the real agents on the headless
# execution path of `sm session continue` (Claude stream-json, Codex app-server, Pi RPC),
# on a PATH holding only sm and the native CLIs.
set -eu

ax=${1:-./bin/sm}
case $ax in /*) ;; *) ax=$(pwd)/$ax ;; esac
root=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
recorder=$root/test/smoke/record-hook.sh
workspace=$(mktemp -d "${TMPDIR:-/tmp}/sessionman-hooks-smoke-XXXXXX")
workspace=$(CDPATH= cd -- "$workspace" && pwd -P)
project=$workspace/project
results=$workspace/results
native=$workspace/native-bin
mkdir -p "$project" "$results" "$native"
trap 'printf "Hook smoke evidence: %s\n" "$results"' EXIT HUP INT TERM

for agent in claude codex pi; do
	if ! command -v "$agent" >/dev/null 2>&1; then
		"$ax" agent install "$agent"
	fi
done

ln -s "$ax" "$native/sm"
for tool in claude codex pi node; do
	ln -s "$(command -v "$tool")" "$native/$tool"
done
native_path=$native:/usr/bin:/bin:/usr/sbin:/sbin
if PATH=$native_path command -v claude-agent-acp >/dev/null 2>&1 || PATH=$native_path command -v codex-acp >/dev/null 2>&1; then
	printf 'an ACP wrapper is still reachable on the native-only PATH\n' >&2
	exit 1
fi

require_login() {
	agent=$1
	if ! "$ax" --json auth status "$agent" | node -e '
let data = "";
process.stdin.on("data", chunk => data += chunk);
process.stdin.on("end", () => process.exit(JSON.parse(data).providers.length > 0 ? 0 : 1));
'; then
		printf '%s is not logged in. Run: %s auth login %s\n' "$agent" "$ax" "$agent" >&2
		exit 1
	fi
}

require_login claude
require_login codex
require_login pi

transcript=$(printf '{"id":"hooks-smoke","provider":"smoke","workspace":"%s","messages":[{"id":"u1","role":"user","content":[{"type":"text","text":"Reply with exactly OK."}]}]}' "$project")
failures=

run_case() {
	agent=$1
	model=$2
	agent_results=$results/$agent
	config=$workspace/config-$agent
	mkdir -p "$agent_results" "$config"
	node "$root/test/smoke/make-hooks.mjs" "$config/hooks.json" "$recorder" "$agent_results"
	if printf '%s\n' "$transcript" | env PATH="$native_path" SM_CONFIG_DIR="$config" "$ax" session continue "$agent" --model "$model" >"$agent_results/stdout.log" 2>"$agent_results/stderr.log" &&
		node "$root/test/smoke/assert-events.mjs" "$agent_results/events.jsonl" "$agent"; then
		return
	fi
	printf '%s smoke failed; continuing with the remaining agents.\n' "$agent" >&2
	failures="${failures}${failures:+, }${agent}"
}

run_case claude haiku
run_case codex gpt-6-luna
run_case pi "${SM_HOOKS_PI_MODEL:-openai-codex/gpt-5.6-luna}"

if [ -n "$failures" ]; then
	printf 'Hook smoke failures: %s\n' "$failures" >&2
	exit 1
fi
