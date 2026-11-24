package ice

import (
	"context"
	"errors"
	"time"

	"github.com/VeryItalianPeople/go-mumble/internal/iceproto"
)

// Server is a handle on one virtual server. Getting one makes no call. Most
// operations on a server that exists but isn't running fail with
// [ErrServerBooted]; on one that doesn't exist, with [ErrServerNotFound].
//
// Sessions identify connections, registered or not, and change on every
// reconnect; user IDs identify registered users (0 is SuperUser). Murmur's
// Slice names some session parameters "userid"; the methods here say which
// one each takes.
type Server struct {
	c     *Client
	id    int32
	ident iceproto.Identity
}

// ID returns the virtual server's ID.
func (s *Server) ID() int32 { return s.id }

func (s *Server) call(ctx context.Context, op string, mode byte, write func(*iceproto.Encoder), read func(*iceproto.Decoder)) error {
	return s.c.call(ctx, s.ident, op, mode, write, read)
}

// lastStoppedCrashBuild is the last Murmur 1.6 build whose getConf, getAllConf,
// setConf, setSuperuserPassword, getLog and getLogLen crash the daemon
// (SIGSEGV, every virtual server with it) when the server exists but isn't
// running: mumble-voip/mumble#7449, fixed by #7487 after 1.6.1001
// (v1.6.0-alpha2) was built. Murmur reports the build as the patch number.
const lastStoppedCrashBuild = 1001

// on16 reports whether the guards apply: the daemon is Murmur 1.6 and they
// haven't been turned off.
func (s *Server) on16(ctx context.Context) (Version, bool, error) {
	if s.c.noGuards {
		return Version{}, false, nil
	}
	v, err := s.c.cachedVersion(ctx)
	if err != nil {
		return Version{}, false, err
	}
	return v, v.Major == 1 && v.Minor == 6, nil
}

// guardStopped makes the calls listed at lastStoppedCrashBuild first ask
// whether the server is running, on the builds that crash, and refuse with
// ErrWouldCrash if it isn't.
func (s *Server) guardStopped(ctx context.Context, op string) error {
	v, guard, err := s.on16(ctx)
	if err != nil || !guard || v.Patch > lastStoppedCrashBuild {
		return err
	}
	running, err := s.IsRunning(ctx)
	if err != nil || running {
		return err
	}
	// A server that doesn't exist at all is answered safely.
	if _, err := s.c.GetServer(ctx, s.id); err != nil {
		return err
	}
	return &Error{Operation: op, Err: errors.New("the server is stopped"), sentinel: ErrWouldCrash}
}

func intArg(n int32) func(*iceproto.Encoder) {
	return func(e *iceproto.Encoder) { e.Int(n) }
}

// IsRunning reports whether the server is running.
func (s *Server) IsRunning(ctx context.Context) (bool, error) {
	var ok bool
	err := s.call(ctx, "isRunning", idempotent, nil, func(d *iceproto.Decoder) { ok = d.Bool() })
	return ok, err
}

// Start starts the server.
func (s *Server) Start(ctx context.Context) error {
	return s.call(ctx, "start", normal, nil, nil)
}

// Stop stops the server.
func (s *Server) Stop(ctx context.Context) error {
	return s.call(ctx, "stop", normal, nil, nil)
}

// Delete deletes a stopped server and all of its data.
func (s *Server) Delete(ctx context.Context) error {
	return s.call(ctx, "delete", normal, nil, nil)
}

// QueryID asks Murmur for the server's ID; [Server.ID] knows it already.
func (s *Server) QueryID(ctx context.Context) (int32, error) {
	var id int32
	err := s.call(ctx, "id", idempotent, nil, func(d *iceproto.Decoder) { id = d.Int() })
	return id, err
}

// Conf returns a configuration value stored for the server, or "" when it
// uses the default. "key" and "passphrase" are write-only ([ErrWriteOnly]).
//
// Murmur 1.6 up to build 1001 crashes when this, [Server.AllConf],
// [Server.SetConf], [Server.SetSuperuserPassword], [Server.Log] or
// [Server.LogLen] is called on a server that exists but isn't running
// (mumble-voip/mumble#7449, fixed in later builds). On those builds these
// methods check first and return [ErrWouldCrash] instead;
// [WithoutCrashGuards] turns the check off.
func (s *Server) Conf(ctx context.Context, key string) (string, error) {
	if err := s.guardStopped(ctx, "getConf"); err != nil {
		return "", err
	}
	var v string
	err := s.call(ctx, "getConf", idempotent, func(e *iceproto.Encoder) { e.String(key) }, func(d *iceproto.Decoder) { v = d.String() })
	return v, err
}

// AllConf returns every configuration value stored for the server.
func (s *Server) AllConf(ctx context.Context) (map[string]string, error) {
	if err := s.guardStopped(ctx, "getAllConf"); err != nil {
		return nil, err
	}
	var m map[string]string
	err := s.call(ctx, "getAllConf", idempotent, nil, func(d *iceproto.Decoder) { m = d.Context() })
	return m, err
}

// SetConf stores a configuration value for the server.
func (s *Server) SetConf(ctx context.Context, key, value string) error {
	if err := s.guardStopped(ctx, "setConf"); err != nil {
		return err
	}
	return s.call(ctx, "setConf", idempotent, func(e *iceproto.Encoder) { e.String(key); e.String(value) }, nil)
}

// SetSuperuserPassword sets the password of the SuperUser account.
func (s *Server) SetSuperuserPassword(ctx context.Context, password string) error {
	if err := s.guardStopped(ctx, "setSuperuserPassword"); err != nil {
		return err
	}
	return s.call(ctx, "setSuperuserPassword", idempotent, func(e *iceproto.Encoder) { e.String(password) }, nil)
}

// Log returns up to count lines of the server's log, newest first, skipping
// the newest offset lines.
//
// Murmur's getLog takes (first, last), but 1.5 passes last to the database
// as a count while 1.6 reads it as an end index. Log asks for the daemon's
// version once and translates, so it behaves the same on both.
func (s *Server) Log(ctx context.Context, offset, count int32) ([]LogEntry, error) {
	if err := s.guardStopped(ctx, "getLog"); err != nil {
		return nil, err
	}
	v, err := s.c.cachedVersion(ctx)
	if err != nil {
		return nil, err
	}
	last := count
	if v.atLeast(1, 6) {
		last = offset + count
	}
	var out []LogEntry
	err = s.call(ctx, "getLog", idempotent, func(e *iceproto.Encoder) { e.Int(offset); e.Int(last) }, func(d *iceproto.Decoder) {
		n := d.Size()
		for i := 0; i < n && d.Err() == nil; i++ {
			out = append(out, LogEntry{Time: time.Unix(int64(d.Int()), 0), Text: d.String()})
		}
	})
	return out, err
}

// LogLen returns how many lines the server's log has.
func (s *Server) LogLen(ctx context.Context) (int32, error) {
	if err := s.guardStopped(ctx, "getLogLen"); err != nil {
		return 0, err
	}
	var n int32
	err := s.call(ctx, "getLogLen", idempotent, nil, func(d *iceproto.Decoder) { n = d.Int() })
	return n, err
}

// Users returns the connected users by session.
func (s *Server) Users(ctx context.Context) (map[int32]User, error) {
	var m map[int32]User
	err := s.call(ctx, "getUsers", idempotent, nil, func(d *iceproto.Decoder) {
		n := d.Size()
		m = make(map[int32]User, n)
		for i := 0; i < n && d.Err() == nil; i++ {
			k := d.Int()
			m[k] = readUser(d)
		}
	})
	return m, err
}

// Channels returns every channel by ID.
func (s *Server) Channels(ctx context.Context) (map[int32]Channel, error) {
	var m map[int32]Channel
	err := s.call(ctx, "getChannels", idempotent, nil, func(d *iceproto.Decoder) {
		n := d.Size()
		m = make(map[int32]Channel, n)
		for i := 0; i < n && d.Err() == nil; i++ {
			k := d.Int()
			m[k] = readChannel(d)
		}
	})
	return m, err
}

// CertificateList returns the certificate chain a session's client
// presented, DER-encoded, leaf first. It is empty for a client without a
// certificate.
func (s *Server) CertificateList(ctx context.Context, session int32) ([][]byte, error) {
	var certs [][]byte
	err := s.call(ctx, "getCertificateList", idempotent, intArg(session), func(d *iceproto.Decoder) {
		n := d.Size()
		for i := 0; i < n && d.Err() == nil; i++ {
			certs = append(certs, d.ByteSeq())
		}
	})
	return certs, err
}

// Tree returns the channel tree with the users in each channel.
func (s *Server) Tree(ctx context.Context) (*Tree, error) {
	var t *Tree
	err := s.call(ctx, "getTree", idempotent, nil, func(d *iceproto.Decoder) { t = readTree(d) })
	return t, err
}

// Bans returns the server's bans, IPv4 and IPv6.
func (s *Server) Bans(ctx context.Context) ([]Ban, error) {
	var bans []Ban
	err := s.call(ctx, "getBans", idempotent, nil, func(d *iceproto.Decoder) {
		n := d.Size()
		for i := 0; i < n && d.Err() == nil; i++ {
			bans = append(bans, readBan(d))
		}
	})
	return bans, err
}

// SetBans replaces the server's bans.
func (s *Server) SetBans(ctx context.Context, bans []Ban) error {
	return s.call(ctx, "setBans", idempotent, func(e *iceproto.Encoder) {
		e.Size(len(bans))
		for _, b := range bans {
			writeBan(e, b)
		}
	}, nil)
}

// KickUser disconnects a session with a reason.
func (s *Server) KickUser(ctx context.Context, session int32, reason string) error {
	return s.call(ctx, "kickUser", normal, func(e *iceproto.Encoder) { e.Int(session); e.String(reason) }, nil)
}

// State returns a connected user.
func (s *Server) State(ctx context.Context, session int32) (User, error) {
	var u User
	err := s.call(ctx, "getState", idempotent, intArg(session), func(d *iceproto.Decoder) { u = readUser(d) })
	return u, err
}

// SetState changes the user with u.Session. Murmur applies Mute, Deaf,
// Suppress, PrioritySpeaker, Channel (which moves the user), Name and
// Comment, and ignores the rest.
func (s *Server) SetState(ctx context.Context, u User) error {
	return s.call(ctx, "setState", idempotent, func(e *iceproto.Encoder) { writeUser(e, u) }, nil)
}

// SendMessage sends a text message to one session.
func (s *Server) SendMessage(ctx context.Context, session int32, text string) error {
	return s.call(ctx, "sendMessage", normal, func(e *iceproto.Encoder) { e.Int(session); e.String(text) }, nil)
}

// HasPermission reports whether a session has a permission in a channel.
func (s *Server) HasPermission(ctx context.Context, session, channelID int32, perm Permission) (bool, error) {
	var ok bool
	err := s.call(ctx, "hasPermission", normal, func(e *iceproto.Encoder) {
		e.Int(session)
		e.Int(channelID)
		e.Int(int32(perm))
	}, func(d *iceproto.Decoder) { ok = d.Bool() })
	return ok, err
}

// EffectivePermissions returns a session's permissions in a channel.
func (s *Server) EffectivePermissions(ctx context.Context, session, channelID int32) (Permission, error) {
	var p int32
	err := s.call(ctx, "effectivePermissions", idempotent, func(e *iceproto.Encoder) { e.Int(session); e.Int(channelID) }, func(d *iceproto.Decoder) { p = d.Int() })
	return Permission(p), err
}

// ChannelState returns one channel.
func (s *Server) ChannelState(ctx context.Context, channelID int32) (Channel, error) {
	var c Channel
	err := s.call(ctx, "getChannelState", idempotent, intArg(channelID), func(d *iceproto.Decoder) { c = readChannel(d) })
	return c, err
}

// SetChannelState changes the channel with ch.ID: name, parent, links,
// description and position.
func (s *Server) SetChannelState(ctx context.Context, ch Channel) error {
	return s.call(ctx, "setChannelState", idempotent, func(e *iceproto.Encoder) { writeChannel(e, ch) }, nil)
}

// RemoveChannel deletes a channel and its subchannels. Unlike the D-Bus
// call of the same name, this one is safe.
func (s *Server) RemoveChannel(ctx context.Context, channelID int32) error {
	return s.call(ctx, "removeChannel", normal, intArg(channelID), nil)
}

// AddChannel creates a channel under parentID and returns its ID.
func (s *Server) AddChannel(ctx context.Context, name string, parentID int32) (int32, error) {
	var id int32
	err := s.call(ctx, "addChannel", normal, func(e *iceproto.Encoder) { e.String(name); e.Int(parentID) }, func(d *iceproto.Decoder) { id = d.Int() })
	return id, err
}

// SendMessageChannel sends a text message to a channel, and to its
// subchannels when tree is true.
func (s *Server) SendMessageChannel(ctx context.Context, channelID int32, tree bool, text string) error {
	return s.call(ctx, "sendMessageChannel", normal, func(e *iceproto.Encoder) {
		e.Int(channelID)
		e.Bool(tree)
		e.String(text)
	}, nil)
}

// ACL returns a channel's ACL rules and groups, including inherited ones,
// which are marked Inherited.
func (s *Server) ACL(ctx context.Context, channelID int32) (ACL, error) {
	var acl ACL
	err := s.call(ctx, "getACL", idempotent, intArg(channelID), func(d *iceproto.Decoder) {
		n := d.Size()
		for i := 0; i < n && d.Err() == nil; i++ {
			acl.Entries = append(acl.Entries, readACLEntry(d))
		}
		n = d.Size()
		for i := 0; i < n && d.Err() == nil; i++ {
			acl.Groups = append(acl.Groups, readGroup(d))
		}
		acl.Inherit = d.Bool()
	})
	return acl, err
}

// SetACL replaces a channel's ACL rules and groups. Entries and groups marked
// Inherited are skipped, because Murmur would store them as rules of this
// channel, so the result of [Server.ACL] can be changed and written back.
func (s *Server) SetACL(ctx context.Context, channelID int32, acl ACL) error {
	return s.call(ctx, "setACL", idempotent, func(e *iceproto.Encoder) {
		e.Int(channelID)
		var entries []ACLEntry
		for _, a := range acl.Entries {
			if !a.Inherited {
				entries = append(entries, a)
			}
		}
		e.Size(len(entries))
		for _, a := range entries {
			writeACLEntry(e, a)
		}
		var groups []Group
		for _, g := range acl.Groups {
			if !g.Inherited {
				groups = append(groups, g)
			}
		}
		e.Size(len(groups))
		for _, g := range groups {
			writeGroup(e, g)
		}
		e.Bool(acl.Inherit)
	}, nil)
}

// AddUserToGroup puts a session in a group of a channel until it
// disconnects. The group is created if needed.
func (s *Server) AddUserToGroup(ctx context.Context, channelID, session int32, group string) error {
	return s.call(ctx, "addUserToGroup", idempotent, func(e *iceproto.Encoder) {
		e.Int(channelID)
		e.Int(session)
		e.String(group)
	}, nil)
}

// RemoveUserFromGroup takes a session out of a group it was put in with
// [Server.AddUserToGroup].
func (s *Server) RemoveUserFromGroup(ctx context.Context, channelID, session int32, group string) error {
	return s.call(ctx, "removeUserFromGroup", idempotent, func(e *iceproto.Encoder) {
		e.Int(channelID)
		e.Int(session)
		e.String(group)
	}, nil)
}

// RedirectWhisperGroup makes a session's whispers to group source go to
// group target instead. An empty target removes the redirection.
func (s *Server) RedirectWhisperGroup(ctx context.Context, session int32, source, target string) error {
	return s.call(ctx, "redirectWhisperGroup", idempotent, func(e *iceproto.Encoder) {
		e.Int(session)
		e.String(source)
		e.String(target)
	}, nil)
}

// UserNames maps registered user IDs to names.
func (s *Server) UserNames(ctx context.Context, userIDs []int32) (map[int32]string, error) {
	var m map[int32]string
	err := s.call(ctx, "getUserNames", idempotent, func(e *iceproto.Encoder) { e.Ints(userIDs) }, func(d *iceproto.Decoder) { m = readNameMap(d) })
	return m, err
}

// UserIDs maps names to registered user IDs.
func (s *Server) UserIDs(ctx context.Context, names []string) (map[string]int32, error) {
	var m map[string]int32
	err := s.call(ctx, "getUserIds", idempotent, func(e *iceproto.Encoder) { e.Strings(names) }, func(d *iceproto.Decoder) {
		n := d.Size()
		m = make(map[string]int32, n)
		for i := 0; i < n && d.Err() == nil; i++ {
			k := d.String()
			m[k] = d.Int()
		}
	})
	return m, err
}

// RegisterUser registers a user and returns the new user ID. info must have
// a UserName; UserPassword, UserEmail and the rest are optional.
func (s *Server) RegisterUser(ctx context.Context, info UserInfoMap) (int32, error) {
	var id int32
	err := s.call(ctx, "registerUser", normal, func(e *iceproto.Encoder) { writeUserInfoMap(e, info) }, func(d *iceproto.Decoder) { id = d.Int() })
	return id, err
}

// UnregisterUser deletes a registered user.
func (s *Server) UnregisterUser(ctx context.Context, userID int32) error {
	return s.call(ctx, "unregisterUser", normal, intArg(userID), nil)
}

// UpdateRegistration sets the given fields of a registration and leaves the
// others alone.
func (s *Server) UpdateRegistration(ctx context.Context, userID int32, info UserInfoMap) error {
	return s.call(ctx, "updateRegistration", idempotent, func(e *iceproto.Encoder) {
		e.Int(userID)
		writeUserInfoMap(e, info)
	}, nil)
}

// Registration returns a registered user's fields.
func (s *Server) Registration(ctx context.Context, userID int32) (UserInfoMap, error) {
	var m UserInfoMap
	err := s.call(ctx, "getRegistration", idempotent, intArg(userID), func(d *iceproto.Decoder) { m = readUserInfoMap(d) })
	return m, err
}

// RegisteredUsers lists registered users whose name matches filter, by ID.
// An empty filter lists everyone.
func (s *Server) RegisteredUsers(ctx context.Context, filter string) (map[int32]string, error) {
	var m map[int32]string
	err := s.call(ctx, "getRegisteredUsers", idempotent, func(e *iceproto.Encoder) { e.String(filter) }, func(d *iceproto.Decoder) { m = readNameMap(d) })
	return m, err
}

// VerifyPassword checks a name and password the way a login would. It
// returns the user ID on success, AuthFailed for a wrong password and
// Fallthrough for a name that isn't registered.
func (s *Server) VerifyPassword(ctx context.Context, name, password string) (int32, error) {
	var id int32
	err := s.call(ctx, "verifyPassword", idempotent, func(e *iceproto.Encoder) { e.String(name); e.String(password) }, func(d *iceproto.Decoder) { id = d.Int() })
	return id, err
}

// Texture returns a registered user's avatar.
//
// Murmur 1.6 checks the user ID the wrong way round here and in
// [Server.SetTexture]: it fails with [ErrInvalidUser] for every registered
// user, and for an ID that isn't registered it returns an empty texture.
// Seen on 1.6.1001 and still in the source after it (mumble-voip/mumble#7522).
func (s *Server) Texture(ctx context.Context, userID int32) ([]byte, error) {
	var tex []byte
	err := s.call(ctx, "getTexture", idempotent, intArg(userID), func(d *iceproto.Decoder) { tex = d.ByteSeq() })
	return tex, err
}

// SetTexture sets a registered user's avatar.
//
// On Murmur 1.6 the inverted check described at [Server.Texture] lets an
// unregistered ID through, and Murmur then terminates with "Database
// error". On 1.6, SetTexture first checks the user is registered and
// returns [ErrWouldCrash] if not; for a registered user Murmur fails with
// [ErrInvalidUser].
func (s *Server) SetTexture(ctx context.Context, userID int32, texture []byte) error {
	_, guard, err := s.on16(ctx)
	if err != nil {
		return err
	}
	if guard {
		if _, err := s.Registration(ctx, userID); errors.Is(err, ErrInvalidUser) {
			return &Error{Operation: "setTexture", Err: errors.New("the user is not registered"), sentinel: ErrWouldCrash}
		} else if err != nil {
			return err
		}
	}
	return s.call(ctx, "setTexture", idempotent, func(e *iceproto.Encoder) { e.Int(userID); e.ByteSeq(texture) }, nil)
}

// Uptime returns how long the server has been running.
func (s *Server) Uptime(ctx context.Context) (time.Duration, error) {
	var n int32
	err := s.call(ctx, "getUptime", idempotent, nil, func(d *iceproto.Decoder) { n = d.Int() })
	return time.Duration(n) * time.Second, err
}

// UpdateCertificate replaces the server's TLS certificate and key, both
// PEM-encoded. passphrase unlocks the key, or is empty. Murmur fails with
// [ErrInvalidInputData] when it can't load them or they don't match.
func (s *Server) UpdateCertificate(ctx context.Context, certificatePEM, privateKeyPEM, passphrase string) error {
	return s.call(ctx, "updateCertificate", idempotent, func(e *iceproto.Encoder) {
		e.String(certificatePEM)
		e.String(privateKeyPEM)
		e.String(passphrase)
	}, nil)
}

// StartListening makes a session listen to a channel it isn't in.
func (s *Server) StartListening(ctx context.Context, session, channelID int32) error {
	return s.call(ctx, "startListening", idempotent, func(e *iceproto.Encoder) { e.Int(session); e.Int(channelID) }, nil)
}

// StopListening ends [Server.StartListening].
func (s *Server) StopListening(ctx context.Context, session, channelID int32) error {
	return s.call(ctx, "stopListening", idempotent, func(e *iceproto.Encoder) { e.Int(session); e.Int(channelID) }, nil)
}

// IsListening reports whether a session listens to a channel.
func (s *Server) IsListening(ctx context.Context, session, channelID int32) (bool, error) {
	var ok bool
	err := s.call(ctx, "isListening", idempotent, func(e *iceproto.Encoder) { e.Int(session); e.Int(channelID) }, func(d *iceproto.Decoder) { ok = d.Bool() })
	return ok, err
}

// ListeningChannels returns the channels a session listens to.
func (s *Server) ListeningChannels(ctx context.Context, session int32) ([]int32, error) {
	var ids []int32
	err := s.call(ctx, "getListeningChannels", idempotent, intArg(session), func(d *iceproto.Decoder) { ids = d.Ints() })
	return ids, err
}

// ListeningUsers returns the sessions listening to a channel.
func (s *Server) ListeningUsers(ctx context.Context, channelID int32) ([]int32, error) {
	var ids []int32
	err := s.call(ctx, "getListeningUsers", idempotent, intArg(channelID), func(d *iceproto.Decoder) { ids = d.Ints() })
	return ids, err
}

// ListenerVolumeAdjustment returns the volume factor a session hears a
// listened channel at (1 is unchanged).
func (s *Server) ListenerVolumeAdjustment(ctx context.Context, channelID, session int32) (float32, error) {
	var f float32
	err := s.call(ctx, "getListenerVolumeAdjustment", idempotent, func(e *iceproto.Encoder) { e.Int(channelID); e.Int(session) }, func(d *iceproto.Decoder) { f = d.Float() })
	return f, err
}

// SetListenerVolumeAdjustment sets the volume factor a session hears a
// listened channel at.
func (s *Server) SetListenerVolumeAdjustment(ctx context.Context, channelID, session int32, factor float32) error {
	return s.call(ctx, "setListenerVolumeAdjustment", idempotent, func(e *iceproto.Encoder) {
		e.Int(channelID)
		e.Int(session)
		e.Float(factor)
	}, nil)
}

// SendWelcomeMessage sends the server's welcome text again to the given
// sessions.
func (s *Server) SendWelcomeMessage(ctx context.Context, sessions []int32) error {
	return s.call(ctx, "sendWelcomeMessage", idempotent, func(e *iceproto.Encoder) { e.Ints(sessions) }, nil)
}
