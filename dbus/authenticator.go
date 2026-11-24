package dbus

import (
	"context"
	"fmt"
	"sync"
	"time"

	godbus "github.com/godbus/dbus/v5"
)

// Values an [Authenticator] returns to tell Murmur what to do.
const (
	// AuthFailed rejects a login with "Wrong certificate or password for
	// existing user". Returned from RegisterPlayer, it refuses the
	// registration.
	AuthFailed int32 = -1
	// Fallthrough leaves the decision to Murmur's own user database, as if
	// no authenticator were set.
	Fallthrough int32 = -2
	// AuthTemporaryFailure rejects a login with "Your account information
	// can not be verified currently. Please try again later".
	AuthTemporaryFailure int32 = -3
)

// AuthResult is the answer to a login attempt.
type AuthResult struct {
	// UserID is the user's ID (0 or more) to accept the login, or one of
	// AuthFailed, Fallthrough and AuthTemporaryFailure. Murmur stores an
	// accepted ID in its own database, so it must be stable per user.
	UserID int32
	// Name, if set, replaces the name the user connected with.
	Name string
	// Groups are temporary groups of the root channel that the user is put
	// in for this session.
	Groups []string
}

// Authenticator takes over user authentication for a virtual server. Murmur
// calls it for every login and for every user lookup, and only consults its
// own user database when the authenticator falls through.
//
// Every method has a way to fall through, listed on the method. An error
// returned from Authenticate counts as AuthTemporaryFailure, and one from
// UserName or UserID as falling through; Murmur would drop the authenticator
// on the spot if any of those three calls failed on the bus. An error from
// any other method is sent to Murmur as a failed call, which it treats as
// falling through.
//
// Embed [UnimplementedAuthenticator] to fall through on everything you don't
// implement. Methods may be called concurrently.
type Authenticator interface {
	// Authenticate checks a login.
	Authenticate(ctx context.Context, name, password string) (AuthResult, error)
	// UserName returns the name of a user ID, or "" to fall through.
	UserName(ctx context.Context, userID int32) (string, error)
	// UserID returns the ID of a user name, or Fallthrough. Murmur caches
	// any other answer, AuthFailed included.
	UserID(ctx context.Context, name string) (int32, error)
	// UserTexture returns a user's avatar, or nil to fall through. A
	// non-nil empty slice means the user has no avatar.
	UserTexture(ctx context.Context, userID int32) ([]byte, error)
	// RegisterPlayer is called when a user registers: return the new ID,
	// AuthFailed to refuse, or Fallthrough to let Murmur pick the ID.
	// Murmur writes the user to its own database either way.
	RegisterPlayer(ctx context.Context, name string) (int32, error)
	// UnregisterPlayer is called when a user is deleted. Return 0 to
	// refuse; anything else lets Murmur delete its own record.
	UnregisterPlayer(ctx context.Context, userID int32) (int32, error)
	// Registration returns a user's registration, or false to fall
	// through. Only Name and Email are used.
	Registration(ctx context.Context, userID int32) (Registration, bool, error)
	// RegisteredPlayers lists users matching filter. Murmur merges them
	// with the ones in its own database.
	RegisteredPlayers(ctx context.Context, filter string) ([]Registration, error)
	// SetName, SetPassword, SetEmail and SetTexture change a user. Return
	// 1 when done, 0 when it failed, or Fallthrough to have Murmur store
	// the change in its own database.
	SetName(ctx context.Context, userID int32, name string) (int32, error)
	SetPassword(ctx context.Context, userID int32, password string) (int32, error)
	SetEmail(ctx context.Context, userID int32, email string) (int32, error)
	SetTexture(ctx context.Context, userID int32, texture []byte) (int32, error)
}

// UnimplementedAuthenticator falls through on every method. Embed it in an
// [Authenticator] and override only what you need.
type UnimplementedAuthenticator struct{}

func (UnimplementedAuthenticator) Authenticate(context.Context, string, string) (AuthResult, error) {
	return AuthResult{UserID: Fallthrough}, nil
}

func (UnimplementedAuthenticator) UserName(context.Context, int32) (string, error) {
	return "", nil
}

func (UnimplementedAuthenticator) UserID(context.Context, string) (int32, error) {
	return Fallthrough, nil
}

func (UnimplementedAuthenticator) UserTexture(context.Context, int32) ([]byte, error) {
	return nil, nil
}

func (UnimplementedAuthenticator) RegisterPlayer(context.Context, string) (int32, error) {
	return Fallthrough, nil
}

func (UnimplementedAuthenticator) UnregisterPlayer(context.Context, int32) (int32, error) {
	return Fallthrough, nil
}

func (UnimplementedAuthenticator) Registration(context.Context, int32) (Registration, bool, error) {
	return Registration{}, false, nil
}

func (UnimplementedAuthenticator) RegisteredPlayers(context.Context, string) ([]Registration, error) {
	return nil, nil
}

func (UnimplementedAuthenticator) SetName(context.Context, int32, string) (int32, error) {
	return Fallthrough, nil
}

func (UnimplementedAuthenticator) SetPassword(context.Context, int32, string) (int32, error) {
	return Fallthrough, nil
}

func (UnimplementedAuthenticator) SetEmail(context.Context, int32, string) (int32, error) {
	return Fallthrough, nil
}

func (UnimplementedAuthenticator) SetTexture(context.Context, int32, []byte) (int32, error) {
	return Fallthrough, nil
}

// authenticatorInterface is the interface the exported object is registered
// under. Murmur calls the methods without naming an interface, so the name is
// only for introspection.
const authenticatorInterface = "com.github.VeryItalianPeople.GoMumble.Authenticator"

// callTimeout bounds each callback. Murmur waits for the reply with Qt's
// default D-Bus timeout of 25 seconds and gives up after that.
const callTimeout = 25 * time.Second

var errNotHandled = godbus.NewError(authenticatorInterface+".NotHandled", []any{"not handled"})

// AuthenticatorRegistration is an authenticator exported on the client's
// connection and installed on a virtual server.
type AuthenticatorRegistration struct {
	c      *Client
	path   godbus.ObjectPath
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
}

// Path returns the object path the authenticator is exported on.
func (r *AuthenticatorRegistration) Path() godbus.ObjectPath {
	return r.path
}

// Close stops answering Murmur's calls. Murmur has no call to remove an
// authenticator: it drops one when a call to it fails, so the first login or
// name lookup after Close fails on the bus, Murmur drops the authenticator,
// and that one request falls through to Murmur's own database. Installing a
// new authenticator on the server replaces this one outright.
func (r *AuthenticatorRegistration) Close() error {
	r.unexport()
	return nil
}

func (r *AuthenticatorRegistration) unexport() {
	r.once.Do(func() {
		r.cancel()
		_ = r.c.conn.ExportMethodTable(nil, r.path, authenticatorInterface)
		r.c.mu.Lock()
		delete(r.c.authRegs, r)
		r.c.mu.Unlock()
	})
}

// SetAuthenticator exports a on the client's connection and installs it on
// the virtual server, replacing any authenticator the server had.
//
// While Murmur waits for an answer it blocks its main loop, unless reentrant
// is true, in which case it keeps serving its own D-Bus calls meanwhile. Pass
// true if a calls back into Murmur from its methods; otherwise such a call
// hangs until Murmur's 25-second timeout.
//
// Each method receives a context that is cancelled after 25 seconds, when the
// registration is closed, or when the client is closed.
func (s *Server) SetAuthenticator(ctx context.Context, a Authenticator, reentrant bool) (*AuthenticatorRegistration, error) {
	c := s.c
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errClosed
	}
	c.authN++
	path := godbus.ObjectPath(fmt.Sprintf("/gomumble/authenticator/s%d_%d", s.id, c.authN))
	rctx, cancel := context.WithCancel(context.Background())
	r := &AuthenticatorRegistration{c: c, path: path, ctx: rctx, cancel: cancel}
	c.authRegs[r] = struct{}{}
	c.mu.Unlock()

	if err := c.conn.ExportMethodTable(authenticatorMethods(rctx, a), path, authenticatorInterface); err != nil {
		r.unexport()
		return nil, fmt.Errorf("murmur: exporting authenticator: %w", err)
	}
	if err := s.SetAuthenticatorPath(ctx, path, reentrant); err != nil {
		r.unexport()
		return nil, err
	}
	return r, nil
}

// SetAuthenticatorPath installs an authenticator you exported yourself on
// the client's connection at path. Murmur calls back the connection that
// made this call. Most programs want [Server.SetAuthenticator] instead.
func (s *Server) SetAuthenticatorPath(ctx context.Context, path godbus.ObjectPath, reentrant bool) error {
	return s.call(ctx, "setAuthenticator", []any{path, reentrant})
}

// authenticatorMethods builds the method table Murmur calls. The method
// names and signatures are the ones Murmur's MurmurDBus uses in its *Slot
// functions.
func authenticatorMethods(base context.Context, a Authenticator) map[string]any {
	ctx := func() (context.Context, context.CancelFunc) {
		return context.WithTimeout(base, callTimeout)
	}
	intReply := func(n int32, err error) (int32, *godbus.Error) {
		if err != nil {
			return 0, errNotHandled
		}
		return n, nil
	}
	return map[string]any{
		"authenticate": func(name, password string) (int32, string, []string, *godbus.Error) {
			cctx, cancel := ctx()
			defer cancel()
			res, err := a.Authenticate(cctx, name, password)
			if err != nil {
				return AuthTemporaryFailure, "", []string{}, nil
			}
			return res.UserID, res.Name, nonNil(res.Groups), nil
		},
		"getUserName": func(id int32) (string, *godbus.Error) {
			cctx, cancel := ctx()
			defer cancel()
			name, err := a.UserName(cctx, id)
			if err != nil {
				return "", nil
			}
			return name, nil
		},
		"getUserId": func(name string) (int32, *godbus.Error) {
			cctx, cancel := ctx()
			defer cancel()
			id, err := a.UserID(cctx, name)
			if err != nil {
				return Fallthrough, nil
			}
			return id, nil
		},
		"getUserTexture": func(id int32) ([]byte, *godbus.Error) {
			cctx, cancel := ctx()
			defer cancel()
			tex, err := a.UserTexture(cctx, id)
			if err != nil || tex == nil {
				return nil, errNotHandled
			}
			return tex, nil
		},
		"registerPlayer": func(name string) (int32, *godbus.Error) {
			cctx, cancel := ctx()
			defer cancel()
			return intReply(a.RegisterPlayer(cctx, name))
		},
		"unregisterPlayer": func(id int32) (int32, *godbus.Error) {
			cctx, cancel := ctx()
			defer cancel()
			return intReply(a.UnregisterPlayer(cctx, id))
		},
		"getRegistration": func(id int32) (wireRegistration, *godbus.Error) {
			cctx, cancel := ctx()
			defer cancel()
			reg, ok, err := a.Registration(cctx, id)
			if err != nil || !ok {
				return wireRegistration{}, errNotHandled
			}
			reg.ID = id
			reg.Password = ""
			return reg.wire(), nil
		},
		"getRegisteredPlayers": func(filter string) ([]wireRegistration, *godbus.Error) {
			cctx, cancel := ctx()
			defer cancel()
			regs, err := a.RegisteredPlayers(cctx, filter)
			if err != nil {
				return nil, errNotHandled
			}
			out := make([]wireRegistration, len(regs))
			for i, r := range regs {
				r.Password = ""
				out[i] = r.wire()
			}
			return out, nil
		},
		"setName": func(id int32, name string) (int32, *godbus.Error) {
			cctx, cancel := ctx()
			defer cancel()
			return intReply(a.SetName(cctx, id, name))
		},
		"setPW": func(id int32, password string) (int32, *godbus.Error) {
			cctx, cancel := ctx()
			defer cancel()
			return intReply(a.SetPassword(cctx, id, password))
		},
		"setEmail": func(id int32, email string) (int32, *godbus.Error) {
			cctx, cancel := ctx()
			defer cancel()
			return intReply(a.SetEmail(cctx, id, email))
		},
		"setTexture": func(id int32, texture []byte) (int32, *godbus.Error) {
			cctx, cancel := ctx()
			defer cancel()
			return intReply(a.SetTexture(cctx, id, texture))
		},
	}
}
