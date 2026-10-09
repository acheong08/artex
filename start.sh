#!/bin/sh
# ARTEX process supervisor (Linux / macOS / Docker ENTRYPOINT)
#
# Usage:
#   ./start.sh                       Run in the foreground (Ctrl-C to stop)
#   nohup ./start.sh >artex.log 2>&1 &   Run in the background
#   ./start.sh -addr :9000           Pass additional arguments through to artex
#
# It runs artex and uses its exit code to decide whether to restart it.
#
#   0      Normal user shutdown       → exit the loop
#   75     Restart requested by app   → restart immediately (e.g. update or rollback)
#   Other  Crash                      → restart with backoff (1→2→4…up to 60 seconds)
#
# Downloads, SHA256 checks, and binary replacement are intentionally handled by Go
# (the selfupdate package), not duplicated in shell and batch scripts. If a replacement
# binary cannot start, this supervisor would keep restarting it; artex therefore validates
# and installs updates during startup, keeping this script simple.
set -u

cd "$(dirname "$0")" || exit 1

BIN=./artex
[ -x "$BIN" ] || { echo "[artex] Executable not found: $BIN" >&2; exit 1; }

RESTART_CODE=75
MAX_DELAY=60

child=0
stopping=0

# Forward shutdown signals to artex.
#
# This is required in Docker: docker stop sends SIGTERM only to PID 1 (this script), not its
# child process. Without forwarding the signal, artex cannot shut down gracefully and is
# forcibly killed with SIGKILL after 10 seconds, interrupting any running tasks.
forward() {
	stopping=1
	if [ "$child" -ne 0 ]; then
		kill -TERM "$child" 2>/dev/null || true
	fi
}
trap forward INT TERM

delay=1
while :; do
	"$BIN" "$@" &
	child=$!

	# A signal interrupts wait and makes it return >128, while the child is still shutting down
	# gracefully. Wait again to get the child's actual exit code.
	wait "$child"
	code=$?
	if [ "$code" -gt 128 ]; then
		wait "$child"
		code=$?
	fi
	child=0

	if [ "$stopping" -eq 1 ]; then
		echo "[artex] Stopped"
		exit 0
	fi

	case "$code" in
		0)
			echo "[artex] Exited normally"
			exit 0
			;;
		"$RESTART_CODE")
			# Update/rollback is ready; artex will install it during the next startup.
			echo "[artex] Restart requested (applying update)…"
			delay=1
			;;
		*)
			echo "[artex] Exited unexpectedly (code=$code); restarting in ${delay}s" >&2
			sleep "$delay"
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done
