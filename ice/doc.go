// Package ice controls a Murmur (Mumble server) daemon through its ZeroC Ice
// interface, the MumbleServer module of Mumble 1.5 and 1.6.
//
// It speaks the Ice protocol itself, in pure Go: no ZeroC runtime and no
// generated code. It covers every operation of the Meta and Server
// interfaces, the server and Meta callbacks (as event channels), context
// menu actions, and both kinds of authenticator.
//
//	c, err := ice.Dial(ctx, "127.0.0.1:6502", ice.WithSecret("icesecretwrite value"))
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer c.Close()
//
//	servers, err := c.BootedServers(ctx)
//	users, err := servers[0].Users(ctx)
//
// Murmur only listens for Ice when its ini file sets ice, for example
// ice="tcp -h 127.0.0.1 -p 6502", and checks icesecretread and
// icesecretwrite against the secret each call carries.
//
// Callbacks, context actions and authenticators are calls from Murmur back
// to this process: the client opens a listener for them on first use, and
// Murmur must be able to connect to it. See [WithCallbackAddress] and
// [WithCallbackSecret].
//
// Murmur 1.4 and earlier use the older "Murmur" Slice module, which this
// package does not speak; use the dbus package for those.
package ice
