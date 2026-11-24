# go-mumble/dbus

Package `dbus` controls Murmur through its D-Bus interface, which Murmur has up to the 1.5 series. Mumble 1.6 removed it ([mumble-voip/mumble#6580](https://github.com/mumble-voip/mumble/pull/6580)); use the [`ice`](../ice) package there.

The package covers the whole interface: the daemon-wide methods that create, start, stop, configure and delete virtual servers; the methods of each virtual server (connected users, channels, ACLs and groups, bans, registered users, avatars, text messages, kicks); the signals Murmur emits; and external authentication. It is pure Go on top of [godbus](https://github.com/godbus/dbus).

The tests run against Murmur 1.3.4 (Debian bookworm) and 1.5.735 (Debian trixie).

```bash
go get github.com/VeryItalianPeople/go-mumble/dbus
```

## Setting up Murmur

Murmur leaves D-Bus off until its ini file says which bus to use:

```ini
dbus=system
```

`session` and a bus address such as `unix:path=/run/murmur/bus` work too. `dbusservice` changes the bus name Murmur registers (the default is `net.sourceforge.mumble.murmur`); pass the same name to `dbus.WithService`.

On the system bus, a policy file has to let Murmur own its name and let your program call it. Debian's 1.3 package installs one in `/etc/dbus-1/system.d/mumble-server.conf`, but it lets every local user call Murmur, which includes setting the SuperUser password. The 1.5 packages install none. A tighter policy limits callers to a group:

```xml
<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-BUS Bus Configuration 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <policy user="mumble-server">
    <allow own="net.sourceforge.mumble.murmur"/>
  </policy>
  <policy group="mumble-admin">
    <allow send_destination="net.sourceforge.mumble.murmur"/>
  </policy>
</busconfig>
```

The stock system bus policy already lets signals through, so events need no extra rule. An authenticator does: see [Authentication](#authentication).

## Usage

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/VeryItalianPeople/go-mumble/dbus"
)

func main() {
	ctx := context.Background()

	c, err := dbus.ConnectSystemBus()
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	// Murmur starts with one virtual server, ID 1.
	srv := c.Server(1)

	players, err := srv.Players(ctx)
	if err != nil {
		log.Fatal(err)
	}
	for _, p := range players {
		fmt.Printf("%s (session %d) in channel %d\n", p.Name, p.Session, p.Channel)
		_ = srv.SendMessage(ctx, p.Session, "Hello from Go")
	}

	// Register a user, or reset the password of an existing one.
	// RegisterPlayer fails the same way for a taken name and an invalid one.
	id, err := srv.RegisterPlayer(ctx, "alice")
	if errors.Is(err, dbus.ErrInvalidPlayerName) {
		ids, err := srv.PlayerIDs(ctx, []string{"alice"})
		if err != nil {
			log.Fatal(err)
		}
		if ids[0] < 0 {
			log.Fatal("Murmur won't accept that name")
		}
		id = ids[0]
	} else if err != nil {
		log.Fatal(err)
	}
	if err := srv.SetRegistration(ctx, id, "alice", "", "s3cret"); err != nil {
		log.Fatal(err)
	}
}
```

Methods on `*dbus.Client` are the daemon-wide ones (`NewServer`, `Start`, `Stop`, `SetConf`, `Log`, ...). Methods on `*dbus.Server` act on one virtual server. They follow Murmur's D-Bus names, which say "player" where Mumble now says "user".

`dbus.Connect` takes any D-Bus address, and `dbus.New` wraps a `*godbus.Conn` you already have. `dbus.WithConnOptions` passes options through to godbus, for example anonymous authentication on a TCP bus.

### Errors

Errors that Murmur reports come back as `*dbus.Error`, which carries the D-Bus error name and Murmur's message. Compare them against the package's sentinels with `errors.Is`:

```go
_, err := srv.ChannelState(ctx, 42)
if errors.Is(err, dbus.ErrInvalidChannel) {
	// no such channel
}
```

`ErrNoServer` means the virtual server isn't running, and `ErrUnsupported` means the daemon doesn't export the method.

### Events

```go
events, err := srv.Events(ctx)
if err != nil {
	log.Fatal(err)
}
for ev := range events {
	switch ev.Kind {
	case dbus.PlayerConnected:
		fmt.Println("session", ev.Player.Session, "connected")
	case dbus.ChannelStateChanged:
		fmt.Println("channel", ev.Channel.Name, "changed")
	}
}
```

`Server.Events` delivers one virtual server's events, including its own `ServerStarted` and `ServerStopped`. `Client.Events` delivers everything. The channel closes when the context is done, the client is closed, or the connection drops. Events are queued, never dropped, so keep reading or cancel the context.

### Authentication

An authenticator takes over logins and user lookups for a virtual server. Murmur asks it first and only uses its own user database when the authenticator falls through. Embed `dbus.UnimplementedAuthenticator` and override what you need:

```go
type auth struct {
	dbus.UnimplementedAuthenticator
}

func (auth) Authenticate(ctx context.Context, name, password string) (dbus.AuthResult, error) {
	user, ok := lookUp(ctx, name, password) // your user store
	if !ok {
		return dbus.AuthResult{UserID: dbus.Fallthrough}, nil
	}
	return dbus.AuthResult{UserID: user.ID, Name: user.DisplayName, Groups: user.Groups}, nil
}

reg, err := srv.SetAuthenticator(ctx, auth{}, false)
```

`Authenticate` returns a user ID to accept the login, `dbus.AuthFailed` to reject it as a wrong password, `dbus.AuthTemporaryFailure` to reject it with "try again later", or `dbus.Fallthrough` to let Murmur decide. Murmur stores every accepted ID in its own database, so the IDs must be stable. An error returned from `Authenticate` counts as `AuthTemporaryFailure`. The other methods (`UserName`, `UserID`, `RegisterPlayer`, `SetPassword`, ...) are documented with their own fall-through values.

While Murmur waits for the authenticator, its main loop is blocked. If your methods call back into Murmur, pass `reentrant` as true, or those calls hang until Murmur gives up after 25 seconds.

Murmur has no call that removes an authenticator. It drops one when a call to it fails, so after `reg.Close()` the next login fails over to Murmur's own database and Murmur forgets the authenticator. Setting a new authenticator replaces the old one.

On the system bus, Murmur has to be allowed to call your program, and the stock policy denies method calls. Give your connection a well-known name before setting the authenticator. Murmur notices it and calls back on that name, which a policy can then allow:

```go
c.Conn().RequestName("org.example.MumbleAuth", godbus.NameFlagDoNotQueue)
reg, err := srv.SetAuthenticator(ctx, auth{}, false)
```

```xml
<policy user="your-user">
  <allow own="org.example.MumbleAuth"/>
</policy>
<policy user="mumble-server">
  <allow send_destination="org.example.MumbleAuth"/>
</policy>
```

## Murmur quirks

Murmur's D-Bus code has been unmaintained for a long time, and some of it doesn't do what its name says. The package documents each case on the method; the ones most likely to surprise you:

- `removeChannel` deadlocks the daemon. It takes a lock and then calls a function that takes the same non-recursive lock, so Murmur's main thread hangs and every virtual server on it stops answering. This happens on every version with D-Bus, so `Server.RemoveChannel` refuses with `ErrDeadlock` and never calls Murmur. Delete channels from a Mumble client with Write permission, or through Ice with the [`ice`](../ice) package, where the call is safe.
- `setBans` does nothing in any version, and `getBans` only lists IPv4 bans.
- `getVersion` is only exported up to 1.3. Later versions declare it with a type Qt can't marshal, so `Client.Version` returns `ErrUnsupported` there.
- Channels created with `AddChannel` emit no `ChannelCreated` signal. Murmur only sends it for channels clients create.
- After `SetTemporaryGroups`, Murmur clears the permission cache of the wrong user, so the target user can keep stale permissions. `SetACL` on any channel clears the cache for everyone.
- `SetACL` recreates a channel's groups from scratch, and only the groups you pass keep their temporary members.
- `ACL` lists the parent's rules even when the channel doesn't inherit them. `SetACL` skips every entry and group marked `Inherited`, so you can change the result and write it back.
- `UpdateRegistration` and `SetRegistration` report success for user IDs that don't exist, and leave empty fields unchanged.
- The `RegisteredPlayers` filter goes straight into an SQL `LIKE`: `%` and `_` are wildcards.
- `getLog` names its arguments `min_offset` and `max_offset`, but the second is a count. `Client.Log` calls them `offset` and `count`.
- When a call fails, Murmur sends an error and then a second, empty reply. The bus discards the second one and may log `Rejected send message ... requested_reply="0"`. That log line is harmless.
