#!/bin/sh
set -eu

# One directory deeper than people think matters.
chmod 0700 /home/oncall/.local /home/oncall/.local/share
chmod 0600 /home/oncall/.local/share/pass-notes.txt
