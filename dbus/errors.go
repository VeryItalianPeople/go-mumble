package dbus

import (
	"errors"

	godbus "github.com/godbus/dbus/v5"
)

// Errors Murmur reports. Test for them with [errors.Is]; the returned error is
// an [*Error] carrying Murmur's own message.
var (
	ErrInvalidSession    = errors.New("murmur: invalid session")
	ErrInvalidChannel    = errors.New("murmur: invalid channel")
	ErrInvalidPlayerName = errors.New("murmur: invalid player name")
	ErrInvalidPlayerID   = errors.New("murmur: invalid player ID")
	ErrInvalidTexture    = errors.New("murmur: invalid texture")
	ErrInvalidServer     = errors.New("murmur: invalid server ID")
	ErrBootFailed        = errors.New("murmur: booting server failed")
	ErrWriteOnly         = errors.New("murmur: configuration key is write-only")
	// ErrAlreadyBooted is returned by [Client.Start] for a running server.
	ErrAlreadyBooted = errors.New("murmur: server already booted")
	// ErrNotBooted is returned by [Client.Stop] for a stopped server.
	ErrNotBooted = errors.New("murmur: server not booted")
	// ErrServerRunning is returned by [Client.DeleteServer] for a running
	// server.
	ErrServerRunning = errors.New("murmur: server is running")
	// ErrUnsupported is returned when the daemon doesn't export the method,
	// such as getVersion on Murmur 1.4 and later.
	ErrUnsupported = errors.New("murmur: method not supported by this Murmur")
	// ErrNoServer is returned when no virtual server answers on the object
	// path, which is what Murmur does for a server that is not running.
	ErrNoServer = errors.New("murmur: no such running server")
	// ErrDeadlock is returned, without calling Murmur, by methods whose
	// implementation in Murmur hangs the daemon. See [Server.RemoveChannel].
	ErrDeadlock = errors.New("murmur: refusing a call that deadlocks Murmur")
)

const errorPrefix = "net.sourceforge.mumble.Error."

// Error is a D-Bus error returned by Murmur or by the bus.
type Error struct {
	// Method is the D-Bus method that failed, e.g. "getChannelState".
	Method string
	// Name is the D-Bus error name, e.g.
	// "net.sourceforge.mumble.Error.channel".
	Name string
	// Message is the text that came with the error.
	Message string

	sentinel error
}

func (e *Error) Error() string {
	msg := e.Message
	if msg == "" {
		msg = e.Name
	}
	return "murmur: " + e.Method + ": " + msg
}

// Unwrap returns the matching Err* value, if there is one.
func (e *Error) Unwrap() error {
	return e.sentinel
}

// wrapError turns an error from a godbus call into an *Error when it is a
// D-Bus error, and leaves anything else (context, transport) untouched.
func wrapError(method string, err error) error {
	if err == nil {
		return nil
	}
	var dbe godbus.Error
	switch {
	case errors.As(err, &dbe):
	default:
		var p *godbus.Error
		if !errors.As(err, &p) || p == nil {
			return err
		}
		dbe = *p
	}
	e := &Error{Method: method, Name: dbe.Name}
	if len(dbe.Body) > 0 {
		if s, ok := dbe.Body[0].(string); ok {
			e.Message = s
		}
	}
	e.sentinel = sentinelFor(method, dbe.Name)
	return e
}

func sentinelFor(method, name string) error {
	switch name {
	case errorPrefix + "session":
		return ErrInvalidSession
	case errorPrefix + "channel":
		return ErrInvalidChannel
	case errorPrefix + "playername":
		return ErrInvalidPlayerName
	case errorPrefix + "playerid":
		return ErrInvalidPlayerID
	case errorPrefix + "texture":
		return ErrInvalidTexture
	case errorPrefix + "server":
		return ErrInvalidServer
	case errorPrefix + "bootfail":
		return ErrBootFailed
	case errorPrefix + "writeonly":
		return ErrWriteOnly
	case errorPrefix + "booted":
		// One error name, three meanings.
		switch method {
		case "start":
			return ErrAlreadyBooted
		case "stop":
			return ErrNotBooted
		case "deleteServer":
			return ErrServerRunning
		}
	case "org.freedesktop.DBus.Error.UnknownMethod":
		return ErrUnsupported
	case "org.freedesktop.DBus.Error.UnknownObject":
		return ErrNoServer
	}
	return nil
}
