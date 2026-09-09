#!/bin/sh
# Machine configuration, for the things that belong to no single level.
set -eu

# Postfix ships expecting to chroot each daemon into /var/spool/postfix, which
# needs device nodes and a copied resolver that a container does not have. Left
# alone it fails on every pickup and fills syslog with throttling warnings --
# a mail relay whose own MTA is visibly broken is worse than no MTA at all.
postconf -e "myhostname = relay2.ardent-freight.se"
postconf -e "mydomain = ardent-freight.se"
postconf -e "myorigin = \$mydomain"
postconf -e "mydestination = \$myhostname, localhost.\$mydomain, localhost, \$mydomain"
postconf -e "inet_interfaces = loopback-only"
postconf -e "inet_protocols = ipv4"
postconf -F '*/*/chroot=n'

# The spool has to be laid out and owned the way postfix expects before it will
# start; the image build has been moving ownership around underneath it.
postfix set-permissions >/dev/null 2>&1 || true
newaliases >/dev/null 2>&1 || true
