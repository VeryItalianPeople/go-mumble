#!/bin/sh
# Runs the integration tests against a real Murmur in a container.
#
#   scripts/integration-test.sh dbus [trixie|bookworm] [go test flags...]
#   scripts/integration-test.sh ice  [1.6|1.5]          [go test flags...]
#
# dbus builds a Debian image (trixie ships Murmur 1.5, bookworm 1.3) with a
# private D-Bus. ice runs the official mumblevoip/mumble-server image with
# host networking, so that Murmur can call the tests' callback listener on
# localhost. Uses podman, or docker if there is no podman; set
# CONTAINER_RUNTIME to choose.
set -eu
cd "$(dirname "$0")/.."

suite=${1:-}
[ $# -gt 0 ] && shift
runtime=${CONTAINER_RUNTIME:-$(command -v podman || command -v docker || true)}
[ -n "$runtime" ] || { echo "need podman or docker" >&2; exit 1; }

free_port() {
	python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])'
}

case "$suite" in
dbus)
	debian=${1:-trixie}
	[ $# -gt 0 ] && shift
	image=go-mumble-murmur:$debian
	"$runtime" build -q --build-arg DEBIAN="$debian" -t "$image" dbus/testdata/murmur >/dev/null
	cid=$("$runtime" run -d -p 127.0.0.1::4300 -p 127.0.0.1::64738 "$image")
	trap '"$runtime" rm -f "$cid" >/dev/null' EXIT
	bus=$("$runtime" port "$cid" 4300/tcp | head -n 1)
	MURMUR_DBUS_ADDRESS="tcp:host=${bus%:*},port=${bus##*:}"
	MURMUR_ADDRESS=$("$runtime" port "$cid" 64738/tcp | head -n 1)
	export MURMUR_DBUS_ADDRESS MURMUR_ADDRESS
	pkg=./dbus/...
	;;
ice)
	version=${1:-1.6}
	[ $# -gt 0 ] && shift
	case "$version" in
	1.6) tag=v1.6.1001 ;;
	1.5) tag=v1.5.939 ;;
	*) echo "unknown Ice version $version (1.6 or 1.5)" >&2; exit 1 ;;
	esac
	ice_port=$(free_port)
	mumble_port=$(free_port)
	cid=$("$runtime" run -d --network=host \
		-e MUMBLE_CONFIG_PORT="$mumble_port" \
		-e MUMBLE_CONFIG_ICE="\"tcp -h 127.0.0.1 -p $ice_port\"" \
		-e MUMBLE_CONFIG_ICESECRETWRITE=go-mumble-write \
		-e MUMBLE_CONFIG_ICESECRETREAD=go-mumble-read \
		-e MUMBLE_CONFIG_AUTOBANATTEMPTS=0 \
		-e MUMBLE_CONFIG_WELCOMETEXT=go-mumble-welcome \
		"docker.io/mumblevoip/mumble-server:$tag")
	trap '"$runtime" rm -f "$cid" >/dev/null' EXIT
	export MURMUR_ICE_ADDRESS="127.0.0.1:$ice_port"
	export MURMUR_ADDRESS="127.0.0.1:$mumble_port"
	export MURMUR_ICE_SECRET=go-mumble-write MURMUR_ICE_READ_SECRET=go-mumble-read
	pkg=./ice/...
	;;
*)
	echo "usage: $0 dbus|ice [version] [go test flags...]" >&2
	exit 2
	;;
esac

status=0
go test -tags integration -count=1 "$@" "$pkg" || status=$?
if [ "$status" -ne 0 ]; then
	echo "--- Murmur log" >&2
	"$runtime" logs "$cid" 2>&1 | tail -n 40 >&2
fi
exit "$status"
