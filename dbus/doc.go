// Package dbus controls a Murmur (Mumble server) daemon through its D-Bus
// interface.
//
// It covers the whole interface: the daemon-wide methods for creating,
// starting and configuring virtual servers (methods on [Client]), the
// methods of each virtual server for users, channels, ACLs, bans and
// registrations (methods on [Server]), the signals Murmur emits
// ([Client.Events], [Server.Events]), and external authentication
// ([Server.SetAuthenticator]).
//
// Murmur only has this interface up to the 1.5 series. Mumble 1.6 removed it
// in favour of ZeroC Ice. Murmur must also be told to use it: set dbus=system
// (or session, or a bus address) in its ini file.
//
//	c, err := dbus.ConnectSystemBus()
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer c.Close()
//
//	players, err := c.Server(1).Players(ctx)
//
// Method names follow Murmur's D-Bus names, which say "player" where Mumble
// now says "user". Each method's documentation notes what Murmur actually
// does where that differs from what the name suggests; [Server.SetBans], for
// one, does nothing at all.
package dbus
