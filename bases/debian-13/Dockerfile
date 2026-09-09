# The base image game images are built on.
#
# It is deliberately a plausible Debian box rather than a minimal one. A player
# who reaches for file(1), hexdump, ss or mail and finds it missing learns that
# the machine is a stage set; every tool here is one somebody would reasonably
# expect to have.
FROM debian:13-slim

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      bash coreutils util-linux passwd login procps psmisc \
      findutils grep sed gawk diffutils \
      tar gzip bzip2 xz-utils \
      less vim-tiny nano file bsdextrautils \
      iproute2 netcat-openbsd \
      s-nail openssh-client ca-certificates \
      wtmpdb lastlog2 \
      cron sudo man-db \
 && rm -rf /var/lib/apt/lists/*

# Mail is the narrative vehicle, so the spool has to behave like a spool:
# group-mail, setgid, and individual boxes readable only by their owner.
RUN mkdir -p /var/mail && chgrp mail /var/mail && chmod 2775 /var/mail

# Generated histories carry timestamps. Without this, `history` shows the
# commands but silently drops the times, and the shell writes new entries in a
# format that does not match what is already in the file.
RUN printf 'HISTTIMEFORMAT="%%F %%T "\nexport HISTTIMEFORMAT\n' \
      > /etc/profile.d/10-history.sh

# Players arrive by exec; PID 1 only has to keep the container alive.
CMD ["/bin/sleep", "infinity"]
