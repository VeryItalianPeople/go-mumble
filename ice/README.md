# go-mumble/ice

Package `ice` controls Murmur through its ZeroC Ice interface, the `MumbleServer` Slice module of Mumble 1.5 and 1.6. Since 1.6 it is the only RPC interface Murmur has.

There is no ZeroC runtime for Go, so the package speaks the Ice protocol itself (protocol 1.0, encoding 1.1, plain TCP). It covers every operation of the `Meta` and `Server` interfaces, the server and Meta callbacks, context menu actions, and both kinds of authenticator. There is no generated code and no cgo.

The tests run against the official `mumblevoip/mumble-server` images of Murmur 1.6.1001 and 1.5.939. Murmur 1.4 and earlier use the older `Murmur` Slice module, which this package doesn't speak; the [`dbus`](../dbus) package covers those.

```bash
go get github.com/VeryItalianPeople/go-mumble/ice
```

## Setting up Murmur

Murmur listens for Ice only when its ini file sets an endpoint, and checks the secret every call carries:

```ini
ice="tcp -h 127.0.0.1 -p 6502"
icesecretread=a-read-secret
icesecretwrite=a-write-secret
```

With `icesecretwrite` set, anything that changes state needs that secret, and that includes registering callbacks and authenticators. The read secret is enough for the rest. Only the `tcp` transport is supported, not `ssl`, so keep the endpoint on localhost or a private network.

## Usage

```go
c, err := ice.Dial(ctx, "127.0.0.1:6502", ice.WithSecret("a-write-secret"))
if err != nil {
	log.Fatal(err)
}
defer c.Close()

servers, err := c.BootedServers(ctx)
if err != nil {
	log.Fatal(err)
}
srv := servers[0] // 0 on a fresh 1.6, 1 on 1.5

users, err := srv.Users(ctx)
for session, u := range users {
	fmt.Println(session, u.Name, u.Channel)
	_ = srv.SendMessage(ctx, session, "Hello from Go")
}

id, err := srv.RegisterUser(ctx, ice.UserInfoMap{
	ice.UserName:     "alice",
	ice.UserPassword: "s3cret",
})
```

Methods on `*ice.Client` are the `Meta` interface (`NewServer`, `BootedServers`, `Version`, `Slice`, ...). `Client.Server(id)` returns a handle on one virtual server without calling Murmur, and its methods cover the `Server` interface. The client keeps one connection, sends heartbeats on it, and reconnects by itself when it is closed or lost, retrying a call when Ice's own rules say that's safe.

Errors that Murmur throws come back as `*ice.Error`, carrying the exception's Slice type ID. Compare them with `errors.Is` against the sentinels (`ErrInvalidSession`, `ErrInvalidChannel`, `ErrServerBooted`, ...).

### Events and context actions

```go
events, err := srv.Events(ctx)
for ev := range events {
	switch ev.Kind {
	case ice.UserConnected:
		fmt.Println(ev.User.Name, "connected")
	case ice.UserTextMessage:
		fmt.Println(ev.User.Name, "said", ev.Message.Text)
	}
}
```

`Server.Events` registers a server callback; `Client.Events` registers a Meta callback, which reports servers starting and stopping. Both channels close when the context is done or the client is closed, and the callback is removed from Murmur then. Events are queued, never dropped.

`Server.AddContextCallback` adds an entry to a user's context menu and calls a function when they click it.

### Callbacks reach your process

Events, context actions and authenticators are calls from Murmur into your program. On first use the client starts a listener and gives Murmur its address. By default that is a random port on the local address of the client's connection to Murmur, which works when Murmur can connect back the way you connected to it. When it can't (NAT, containers), say where to listen and what address to give Murmur:

```go
ice.Dial(ctx, addr,
	ice.WithSecret(writeSecret),
	ice.WithCallbackAddress("0.0.0.0:6503", "10.0.0.5"),
	ice.WithCallbackSecret(writeSecret),
)
```

Murmur sends its `icesecretwrite` with every call it makes. `WithCallbackSecret` makes the client refuse calls without it, so nobody else who reaches the port can feed you events or ask your authenticator about passwords. Murmur doesn't test a callback when you register it, so with the wrong secret, registration succeeds and every event is then quietly refused.

### Authentication

```go
type auth struct {
	ice.UnimplementedAuthenticator
}

func (auth) Authenticate(ctx context.Context, req ice.AuthRequest) (ice.AuthResult, error) {
	user, ok := lookUp(ctx, req.Name, req.Password) // your user store
	if !ok {
		return ice.AuthResult{UserID: ice.Fallthrough}, nil
	}
	return ice.AuthResult{UserID: user.MurmurID, Name: user.DisplayName, Groups: user.Groups}, nil
}

h, err := srv.SetAuthenticator(ctx, auth{})
```

Implement `ice.UpdatingAuthenticator` as well (or embed `UnimplementedUpdatingAuthenticator`) to be told about registrations and changes to them. `Authenticate` returns a user ID, `AuthFailed`, `AuthTemporaryFailure` or `Fallthrough`; the other methods document their own fall-through values.

Murmur drops an authenticator as soon as any call to it fails. The package never lets that happen because of your code: an error from `Authenticate` is answered as `AuthTemporaryFailure`, and an error anywhere else as falling through. Murmur also drops the authenticator, and every callback, when the server stops; set it again after `ServerStarted`.

Don't call Murmur from inside an authenticator method. Murmur waits for your answer on the thread that would serve the call, so it can only time out, after 5 seconds.

## Murmur quirks

These are behaviours of Murmur itself, found in its source and checked against a running Murmur wherever the tests can reach them. The package works around them where it can and documents them on the methods.

On 1.6 (seen on 1.6.1001, which is v1.6.0-alpha2):

- `getConf`, `getAllConf`, `setConf`, `setSuperuserPassword`, `getLog` and `getLogLen` crash the daemon with a segmentation fault, taking every virtual server down, when the server they're called on exists but isn't running ([mumble-voip/mumble#7449](https://github.com/mumble-voip/mumble/issues/7449), fixed after that build by [#7487](https://github.com/mumble-voip/mumble/pull/7487)). On builds up to 1001 the package asks whether the server is running first and returns `ErrWouldCrash` instead.
- When an authenticator accepts a login with a user ID that isn't in Murmur's own database, Murmur fails to look up the user's last channel and terminates ([mumble-voip/mumble#7523](https://github.com/mumble-voip/mumble/issues/7523)). Only return IDs of users registered in Murmur (`Server.RegisterUser` gives you one), or set `rememberchannel=false`. On 1.5, any ID works and Murmur adds it to its database.
- `getTexture` and `setTexture` test the user the wrong way round ([mumble-voip/mumble#7522](https://github.com/mumble-voip/mumble/issues/7522)): they fail with `ErrInvalidUser` for every registered user, and let unregistered IDs through. For `setTexture` that means Murmur terminates, so `Server.SetTexture` checks the user is registered first and returns `ErrWouldCrash` otherwise.
- Operations on a server ID that doesn't exist mostly fail with `ErrServerBooted`, where 1.5 gives `ErrServerNotFound`.
- `setConf` with an empty value stores the empty value; 1.5 deletes the key.

`WithoutCrashGuards` turns the package's checks off.

On both:

- Channels created through Ice emit no `ChannelCreated` event. Murmur only reports channels clients create.
- `setACL` would store inherited entries as new rules of the channel; `Server.SetACL` skips everything marked `Inherited`, so you can change the result of `ACL` and write it back.
- `getLog(first, last)` reads `last` as a count on 1.5 and as an end index on 1.6. `Server.Log` takes an offset and a count and translates.
- The listener calls (`startListening`, `stopListening`, `isListening`, `getListeningChannels`, `getListenerVolumeAdjustment`, `setListenerVolumeAdjustment`) and `sendWelcomeMessage` name their parameter `userid` but take a session. The Go methods say `session`.
- Unlike the D-Bus call of the same name, `removeChannel` is safe.
