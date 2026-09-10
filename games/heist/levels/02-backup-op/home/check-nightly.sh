#!/bin/sh
# Ran by the agent, not by hand. Checks last night's run actually produced
# something before the retention job eats the previous one.
set -eu

latest=$(ls -1t /var/backups/nightly*.tar.gz 2>/dev/null | head -1)
if [ -z "$latest" ]; then
	echo "no nightly tarball found" >&2
	exit 1
fi

tar tzf "$latest" | wc -l
