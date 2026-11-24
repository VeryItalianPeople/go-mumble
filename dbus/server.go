package dbus

import (
	"context"
	"fmt"

	godbus "github.com/godbus/dbus/v5"
)

// Server is a handle on one virtual server. Getting one makes no call; a
// server that isn't running answers every method with [ErrNoServer].
//
// User IDs are the IDs of registered users (0 is SuperUser). Sessions
// identify connections, registered or not, and change on every reconnect.
type Server struct {
	c   *Client
	id  int32
	obj godbus.BusObject
}

// Server returns a handle on the virtual server with the given ID. Murmur
// starts with a single server, ID 1.
func (c *Client) Server(id int32) *Server {
	return &Server{c: c, id: id, obj: c.conn.Object(c.service, serverPath(id))}
}

func serverPath(id int32) godbus.ObjectPath {
	return godbus.ObjectPath(fmt.Sprintf("/%d", id))
}

// ID returns the virtual server's ID.
func (s *Server) ID() int32 {
	return s.id
}

func (s *Server) call(ctx context.Context, method string, args []any, out ...any) error {
	return call(ctx, s.obj, serverInterface, method, args, out...)
}

// Players returns the users connected to the server. Connections still
// authenticating are left out.
func (s *Server) Players(ctx context.Context) ([]Player, error) {
	var w []wirePlayer
	if err := s.call(ctx, "getPlayers", nil, &w); err != nil {
		return nil, err
	}
	return mapSlice(w, wirePlayer.player), nil
}

// Channels returns every channel, the root first, in breadth-first order.
func (s *Server) Channels(ctx context.Context) ([]Channel, error) {
	var w []wireChannel
	if err := s.call(ctx, "getChannels", nil, &w); err != nil {
		return nil, err
	}
	return mapSlice(w, wireChannel.channel), nil
}

// ACL returns a channel's ACL rules and groups, including the rules and
// groups it inherits, which are marked Inherited. Murmur lists the parent's
// rules even when Inherit is false, although they don't apply then.
func (s *Server) ACL(ctx context.Context, channelID int32) (ACL, error) {
	var (
		entries []wireACL
		groups  []wireGroup
		inherit bool
	)
	if err := s.call(ctx, "getACL", []any{channelID}, &entries, &groups, &inherit); err != nil {
		return ACL{}, err
	}
	return ACL{
		Inherit: inherit,
		Entries: mapSlice(entries, wireACL.entry),
		Groups:  mapSlice(groups, wireGroup.group),
	}, nil
}

// SetACL replaces a channel's ACL rules and groups. Entries and groups marked
// Inherited are skipped, so the result of [Server.ACL] can be changed and
// written back: Murmur itself would store an inherited rule as a new rule of
// this channel. Murmur deletes the channel's groups and recreates the ones in
// acl.Groups, keeping their temporary members ([Server.SetTemporaryGroups]);
// a group left out loses them.
func (s *Server) SetACL(ctx context.Context, channelID int32, acl ACL) error {
	entries := make([]wireACL, 0, len(acl.Entries))
	for _, e := range acl.Entries {
		if !e.Inherited {
			entries = append(entries, e.wire())
		}
	}
	groups := make([]wireGroup, 0, len(acl.Groups))
	for _, g := range acl.Groups {
		if !g.Inherited {
			groups = append(groups, g.wire())
		}
	}
	return s.call(ctx, "setACL", []any{channelID, entries, groups, acl.Inherit})
}

// Bans returns the server's IPv4 bans. IPv6 bans exist but Murmur leaves them
// out of the D-Bus interface.
func (s *Server) Bans(ctx context.Context) ([]Ban, error) {
	var w []wireBan
	if err := s.call(ctx, "getBans", nil, &w); err != nil {
		return nil, err
	}
	return mapSlice(w, wireBan.ban), nil
}

// SetBans is part of Murmur's D-Bus interface but does nothing: every Murmur
// release implements it as an empty function. It is here so the interface is
// complete; it returns nil and changes no ban.
func (s *Server) SetBans(ctx context.Context, bans []Ban) error {
	return s.call(ctx, "setBans", []any{mapSlice(bans, Ban.wire)})
}

// KickPlayer disconnects a user. The reason is shown to everyone on the
// server.
func (s *Server) KickPlayer(ctx context.Context, session uint32, reason string) error {
	return s.call(ctx, "kickPlayer", []any{session, reason})
}

// PlayerState returns the state of a connected user.
func (s *Server) PlayerState(ctx context.Context, session uint32) (PlayerState, error) {
	var w wirePlayerState
	if err := s.call(ctx, "getPlayerState", []any{session}, &w); err != nil {
		return PlayerState{}, err
	}
	return w.state(), nil
}

// SetPlayerState changes a connected user's state. Murmur applies Mute, Deaf,
// Suppressed and Channel (which moves the user) and ignores SelfMute and
// SelfDeaf, which only the user can change.
func (s *Server) SetPlayerState(ctx context.Context, state PlayerState) error {
	return s.call(ctx, "setPlayerState", []any{state.wire()})
}

// SendMessage sends a text message to one user. Murmur sends the text as is;
// Mumble clients display it as HTML.
func (s *Server) SendMessage(ctx context.Context, session uint32, text string) error {
	return s.call(ctx, "sendMessage", []any{session, text})
}

// SendMessageChannel sends a text message to a channel, and to its
// subchannels too when tree is true.
func (s *Server) SendMessageChannel(ctx context.Context, channelID int32, tree bool, text string) error {
	return s.call(ctx, "sendMessageChannel", []any{channelID, tree, text})
}

// ChannelState returns one channel.
func (s *Server) ChannelState(ctx context.Context, channelID int32) (Channel, error) {
	var w wireChannel
	if err := s.call(ctx, "getChannelState", []any{channelID}, &w); err != nil {
		return Channel{}, err
	}
	return w.channel(), nil
}

// SetChannelState renames, moves and relinks the channel with ID ch.ID.
// Links replaces the channel's links. Moving a channel under one of its own
// subchannels fails with [ErrInvalidChannel].
func (s *Server) SetChannelState(ctx context.Context, ch Channel) error {
	return s.call(ctx, "setChannelState", []any{ch.wire()})
}

// AddChannel creates a channel under parentID and returns its ID. Connected
// clients see the new channel, but Murmur emits no ChannelCreated signal for
// it: it only does for channels clients create.
func (s *Server) AddChannel(ctx context.Context, name string, parentID int32) (int32, error) {
	var id int32
	err := s.call(ctx, "addChannel", []any{name, parentID}, &id)
	return id, err
}

// RemoveChannel returns [ErrDeadlock] without calling Murmur.
//
// Murmur's removeChannel takes the server's voice-thread lock for writing and
// then calls a function that takes the same lock again. The lock is not
// recursive, so the daemon's main thread blocks on itself for good: every
// virtual server on it stops answering, on D-Bus and to clients alike. Every
// release with D-Bus has this (seen happening on 1.3.4 and 1.5.735; 1.4 has
// the same code). To delete a channel, have a Mumble client with Write
// permission on it do it, or use Murmur's Ice interface.
func (s *Server) RemoveChannel(ctx context.Context, channelID int32) error {
	return fmt.Errorf("murmur: removeChannel(%d): %w", channelID, ErrDeadlock)
}

// PlayerNames resolves registered user IDs to names. The result has one entry
// per ID, an empty string for an ID that isn't registered.
func (s *Server) PlayerNames(ctx context.Context, userIDs []int32) ([]string, error) {
	var names []string
	err := s.call(ctx, "getPlayerNames", []any{nonNil(userIDs)}, &names)
	return nonNil(names), err
}

// PlayerIDs resolves names to registered user IDs, ignoring case. The result
// has one entry per name, -2 for a name that isn't registered.
func (s *Server) PlayerIDs(ctx context.Context, names []string) ([]int32, error) {
	var ids []int32
	err := s.call(ctx, "getPlayerIds", []any{nonNil(names)}, &ids)
	return nonNil(ids), err
}

// SetTemporaryGroups puts a registered user in groups of a channel until the
// user disconnects, replacing any temporary groups it had there. Groups that
// don't exist are created.
//
// Murmur then means to clear the user's cached permissions, but looks the
// user ID up as a session ID, so it clears someone else's. A user whose
// permissions in that channel were already checked keeps the old ones until
// the cache is cleared another way: [Server.SetACL] on any channel clears it
// for everyone.
func (s *Server) SetTemporaryGroups(ctx context.Context, channelID, userID int32, groups []string) error {
	return s.call(ctx, "setTemporaryGroups", []any{channelID, userID, nonNil(groups)})
}

// RegisterPlayer registers a new user and returns its ID. The name must be
// valid and not taken, or it fails with [ErrInvalidPlayerName]. Set a
// password with [Server.SetRegistration].
func (s *Server) RegisterPlayer(ctx context.Context, name string) (int32, error) {
	var id int32
	err := s.call(ctx, "registerPlayer", []any{name}, &id)
	return id, err
}

// UnregisterPlayer deletes a registered user.
func (s *Server) UnregisterPlayer(ctx context.Context, userID int32) error {
	return s.call(ctx, "unregisterPlayer", []any{userID})
}

// Registration returns a registered user. Password is always empty.
func (s *Server) Registration(ctx context.Context, userID int32) (Registration, error) {
	var w wireRegistration
	if err := s.call(ctx, "getRegistration", []any{userID}, &w); err != nil {
		return Registration{}, err
	}
	return w.registration(), nil
}

// RegisteredPlayers lists registered users whose name matches filter, which
// Murmur passes to an SQL LIKE: "%" and "_" are wildcards, the match is
// otherwise exact, and on SQLite it ignores ASCII case. An empty filter lists
// everyone. Only ID and Name are set in the result.
func (s *Server) RegisteredPlayers(ctx context.Context, filter string) ([]Registration, error) {
	var w []wireRegistration
	if err := s.call(ctx, "getRegisteredPlayers", []any{filter}, &w); err != nil {
		return nil, err
	}
	return mapSlice(w, wireRegistration.registration), nil
}

// UpdateRegistration changes a registered user. Empty fields are left as they
// are, so a field can't be cleared this way. It fails with
// [ErrInvalidPlayerID] when every field is empty or when the new name belongs
// to someone else. Murmur doesn't check that the user exists: for an unknown
// ID it changes nothing and reports success.
func (s *Server) UpdateRegistration(ctx context.Context, r Registration) error {
	return s.call(ctx, "updateRegistration", []any{r.wire()})
}

// SetRegistration is [Server.UpdateRegistration] with the fields as
// arguments.
func (s *Server) SetRegistration(ctx context.Context, userID int32, name, email, password string) error {
	return s.call(ctx, "setRegistration", []any{userID, name, email, password})
}

// VerifyPassword reports whether password is the password of the registered
// user.
func (s *Server) VerifyPassword(ctx context.Context, userID int32, password string) (bool, error) {
	var ok bool
	err := s.call(ctx, "verifyPassword", []any{userID, password}, &ok)
	return ok, err
}

// Texture returns a registered user's avatar image as Murmur stores it, or
// an empty slice for none.
func (s *Server) Texture(ctx context.Context, userID int32) ([]byte, error) {
	var tex []byte
	err := s.call(ctx, "getTexture", []any{userID}, &tex)
	return nonNil(tex), err
}

// SetTexture sets a registered user's avatar image. Murmur stores the bytes as
// given, except that it compresses a raw 600×60 BGRA image (144,000 bytes)
// first.
func (s *Server) SetTexture(ctx context.Context, userID int32, texture []byte) error {
	return s.call(ctx, "setTexture", []any{userID, nonNil(texture)})
}
