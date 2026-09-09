#!/bin/sh
set -eu

# The mailroom account sees the spool directory but not other people's mail.
# /var/mail is 2775 root:mail; individual boxes are 0660 and owned by their
# reader, which is what stops level one from simply reading level four's post.
chmod 0640 /home/jposti/notes.txt
