#!/bin/sh
set -eu

# The backup tree belongs to the operator and nobody else. If /var/backups were
# traversable by everyone, level one could read the on-call password without
# having earned the operator account first -- which is the whole level.
chmod 0750 /var/backups
chmod 0640 /var/backups/rota-notes.txt

# The nightly tarball is the operator's copy of somebody else's home directory.
# It is exactly as badly protected as the notes complain it is: readable by the
# operator, invisible to everyone below. What is inside it is another matter,
# and that is the point of it.
chmod 0640 /var/backups/nightly.tar.gz
