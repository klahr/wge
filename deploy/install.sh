#!/bin/sh
# Install wge as a system service.
#
# Idempotent: run it again to upgrade the binary and the unit. It never
# overwrites configuration or game content that is already there, and it does
# not start anything -- the images have to be built first, and `serve` refuses
# to run without them.
set -eu

PREFIX=${PREFIX:-/usr/local}
STATE=${STATE:-/var/lib/wge}
CONFIG=${CONFIG:-/etc/wge}
USER=${WGE_USER:-wge}

here=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
binary=${WGE_BINARY:-}

if [ -z "$binary" ]; then
	for candidate in "$here/dist/wge" "$here/bin/wge"; do
		[ -x "$candidate" ] && binary=$candidate && break
	done
fi
if [ -z "$binary" ] || [ ! -x "$binary" ]; then
	echo "no wge binary found; run 'make dist' first, or set WGE_BINARY" >&2
	exit 1
fi

[ "$(id -u)" = 0 ] || { echo "run this as root" >&2; exit 1; }

# The account the engine runs as. It needs the docker group, and membership of
# that group is root on this host by another name -- which is the reason to run
# the engine as its own user rather than as one somebody logs in with.
if ! getent group docker >/dev/null; then
	echo "there is no docker group; install Docker first" >&2
	exit 1
fi
if ! id "$USER" >/dev/null 2>&1; then
	echo "creating system user $USER"
	useradd --system --home-dir "$STATE" --shell /usr/sbin/nologin "$USER"
fi
if ! id -nG "$USER" | tr ' ' '\n' | grep -qx docker; then
	echo "adding $USER to the docker group"
	usermod -a -G docker "$USER"
fi

echo "installing $binary -> $PREFIX/bin/wge"
install -D -m 0755 "$binary" "$PREFIX/bin/wge"

install -d -m 0755 "$CONFIG"
install -d -m 0750 -o "$USER" -g "$USER" "$STATE"

# Configuration is never overwritten: an upgrade that resets somebody's
# settings is an upgrade that takes their service down.
if [ -f "$CONFIG/wge.env" ]; then
	echo "keeping $CONFIG/wge.env (compare against deploy/wge.env for new settings)"
else
	echo "installing $CONFIG/wge.env"
	install -m 0640 -g "$USER" "$here/deploy/wge.env" "$CONFIG/wge.env"
fi

# Nor is game content, which is the operator's, not the package's.
for dir in games bases; do
	if [ -d "$STATE/$dir" ]; then
		echo "keeping $STATE/$dir"
	elif [ -d "$here/$dir" ]; then
		echo "installing $STATE/$dir"
		cp -r "$here/$dir" "$STATE/$dir"
		chown -R "$USER:$USER" "$STATE/$dir"
	fi
done

echo "installing /etc/systemd/system/wge.service"
install -D -m 0644 "$here/deploy/wge.service" /etc/systemd/system/wge.service
systemctl daemon-reload

cat <<NEXT

Installed. The service is not started yet, because it will refuse to run
until the game images exist.

  sudo -u $USER $PREFIX/bin/wge base  $STATE/bases/debian-13
  sudo -u $USER $PREFIX/bin/wge build $STATE/games/demo
  sudo -u $USER $PREFIX/bin/wge test  $STATE/games/demo

  systemctl enable --now wge
  sudo -u $USER $PREFIX/bin/wge invite

Settings live in $CONFIG/wge.env. Back up $STATE/wge.db: it holds the run
salts, and they are the only thing here that cannot be rebuilt.
NEXT
