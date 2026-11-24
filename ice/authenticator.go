package ice

import (
	"context"
	"time"

	"github.com/VeryItalianPeople/go-mumble/internal/iceproto"
)

// Values an authenticator returns to tell Murmur what to do.
const (
	// AuthFailed rejects a login as a wrong password.
	AuthFailed int32 = -1
	// Fallthrough leaves the decision to Murmur's own user database.
	Fallthrough int32 = -2
	// AuthTemporaryFailure rejects a login with "Your account information
	// can not be verified currently. Please try again later".
	AuthTemporaryFailure int32 = -3
)

// AuthRequest is a login attempt.
type AuthRequest struct {
	Name     string
	Password string
	// Certificates is the chain the client presented, DER-encoded, leaf
	// first; empty without a certificate.
	Certificates [][]byte
	// CertHash is the SHA-1 hash of the client certificate, hex-encoded.
	CertHash string
	// CertStrong is set when the certificate was verified against a
	// trusted CA.
	CertStrong bool
}

// AuthResult is the answer to a login attempt.
type AuthResult struct {
	// UserID is the user's ID (0 or more) to accept the login, or one of
	// AuthFailed, Fallthrough and AuthTemporaryFailure.
	//
	// Murmur 1.5 adds an accepted ID to its own database. Murmur 1.6
	// looks the user's last channel up there instead, and when the ID isn't
	// found the whole daemon exits with "Database error" (seen on
	// 1.6.1001, mumble-voip/mumble#7523). On 1.6, only accept IDs of users registered in Murmur,
	// e.g. with [Server.RegisterUser] ahead of time, or run it with
	// rememberchannel=false.
	UserID int32
	// Name, if set, replaces the name the user connected with.
	Name string
	// Groups are temporary groups of the root channel the user is put in
	// for this session.
	Groups []string
}

// Authenticator takes over logins and user lookups for a virtual server.
// Murmur asks it first and only consults its own database when it falls
// through. Each method says how to fall through.
//
// Murmur drops the authenticator the moment any call to it fails, so the
// methods' errors never reach Murmur: an error from Authenticate is answered
// as AuthTemporaryFailure, and an error from anything else as falling
// through. Murmur waits 5 seconds per call; each method's context expires
// then.
//
// Don't call Murmur from inside these methods: Murmur waits for the answer on
// the thread that would serve the call, so the call can only time out.
//
// Embed [UnimplementedAuthenticator] to fall through on what you don't
// implement. Methods may be called concurrently.
type Authenticator interface {
	// Authenticate checks a login.
	Authenticate(ctx context.Context, req AuthRequest) (AuthResult, error)
	// Info returns a user's registration fields, or false to fall through.
	Info(ctx context.Context, userID int32) (UserInfoMap, bool, error)
	// NameToID returns a name's user ID, or Fallthrough.
	NameToID(ctx context.Context, name string) (int32, error)
	// IDToName returns a user ID's name, or "" to fall through.
	IDToName(ctx context.Context, userID int32) (string, error)
	// IDToTexture returns a user's avatar, or nil to fall through.
	IDToTexture(ctx context.Context, userID int32) ([]byte, error)
}

// UpdatingAuthenticator is an [Authenticator] that also handles changes to
// registrations. Murmur sees which interface the value implements.
type UpdatingAuthenticator interface {
	Authenticator
	// RegisterUser registers a user and returns its ID, AuthFailed to
	// refuse, or Fallthrough to let Murmur register it in its own database.
	RegisterUser(ctx context.Context, info UserInfoMap) (int32, error)
	// UnregisterUser deletes a user. Return 0 to refuse; any other value
	// lets Murmur delete its own record too.
	UnregisterUser(ctx context.Context, userID int32) (int32, error)
	// RegisteredUsers lists users matching filter by ID. Murmur adds the
	// ones in its own database.
	RegisteredUsers(ctx context.Context, filter string) (map[int32]string, error)
	// SetInfo and SetTexture change a user. Return 1 when done, 0 when it
	// failed, or Fallthrough to have Murmur store the change itself.
	SetInfo(ctx context.Context, userID int32, info UserInfoMap) (int32, error)
	SetTexture(ctx context.Context, userID int32, texture []byte) (int32, error)
}

// UnimplementedAuthenticator falls through on everything. Embed it and
// override what you need.
type UnimplementedAuthenticator struct{}

func (UnimplementedAuthenticator) Authenticate(context.Context, AuthRequest) (AuthResult, error) {
	return AuthResult{UserID: Fallthrough}, nil
}

func (UnimplementedAuthenticator) Info(context.Context, int32) (UserInfoMap, bool, error) {
	return nil, false, nil
}

func (UnimplementedAuthenticator) NameToID(context.Context, string) (int32, error) {
	return Fallthrough, nil
}

func (UnimplementedAuthenticator) IDToName(context.Context, int32) (string, error) {
	return "", nil
}

func (UnimplementedAuthenticator) IDToTexture(context.Context, int32) ([]byte, error) {
	return nil, nil
}

// UnimplementedUpdatingAuthenticator falls through on everything,
// registration changes included.
type UnimplementedUpdatingAuthenticator struct {
	UnimplementedAuthenticator
}

func (UnimplementedUpdatingAuthenticator) RegisterUser(context.Context, UserInfoMap) (int32, error) {
	return Fallthrough, nil
}

func (UnimplementedUpdatingAuthenticator) UnregisterUser(context.Context, int32) (int32, error) {
	return Fallthrough, nil
}

func (UnimplementedUpdatingAuthenticator) RegisteredUsers(context.Context, string) (map[int32]string, error) {
	return nil, nil
}

func (UnimplementedUpdatingAuthenticator) SetInfo(context.Context, int32, UserInfoMap) (int32, error) {
	return Fallthrough, nil
}

func (UnimplementedUpdatingAuthenticator) SetTexture(context.Context, int32, []byte) (int32, error) {
	return Fallthrough, nil
}

const (
	authenticatorTypeID         = "::MumbleServer::ServerAuthenticator"
	updatingAuthenticatorTypeID = "::MumbleServer::ServerUpdatingAuthenticator"
)

// authTimeout matches the 5-second timeout Murmur sets on its proxy.
const authTimeout = 5 * time.Second

type authServant struct {
	a  Authenticator
	ua UpdatingAuthenticator // nil unless a implements it
}

func (s authServant) TypeIDs() []string {
	if s.ua != nil {
		return []string{updatingAuthenticatorTypeID, authenticatorTypeID}
	}
	return []string{authenticatorTypeID}
}

func (s authServant) Dispatch(base context.Context, cur *iceproto.Current, in *iceproto.Decoder, out *iceproto.Encoder) error {
	ctx, cancel := context.WithTimeout(base, authTimeout)
	defer cancel()
	intOr := func(n int32, err error, fallback int32) int32 {
		if err != nil {
			return fallback
		}
		return n
	}

	switch cur.Operation {
	case "authenticate":
		req := AuthRequest{Name: in.String(), Password: in.String()}
		n := in.Size()
		for i := 0; i < n && in.Err() == nil; i++ {
			req.Certificates = append(req.Certificates, in.ByteSeq())
		}
		req.CertHash = in.String()
		req.CertStrong = in.Bool()
		if err := in.Err(); err != nil {
			return err
		}
		res, err := s.a.Authenticate(ctx, req)
		if err != nil {
			res = AuthResult{UserID: AuthTemporaryFailure}
		}
		// Out-parameters first, then the return value.
		out.String(res.Name)
		out.Strings(res.Groups)
		out.Int(res.UserID)
	case "getInfo":
		id := in.Int()
		if err := in.Err(); err != nil {
			return err
		}
		info, ok, err := s.a.Info(ctx, id)
		if err != nil || !ok {
			info, ok = nil, false
		}
		writeUserInfoMap(out, info)
		out.Bool(ok)
	case "nameToId":
		name := in.String()
		if err := in.Err(); err != nil {
			return err
		}
		id, err := s.a.NameToID(ctx, name)
		out.Int(intOr(id, err, Fallthrough))
	case "idToName":
		id := in.Int()
		if err := in.Err(); err != nil {
			return err
		}
		name, err := s.a.IDToName(ctx, id)
		if err != nil {
			name = ""
		}
		out.String(name)
	case "idToTexture":
		id := in.Int()
		if err := in.Err(); err != nil {
			return err
		}
		tex, err := s.a.IDToTexture(ctx, id)
		if err != nil {
			tex = nil
		}
		out.ByteSeq(tex)
	default:
		if s.ua == nil {
			return iceproto.ErrOperationNotExist
		}
		return s.dispatchUpdating(ctx, cur, in, out, intOr)
	}
	return nil
}

func (s authServant) dispatchUpdating(ctx context.Context, cur *iceproto.Current, in *iceproto.Decoder, out *iceproto.Encoder,
	intOr func(int32, error, int32) int32) error {
	switch cur.Operation {
	case "registerUser":
		info := readUserInfoMap(in)
		if err := in.Err(); err != nil {
			return err
		}
		id, err := s.ua.RegisterUser(ctx, info)
		out.Int(intOr(id, err, Fallthrough))
	case "unregisterUser":
		id := in.Int()
		if err := in.Err(); err != nil {
			return err
		}
		res, err := s.ua.UnregisterUser(ctx, id)
		out.Int(intOr(res, err, Fallthrough))
	case "getRegisteredUsers":
		filter := in.String()
		if err := in.Err(); err != nil {
			return err
		}
		m, err := s.ua.RegisteredUsers(ctx, filter)
		if err != nil {
			m = nil
		}
		writeNameMap(out, m)
	case "setInfo":
		id := in.Int()
		info := readUserInfoMap(in)
		if err := in.Err(); err != nil {
			return err
		}
		res, err := s.ua.SetInfo(ctx, id, info)
		out.Int(intOr(res, err, Fallthrough))
	case "setTexture":
		id := in.Int()
		tex := in.ByteSeq()
		if err := in.Err(); err != nil {
			return err
		}
		res, err := s.ua.SetTexture(ctx, id, tex)
		out.Int(intOr(res, err, Fallthrough))
	default:
		return iceproto.ErrOperationNotExist
	}
	return nil
}

// SetAuthenticator installs a as the server's authenticator, replacing any
// other. If a also implements [UpdatingAuthenticator], Murmur uses that too.
// It needs the write secret, and the server must be running.
//
// Murmur forgets the authenticator when the server stops and whenever a call
// to it fails, which includes this client going away. Close the handle to
// stop answering; Murmur then drops the authenticator at its next call,
// which falls through to its own database.
func (s *Server) SetAuthenticator(ctx context.Context, a Authenticator) (*Handle, error) {
	servant := authServant{a: a}
	if ua, ok := a.(UpdatingAuthenticator); ok {
		servant.ua = ua
	}
	h, proxy, err := s.c.export(ctx, servant)
	if err != nil {
		return nil, err
	}
	if err := s.call(ctx, "setAuthenticator", normal, func(e *iceproto.Encoder) { e.Proxy(proxy) }, nil); err != nil {
		h.Close(ctx)
		return nil, err
	}
	return h, nil
}
