#!/bin/sh
# ARTEX supervisor startup script (Linux / macOS / Docker ENTRYPOINT)
#
# Usage:
#   ./start.sh                       Run in the foreground (Ctrl-C to stop)
#   nohup ./start.sh >artex.log 2>&1 &   Run persistently in the background
#   ./start.sh -addr :9000           Extra arguments are passed through to artex as-is
#
# It does only one thing: launch artex, and after the process exits, decide whether to relaunch based on the exit code.
#
#   0      User stopped normally     -> exit the loop
#   75     Program requested restart -> relaunch immediately ("one-click update" or "rollback" clicked in the UI)
#   other  Crash                     -> relaunch after backoff (1->2->4... up to 60 seconds)
#
# Downloading, SHA256 verification, and the binary swap are deliberately not done here: that logic would have to be written twice, for sh and bat,
# and it's exactly the part that can least afford to fail -- once a non-runnable binary is swapped in, this script would faithfully
# relaunch it over and over, leaving the user to rescue the machine by hand. So verification/swap all stay in Go (the selfupdate package),
# done by artex itself on startup, keeping the script dead-simple.
set -u

cd "$(dirname "$0")" || exit 1

BIN=./artex
[ -x "$BIN" ] || { echo "[artex] executable $BIN not found" >&2; exit 1; }

RESTART_CODE=75
MAX_DELAY=60

child=0
stopping=0

# Forward the stop signal to the artex binary.
#
# This is required under Docker: docker stop only sends SIGTERM to PID 1 (i.e. this script),
# not to child processes. Without forwarding, artex never receives the signal and can't shut down gracefully, so after 10 seconds it gets
# hard-killed by SIGKILL, cutting off running tasks mid-way.
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

	# A signal interrupts wait and makes it return >128. At this point the child is actually still shutting down gracefully,
	# so we must wait once more to get its real exit code.
	wait "$child"
	code=$?
	if [ "$code" -gt 128 ]; then
		wait "$child"
		code=$?
	fi
	child=0

	if [ "$stopping" -eq 1 ]; then
		echo "[artex] stopped"
		exit 0
	fi

	case "$code" in
		0)
			echo "[artex] exited normally"
			exit 0
			;;
		"$RESTART_CODE")
			# Update/rollback is ready: after relaunch, artex completes the binary swap on startup (see selfupdate.Bootstrap).
			echo "[artex] restart requested (applying new version)..."
			delay=1
			;;
		*)
			echo "[artex] abnormal exit (code=$code), restarting in ${delay}s" >&2
			sleep "$delay"
			delay=$((delay * 2))
			[ "$delay" -gt "$MAX_DELAY" ] && delay=$MAX_DELAY
			;;
	esac
done
