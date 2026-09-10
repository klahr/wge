#!/bin/sh
set -eu

chmod 0600 /home/sysop/graduated.txt

# The operator can read the auth log, which is the point of the account and
# the only way the last level's claim can be checked. A payoff that cites
# evidence the player cannot reach is a payoff they have to take on trust.
usermod -a -G adm sysop
