#!/bin/bash
# Supervised worker used by the descendant-stop tests.
# argv: <genfile> <mode>
# Modes:
#   stay-setsid  double-fork into a new session, then the parent stays up
#   exit-setsid  same, then the parent exits once the descendant has recorded its pid
#   stay-bg      shell background job (no new session), parent stays up
#   exit-bg      shell background job, parent exits once the job has recorded its pid
# The descendant ignores TERM/INT/HUP. Only an explicit SIGKILL, a cgroup kill,
# or the death of a private pid-namespace init will stop it.
genfile=$1
mode=$2

record_and_sleep() {
	trap '' TERM INT HUP
	echo $$ >> "$genfile"
	while true; do
		sleep 3600
	done
}

wait_for_record() {
	i=0
	while [ "$i" -lt 200 ]; do
		if [ -s "$genfile" ]; then
			return 0
		fi
		i=$((i + 1))
		sleep 0.01
	done
	return 0
}

case "$mode" in
stay-setsid|exit-setsid)
	if [ "${WORKER:-}" != 1 ]; then
		WORKER=1 setsid "$0" "$genfile" "$mode" </dev/null >/dev/null 2>&1 &
		wait_for_record
		if [ "$mode" = exit-setsid ]; then
			exit 0
		fi
		while true; do
			sleep 3600
		done
	fi
	record_and_sleep
	;;
stay-bg|exit-bg)
	trap '' TERM INT HUP
	(
		trap '' TERM INT HUP
		echo "$BASHPID" >> "$genfile"
		while true; do
			sleep 3600
		done
	) &
	wait_for_record
	if [ "$mode" = exit-bg ]; then
		exit 0
	fi
	while true; do
		sleep 3600
	done
	;;
*)
	echo "unknown mode: $mode" >&2
	exit 2
	;;
esac
