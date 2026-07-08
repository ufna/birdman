#!/usr/bin/env bash
# Smoke: the example game (sdk/example, C++ on birdman_core) against the Go
# mockagent (sdk/mockagent) over a real unix socket — full managed cycle
# ready -> allocate -> match_start -> match_end -> clean exit 0.
#
# Usage: smoke.sh <birdman_example_binary> <mockagent_module_dir>
# Registered as ctest `sdk.smoke` when a Go toolchain is available.
set -euo pipefail

EXAMPLE_BIN=$1
MOCKAGENT_DIR=$2

# Short base dir: sockaddr_un sun_path is ~104 bytes on macOS.
BASE="${TMPDIR:-/tmp}"
[ ${#BASE} -gt 60 ] && BASE=/tmp
DIR=$(mktemp -d "${BASE%/}/birdsmoke.XXXXXX")
SOCK="$DIR/agent.sock"
PORT=$((20000 + RANDOM % 20000))

GAME_PID=""
AGENT_PID=""
cleanup() {
    [ -n "$GAME_PID" ] && kill "$GAME_PID" 2>/dev/null || true
    [ -n "$AGENT_PID" ] && kill "$AGENT_PID" 2>/dev/null || true
    exec 9>&- 2>/dev/null || true
    rm -rf "$DIR"
}
trap cleanup EXIT

fail() {
    echo "SMOKE FAIL: $1" >&2
    echo "--- mockagent log ---" >&2; cat "$DIR/agent.log" >&2 || true
    echo "--- game log ---" >&2; cat "$DIR/game.log" >&2 || true
    exit 1
}

# Waits until a pattern shows up in a log (10s budget).
wait_log() { # <file> <pattern>
    for _ in $(seq 1 100); do
        grep -q "$2" "$1" 2>/dev/null && return 0
        sleep 0.1
    done
    fail "timeout waiting for \"$2\" in $1"
}

echo "smoke: building mockagent"
(cd "$MOCKAGENT_DIR" && go build -o "$DIR/mockagent" .) || fail "mockagent build failed"

echo "smoke: starting mockagent on $SOCK"
mkfifo "$DIR/cmd"
"$DIR/mockagent" -socket "$SOCK" -ping 2s <"$DIR/cmd" >"$DIR/agent.log" 2>&1 &
AGENT_PID=$!
exec 9>"$DIR/cmd" # keep the command pipe open

echo "smoke: starting example game on udp :$PORT"
BIRDMAN_SOCKET="$SOCK" BIRDMAN_SERVER_ID=smoke-1 BIRDMAN_PORT="$PORT" BIRDMAN_SDK_DEBUG=1 \
    "$EXAMPLE_BIN" >"$DIR/game.log" 2>&1 &
GAME_PID=$!

wait_log "$DIR/agent.log" "<- hello"
wait_log "$DIR/agent.log" "<- ready"

echo "smoke: allocating match m-smoke"
echo "allocate m-smoke 1" >&9
wait_log "$DIR/game.log" "allocated: match_id=m-smoke"

echo "smoke: keepalive (ping -> pong)"
echo "ping" >&9 # deterministic: the match may finish before the periodic ping
wait_log "$DIR/agent.log" "<- pong"

echo "smoke: driving a player through the match (JOIN/LEAVE)"
"$EXAMPLE_BIN" --client "$PORT" || fail "udp client cycle failed"

wait_log "$DIR/agent.log" '<- match_start.*m-smoke'
wait_log "$DIR/agent.log" '<- match_end.*m-smoke.*completed'

# One-shot dedicated server: the game must exit 0 by itself after match_end.
GAME_RC=-1
for _ in $(seq 1 100); do
    if ! kill -0 "$GAME_PID" 2>/dev/null; then
        wait "$GAME_PID" && GAME_RC=0 || GAME_RC=$?
        break
    fi
    sleep 0.1
done
GAME_PID=""
[ "$GAME_RC" = 0 ] || fail "game did not exit 0 after match_end (rc=$GAME_RC)"

for expected in "<- hello" "<- ready" "<- players" "<- match_start" "<- match_end" "<- pong"; do
    grep -q "$expected" "$DIR/agent.log" || fail "mockagent never saw \"$expected\""
done

echo "SMOKE OK: ready -> allocate -> match_start -> match_end -> exit 0"
