# The base image game images are built on.
#
# It is deliberately a plausible Debian box rather than a minimal one. A player
# who reaches for file(1), hexdump, ss or mail and finds it missing learns that
# the machine is a stage set; every tool here is one somebody would reasonably
# expect to have.
FROM debian:13-slim

ENV DEBIAN_FRONTEND=noninteractive

# sysvinit rather than systemd.
#
# This is a security decision before it is an aesthetic one. systemd will not
# boot in a container without CAP_SYS_ADMIN, and CAP_SYS_ADMIN is the single
# capability most likely to get a player out of the container -- which is not a
# trade worth making on a box we invite strangers to attack. sysvinit runs
# happily with none of that, and it is a real Debian init: /sbin/init is the
# genuine binary, /etc/init.d and service(8) behave exactly as a player expects,
# and init reaps orphans properly as PID 1.
#
# systemd is then purged outright. Leaving systemctl on a box that did not boot
# with systemd means leaving a tool that answers every question with an error,
# and a tool that cannot do its job is a tell.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      sysvinit-core cron init-system-helpers \
      bash coreutils util-linux passwd login procps psmisc \
      findutils grep sed gawk diffutils \
      tar gzip bzip2 xz-utils \
      less vim-tiny nano file bsdextrautils \
      iproute2 netcat-openbsd \
      bsd-mailx openssh-client ca-certificates \
      sudo man-db inetutils-syslogd \
 && apt-get purge -y --auto-remove systemd \
 && rm -rf /var/lib/apt/lists/* /var/log/apt /var/log/dpkg.log \
      /var/log/alternatives.log

# inetutils-syslogd rather than rsyslog, because Debian 13's rsyslog ships only
# a systemd unit and no init script -- on a sysvinit box it would be installed
# and permanently dead. The traditional syslogd is also the more plausible
# choice for a machine of this vintage, and its stock syslog.conf writes
# auth.log and syslog exactly where the aging pass puts their history.

# There are no consoles here, so the gettys in inittab would respawn and fail
# forever. Everything else in inittab is left as Debian ships it.
RUN sed -i '/getty/d' /etc/inittab

# Mail is the narrative vehicle, so the spool has to behave like a spool:
# group-mail, setgid, and individual boxes readable only by their owner.
RUN mkdir -p /var/mail && chgrp mail /var/mail && chmod 2775 /var/mail

# Generated histories carry timestamps. Without this, `history` shows the
# commands but silently drops the times, and the shell writes new entries in a
# format that does not match what is already in the file.
RUN printf 'HISTTIMEFORMAT="%%F %%T "\nexport HISTTIMEFORMAT\n' \
      > /etc/profile.d/10-history.sh

# A real init, so the box has a real process tree and services have somewhere
# to be started from.
CMD ["/sbin/init"]
