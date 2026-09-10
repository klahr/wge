#!/bin/sh
set -eu

# The shell writes history mode 0600, and the account is shared, so this is
# readable by exactly the people who share it -- which is the point.
chmod 0600 /home/kiosk/.bash_history
chmod 0644 /home/kiosk/README.txt
