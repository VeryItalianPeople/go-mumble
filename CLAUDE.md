# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A public Go module (`github.com/VeryItalianPeople/go-mumble`) with one package per Murmur RPC interface: `ice` (ZeroC Ice, Murmur 1.5 and 1.6) and `dbus` (D-Bus, up to 1.5). `internal/iceproto` is the Ice protocol underneath `ice`; `internal/mumbletest` is a Mumble client used only by the integration tests. The READMEs (root, `ice/`, `dbus/`) are the user documentation and carry the lists of Murmur quirks; don't duplicate them here.

## Commands

```bash
go vet ./... && go test ./...                      # unit tests, no Murmur needed
go vet -tags integration ./...                     # integration files only build with the tag
scripts/integration-test.sh ice  [1.6|1.5]         [go test flags]
scripts/integration-test.sh dbus [trixie|bookworm] [go test flags]
```

Run every combination before calling a change done: the four Murmurs behave differently, and the tests branch on version where they do. The `ice` suite runs the official `mumblevoip/mumble-server` image (tags pinned in the script) with `--network=host` and free ports picked by python3, so Murmur can reach the tests' callback listener on 127.0.0.1; it exports `MURMUR_ICE_ADDRESS`, `MURMUR_ADDRESS`, `MURMUR_ICE_SECRET`, `MURMUR_ICE_READ_SECRET`. The `dbus` suite builds `dbus/testdata/murmur/Containerfile` and exports `MURMUR_DBUS_ADDRESS` (TCP, ANONYMOUS auth) and `MURMUR_ADDRESS`. Without those variables each `TestMain` prints a note and exits 0.

`go.mod` says `go 1.22` and that is checked: run the unit tests in `docker.io/library/golang:1.22` after using anything newer from the standard library.

## Where the truth is

- Ice: `src/murmur/MumbleServer.ice` and `MumbleServerIce.cpp` in mumble-voip/mumble, at `v1.5.939` and at `master` for 1.6 (the docker tag `v1.6.1001` is a build number, not a git tag; `Meta.getSlice()` returns the Slice of whatever is running). The two Slices have identical struct layouts and operations; 1.6 adds three exceptions and the database-state calls. The wire format follows ZeroC's C++ sources at `v3.7.10` (`InputStream.cpp`, `OutputStream.cpp`, `ConnectionI.cpp`, `Incoming.cpp`, `Reference.cpp`), not a reading of the spec.
- D-Bus: `src/murmur/DBus.{h,cpp}` at `v1.5.939`, checked against the introspection of a running 1.3.4 and 1.5.735.

When behaviour is in doubt, read Murmur's implementation of the call and prove it with an integration test. Doc comments describe what Murmur does, not what the operation name suggests.

## Ice architecture

- `internal/iceproto/encoding.go`: encoder/decoder for encoding 1.1. Decoder errors are sticky (check `Err()` once at the end). Classes are supported only inline in the compact format, which is all Murmur's `Tree` needs.
- `internal/iceproto/conn.go`: `Client` multiplexes twoway requests by ID over one lazily dialled TCP connection, waits for the server's ValidateConnection, heartbeats every 30 s, and retries once on a new connection after a graceful close (or a lost connection for idempotent operations).
- `internal/iceproto/adapter.go`: the callback side. Oneway requests (ID 0, how Murmur sends events) are dispatched in order on the reading goroutine; twoway ones (authenticators) each get a goroutine. Built-in `ice_isA`/`ice_ping`/`ice_id`/`ice_ids` are answered for every servant, because Murmur `checkedCast`s callback proxies.
- `ice/`: one method per operation, each passing the operation's mode. **The mode must match the Slice** (`idempotent` or not): Ice servers reject a mismatch with a MarshalException. Struct marshalling in `types.go` follows field order in the Slice, which is the wire format.
- Authenticator servants never return an Ice error to Murmur, because Murmur drops the authenticator on any exception; Go errors become fall-through values (`authenticator.go`).

## Integration test notes

- Murmur 1.6.1001 has three ways to die: six configuration and log calls against a stopped server (mumble-voip/mumble#7449, fixed after that build, so `guardStopped` only applies up to `lastStoppedCrashBuild`), `setTexture` on an unregistered ID (guarded in `Server.SetTexture`), and an authenticator accepting an ID its database lacks (not guardable; the tests register users first). The other two are reported as mumble-voip/mumble#7522 and #7523. A test that trips any of them kills Murmur and every later test fails with "connection refused": look for a crash in the container log, not a network problem.
- Never call Murmur's D-Bus `removeChannel`, even from a test: it deadlocks the daemon. The `dbus` tests delete channels through a SuperUser Mumble connection.
- Don't call Murmur from inside an Ice authenticator method in a test: Murmur is waiting on the thread that would serve it, so it times out (`TestAuthenticator` asserts exactly that).
- Both suites log in a SuperUser over the Mumble protocol; a second SuperUser login kicks the first. The `ice` suite sets the password over Ice rather than trusting the image's `MUMBLE_SUPERUSER_PASSWORD`, which didn't take on 1.6.
- `autobanAttempts=0` matters in both suites: they connect often enough from one address for Murmur to ban it, which shows up as `connection reset by peer`.
- `internal/mumbletest` encodes protobuf by hand; field numbers come from Mumble's `Mumble.proto`. Add a named constant for a new message or field.
