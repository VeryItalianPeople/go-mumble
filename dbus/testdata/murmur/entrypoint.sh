#!/bin/sh
set -eu

mkdir -p /run/murmur-bus /var/lib/murmur-test
# Murmur started as root drops to the package's mumble-server user by itself.
if id mumble-server >/dev/null 2>&1; then
	chown mumble-server: /var/lib/murmur-test
fi
dbus-daemon --config-file=/etc/murmur-test/bus.conf --nofork --nopidfile &

i=0
while [ ! -S /run/murmur-bus/socket ]; do
	i=$((i + 1))
	[ "$i" -lt 50 ] || { echo "dbus-daemon did not start" >&2; exit 1; }
	sleep 0.1
done

# 1.5 ships the binary as mumble-server, 1.3 as murmurd.
murmur=$(command -v mumble-server || command -v murmurd || true)
[ -n "$murmur" ] || murmur=/usr/sbin/murmurd
exec "$murmur" -fg -ini /etc/murmur-test/murmur.ini
