package dbus

import (
	"fmt"
	"net/netip"
	"strings"
	"time"
)

// PlayerState is the state of a connected user, keyed by session. It is what
// Murmur sends with the player signals and what [Server.PlayerState] returns.
type PlayerState struct {
	// Session identifies the connection. It is only valid while the user
	// stays connected; a reconnect gets a new one.
	Session    uint32
	Mute       bool // muted by an admin
	Deaf       bool // deafened by an admin
	Suppressed bool // muted by the server, e.g. for lacking Speak permission
	SelfMute   bool
	SelfDeaf   bool
	Channel    int32 // ID of the channel the user is in
}

// Player is a connected user as listed by [Server.Players].
type Player struct {
	PlayerState
	// UserID is the registered user ID, or -1 for a user that is not
	// registered.
	UserID int32
	Name   string
	// OnlineSeconds is how long the user has been connected.
	OnlineSeconds int32
	// BytesPerSecond is the user's average bandwidth.
	BytesPerSecond int32
}

// Online returns OnlineSeconds as a duration.
func (p Player) Online() time.Duration {
	return time.Duration(p.OnlineSeconds) * time.Second
}

// Channel is a channel on a virtual server. The root channel has ID 0 and
// Parent -1.
type Channel struct {
	ID     int32
	Name   string
	Parent int32
	// Links lists the IDs of the channels this one is linked to.
	Links []int32
}

// Permission is a set of ACL permission bits, as used in [ACLEntry].
type Permission uint32

// The permission bits Murmur understands. Bits Murmur doesn't know are dropped
// when an ACL is written.
const (
	PermWrite           Permission = 0x1
	PermTraverse        Permission = 0x2
	PermEnter           Permission = 0x4
	PermSpeak           Permission = 0x8
	PermMuteDeafen      Permission = 0x10
	PermMove            Permission = 0x20
	PermMakeChannel     Permission = 0x40
	PermLinkChannel     Permission = 0x80
	PermWhisper         Permission = 0x100
	PermTextMessage     Permission = 0x200
	PermMakeTempChannel Permission = 0x400
	PermListen          Permission = 0x800 // Murmur 1.4 and later

	// The following only have an effect on the root channel.

	PermKick             Permission = 0x10000
	PermBan              Permission = 0x20000
	PermRegister         Permission = 0x40000
	PermSelfRegister     Permission = 0x80000
	PermResetUserContent Permission = 0x100000 // Murmur 1.4 and later
)

var permissionNames = []struct {
	p    Permission
	name string
}{
	{PermWrite, "Write"},
	{PermTraverse, "Traverse"},
	{PermEnter, "Enter"},
	{PermSpeak, "Speak"},
	{PermMuteDeafen, "MuteDeafen"},
	{PermMove, "Move"},
	{PermMakeChannel, "MakeChannel"},
	{PermLinkChannel, "LinkChannel"},
	{PermWhisper, "Whisper"},
	{PermTextMessage, "TextMessage"},
	{PermMakeTempChannel, "MakeTempChannel"},
	{PermListen, "Listen"},
	{PermKick, "Kick"},
	{PermBan, "Ban"},
	{PermRegister, "Register"},
	{PermSelfRegister, "SelfRegister"},
	{PermResetUserContent, "ResetUserContent"},
}

// Has reports whether every bit in q is set in p.
func (p Permission) Has(q Permission) bool {
	return p&q == q
}

// String lists the set bits by name, joined with "|". Unknown bits are
// printed in hex.
func (p Permission) String() string {
	if p == 0 {
		return "None"
	}
	var parts []string
	rest := p
	for _, n := range permissionNames {
		if p&n.p != 0 {
			parts = append(parts, n.name)
			rest &^= n.p
		}
	}
	if rest != 0 {
		parts = append(parts, fmt.Sprintf("%#x", uint32(rest)))
	}
	return strings.Join(parts, "|")
}

// ACLEntry is one rule of a channel's access control list. A rule applies
// either to a single user (UserID >= 0, Group empty) or to a group (UserID
// -1).
type ACLEntry struct {
	ApplyHere bool // the rule applies to this channel
	ApplySubs bool // the rule applies to subchannels
	// Inherited is set on rules that come from a parent channel. Murmur
	// returns them from [Server.ACL] for reference; they are not written
	// back by [Server.SetACL].
	Inherited bool
	UserID    int32
	Group     string
	Allow     Permission
	Deny      Permission
}

// Group is a channel group.
type Group struct {
	Name string
	// Inherited is set when the group is defined on a parent channel and
	// not on this one. [Server.SetACL] skips such groups.
	Inherited   bool
	Inherit     bool // members are inherited from the parent channel's group of the same name
	Inheritable bool // subchannels may inherit this group
	Add         []int32
	Remove      []int32
	// Members is the effective member list: inherited members plus Add,
	// minus Remove. It is ignored by [Server.SetACL].
	Members []int32
}

// ACL is a channel's access control list and groups.
type ACL struct {
	// Inherit makes the channel inherit ACL rules from its parent.
	Inherit bool
	Entries []ACLEntry
	Groups  []Group
}

// Ban is an IPv4 address ban. Murmur's D-Bus interface only lists IPv4 bans.
type Ban struct {
	// Address is the banned IPv4 address.
	Address netip.Addr
	// Bits is the mask length the way Murmur stores it, counted over the
	// address as an IPv4-mapped IPv6 address: 128 bans a single IPv4
	// address, 120 bans a /24. Use [Ban.Prefix] for the IPv4 form.
	Bits int32
}

// Prefix returns the banned range as an IPv4 prefix. It returns the zero
// Prefix when Bits is outside 96..128.
func (b Ban) Prefix() netip.Prefix {
	if b.Bits < 96 || b.Bits > 128 || !b.Address.Is4() {
		return netip.Prefix{}
	}
	return netip.PrefixFrom(b.Address, int(b.Bits-96)).Masked()
}

// Registration is a registered user.
type Registration struct {
	ID    int32
	Name  string
	Email string
	// Password is only used when writing a registration. Murmur never
	// returns it.
	Password string
}

// LogEntry is a line of a virtual server's log.
type LogEntry struct {
	Time time.Time
	Text string
}

// Version is the version of the Murmur daemon.
type Version struct {
	Major, Minor, Patch int
	// Release is the full version string, e.g. "1.3.4".
	Release string
}

func (v Version) String() string {
	if v.Release != "" {
		return v.Release
	}
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Wire types. Their field order is the D-Bus struct signature Murmur uses,
// so it must not change.

type wirePlayerState struct { // (ubbbbbi)
	Session    uint32
	Mute       bool
	Deaf       bool
	Suppressed bool
	SelfMute   bool
	SelfDeaf   bool
	Channel    int32
}

type wirePlayer struct { // (ubbbbbiisii)
	Session        uint32
	Mute           bool
	Deaf           bool
	Suppressed     bool
	SelfMute       bool
	SelfDeaf       bool
	Channel        int32
	ID             int32
	Name           string
	OnlineSeconds  int32
	BytesPerSecond int32
}

type wireChannel struct { // (isiai)
	ID     int32
	Name   string
	Parent int32
	Links  []int32
}

type wireGroup struct { // (sbbbaiaiai)
	Name        string
	Inherited   bool
	Inherit     bool
	Inheritable bool
	Add         []int32
	Remove      []int32
	Members     []int32
}

type wireACL struct { // (bbbisuu)
	ApplyHere bool
	ApplySubs bool
	Inherited bool
	UserID    int32
	Group     string
	Allow     uint32
	Deny      uint32
}

type wireBan struct { // (ui)
	Address uint32
	Bits    int32
}

type wireRegistration struct { // (isss)
	ID       int32
	Name     string
	Email    string
	Password string
}

type wireLogEntry struct { // (us)
	Timestamp uint32
	Text      string
}

func (w wirePlayerState) state() PlayerState {
	return PlayerState(w)
}

func (s PlayerState) wire() wirePlayerState {
	return wirePlayerState(s)
}

func (w wirePlayer) player() Player {
	return Player{
		PlayerState: PlayerState{
			Session:    w.Session,
			Mute:       w.Mute,
			Deaf:       w.Deaf,
			Suppressed: w.Suppressed,
			SelfMute:   w.SelfMute,
			SelfDeaf:   w.SelfDeaf,
			Channel:    w.Channel,
		},
		UserID:         w.ID,
		Name:           w.Name,
		OnlineSeconds:  w.OnlineSeconds,
		BytesPerSecond: w.BytesPerSecond,
	}
}

func (w wireChannel) channel() Channel {
	return Channel{ID: w.ID, Name: w.Name, Parent: w.Parent, Links: nonNil(w.Links)}
}

func (c Channel) wire() wireChannel {
	return wireChannel{ID: c.ID, Name: c.Name, Parent: c.Parent, Links: nonNil(c.Links)}
}

func (w wireGroup) group() Group {
	return Group{
		Name:        w.Name,
		Inherited:   w.Inherited,
		Inherit:     w.Inherit,
		Inheritable: w.Inheritable,
		Add:         nonNil(w.Add),
		Remove:      nonNil(w.Remove),
		Members:     nonNil(w.Members),
	}
}

func (g Group) wire() wireGroup {
	return wireGroup{
		Name:        g.Name,
		Inherited:   g.Inherited,
		Inherit:     g.Inherit,
		Inheritable: g.Inheritable,
		Add:         nonNil(g.Add),
		Remove:      nonNil(g.Remove),
		Members:     nonNil(g.Members),
	}
}

func (w wireACL) entry() ACLEntry {
	return ACLEntry{
		ApplyHere: w.ApplyHere,
		ApplySubs: w.ApplySubs,
		Inherited: w.Inherited,
		UserID:    w.UserID,
		Group:     w.Group,
		Allow:     Permission(w.Allow),
		Deny:      Permission(w.Deny),
	}
}

func (e ACLEntry) wire() wireACL {
	return wireACL{
		ApplyHere: e.ApplyHere,
		ApplySubs: e.ApplySubs,
		Inherited: e.Inherited,
		UserID:    e.UserID,
		Group:     e.Group,
		Allow:     uint32(e.Allow),
		Deny:      uint32(e.Deny),
	}
}

// Murmur sends the address in host byte order: 127.0.0.1 is 0x7f000001.
func (w wireBan) ban() Ban {
	a := w.Address
	return Ban{
		Address: netip.AddrFrom4([4]byte{byte(a >> 24), byte(a >> 16), byte(a >> 8), byte(a)}),
		Bits:    w.Bits,
	}
}

func (b Ban) wire() wireBan {
	var a uint32
	if b.Address.Is4() || b.Address.Is4In6() {
		v := b.Address.Unmap().As4()
		a = uint32(v[0])<<24 | uint32(v[1])<<16 | uint32(v[2])<<8 | uint32(v[3])
	}
	return wireBan{Address: a, Bits: b.Bits}
}

func (w wireRegistration) registration() Registration {
	return Registration(w)
}

func (r Registration) wire() wireRegistration {
	return wireRegistration(r)
}

func (w wireLogEntry) entry() LogEntry {
	return LogEntry{Time: time.Unix(int64(w.Timestamp), 0), Text: w.Text}
}

// nonNil turns a nil slice into an empty one. godbus encodes both the same
// way, but callers comparing results shouldn't have to care which they got.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func mapSlice[T, U any](s []T, f func(T) U) []U {
	out := make([]U, len(s))
	for i, v := range s {
		out[i] = f(v)
	}
	return out
}
