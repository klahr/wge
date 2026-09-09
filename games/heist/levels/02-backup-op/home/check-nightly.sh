#!/bin/sh
# Ran by the agent, not by hand. Checks last night's run actually produced
# something before the retention job eats the previous one.
set -eu

latest=$(ls -1dt /var/backups/restore-* 2>/dev/null | head -1)
if [ -z "$latest" ]; then
	echo "no restore staging found" >&2
	exit 1
fi

find "$latest" -type f | wc -l
