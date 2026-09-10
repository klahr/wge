#!/bin/sh
set -eu

# The script holds a credential, so it is readable by the account that runs it
# and by nobody else. Everything under /usr is root-owned by default -- files
# placed there do not belong to a level unless a setup script says so, which is
# exactly why access control lives here and not in the manifest.
chown root:hleino /usr/local/bin/nightly-report
chmod 0750 /usr/local/bin/nightly-report

chmod 0640 /home/hleino/notes.md

# Somewhere for the report to land.
mkdir -p /var/catalogue
chown hleino:hleino /var/catalogue
chmod 0755 /var/catalogue
