# go-mumble

Go libraries for controlling a Murmur (Mumble server) daemon: creating and configuring virtual servers, managing users, channels, ACLs, bans and registrations, following what happens on a server, and taking over authentication.

Murmur has two RPC interfaces, and there is a package for each:

| Package | Interface | Murmur versions |
|---|---|---|
| [`ice`](ice) | ZeroC Ice, the `MumbleServer` Slice module | 1.5 and 1.6 |
| [`dbus`](dbus) | D-Bus | up to 1.5 (removed in 1.6) |

Both are pure Go with no cgo. The `ice` package implements the Ice protocol itself, since ZeroC has no Go runtime; `dbus` builds on [godbus](https://github.com/godbus/dbus). Use `ice` unless your Murmur is older than 1.5 or you already run it with D-Bus enabled.

```bash
go get github.com/VeryItalianPeople/go-mumble/ice
```

The module needs Go 1.22 or later.

```go
c, err := ice.Dial(ctx, "127.0.0.1:6502", ice.WithSecret("icesecretwrite value"))
if err != nil {
	log.Fatal(err)
}
defer c.Close()

servers, err := c.BootedServers(ctx)
if err != nil {
	log.Fatal(err)
}
users, err := servers[0].Users(ctx)
```

Each package's README covers setting Murmur up, the API, events and authenticators, and the bugs in Murmur itself that the package works around. Several are serious: over D-Bus, `removeChannel` deadlocks the daemon in every version, and Murmur 1.6 crashes on some Ice calls against a stopped server and on some authenticator answers. Read the "Murmur quirks" section of the package you use.

## Tests

```bash
go test ./...
```

runs the unit tests. The integration tests run against a real Murmur in a container, with podman or docker:

```bash
scripts/integration-test.sh ice 1.6
```

```bash
scripts/integration-test.sh dbus bookworm -run TestAuthenticator -v
```

The first argument picks the suite. For `ice`, the second is the Murmur version (`1.6`, the default, or `1.5`), run from the official `mumblevoip/mumble-server` image with host networking so that Murmur can reach the tests' callback listener. For `dbus`, it is the Debian release (`trixie` for Murmur 1.5, the default, or `bookworm` for 1.3), with Murmur on a private bus the tests reach over TCP. Anything after that goes to `go test`. The `ice` suite uses python3 to pick free ports. Both suites include a small Mumble protocol client, so they log real users in to check events, messages, kicks and authenticators.

## License

MIT, see [LICENSE](LICENSE).
