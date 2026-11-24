package ice

import (
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/VeryItalianPeople/go-mumble/internal/iceproto"
)

// User is a connected user, as Murmur's Ice interface describes one.
type User struct {
	// Session identifies the connection; it changes on every reconnect.
	Session int32
	// UserID is the registered user ID, or -1 for an unregistered user.
	UserID          int32
	Mute            bool // muted by an admin
	Deaf            bool // deafened by an admin
	Suppress        bool // muted by the server, e.g. for lacking Speak permission
	PrioritySpeaker bool
	SelfMute        bool
	SelfDeaf        bool
	Recording       bool
	Channel         int32
	Name            string
	OnlineSecs      int32
	BytesPerSec     int32
	// Version is the client version in the legacy encoding
	// (major<<16 | minor<<8 | patch); Version2 is the newer one
	// (major<<48 | minor<<32 | patch<<16).
	Version   int32
	Version2  int64
	Release   string
	OS        string
	OSVersion string
	// Identity is the positional-audio identity the client's plugin sends.
	Identity string
	// Context is the positional-audio context, Base64-encoded.
	Context string
	Comment string
	Address netip.Addr
	TCPOnly bool
	// IdleSecs is how many seconds the user has been idle.
	IdleSecs int32
	UDPPing  float32 // milliseconds
	TCPPing  float32 // milliseconds
}

// TextMessage is a message a user sent.
type TextMessage struct {
	Sessions []int32 // users it was sent to
	Channels []int32 // channels it was sent to
	Trees    []int32 // channels it was sent to, subchannels included
	Text     string
}

// Channel is a channel. The root channel has ID 0 and Parent -1.
type Channel struct {
	ID          int32
	Name        string
	Parent      int32
	Links       []int32
	Description string
	Temporary   bool
	Position    int32
}

// Group is a channel group.
type Group struct {
	Name string
	// Inherited is set when the group is defined on a parent channel.
	Inherited   bool
	Inherit     bool // members are inherited from the parent's group of the same name
	Inheritable bool // subchannels may inherit this group
	Add         []int32
	Remove      []int32
	// Members is the effective member list. It is ignored when written.
	Members []int32
}

// ACLEntry is one rule of a channel's access control list. It applies to a
// user (UserID >= 0) or to a group (UserID -1).
type ACLEntry struct {
	ApplyHere bool
	ApplySubs bool
	// Inherited is set on rules that come from a parent channel.
	Inherited bool
	UserID    int32
	Group     string
	Allow     Permission
	Deny      Permission
}

// ACL is a channel's access control list and groups.
type ACL struct {
	Entries []ACLEntry
	Groups  []Group
	Inherit bool
}

// Ban is an address ban.
type Ban struct {
	// Address is the banned address; IPv4 addresses are unmapped.
	Address netip.Addr
	// Bits is the mask length counted over the address as IPv6, so an
	// IPv4 ban of a single address has 128. Use Prefix for the usual form.
	Bits     int32
	Name     string
	Hash     string // certificate hash
	Reason   string
	Start    time.Time
	Duration time.Duration // 0 for a permanent ban
}

// Prefix returns the banned range.
func (b Ban) Prefix() netip.Prefix {
	bits := int(b.Bits)
	if b.Address.Is4() {
		bits -= 96
	}
	p, err := b.Address.Prefix(bits)
	if err != nil {
		return netip.Prefix{}
	}
	return p
}

// LogEntry is a line of a virtual server's log.
type LogEntry struct {
	Time time.Time
	Text string
}

// Tree is a channel with its subchannels and the users in it.
type Tree struct {
	Channel  Channel
	Children []*Tree
	Users    []User
}

// UserInfo selects a field of a registration.
type UserInfo int32

const (
	UserName UserInfo = iota
	UserEmail
	UserComment
	UserHash // certificate hash
	UserPassword
	UserLastActive
	UserKDFIterations
)

func (u UserInfo) String() string {
	switch u {
	case UserName:
		return "UserName"
	case UserEmail:
		return "UserEmail"
	case UserComment:
		return "UserComment"
	case UserHash:
		return "UserHash"
	case UserPassword:
		return "UserPassword"
	case UserLastActive:
		return "UserLastActive"
	case UserKDFIterations:
		return "UserKDFIterations"
	}
	return fmt.Sprintf("UserInfo(%d)", int32(u))
}

// UserInfoMap is a registration, keyed by field.
type UserInfoMap map[UserInfo]string

// DBState is whether Murmur writes to its database (Mumble 1.6).
type DBState int32

const (
	DBNormal DBState = iota
	DBReadOnly
)

// ContextFlag says where a context action is offered in the client.
type ContextFlag int32

const (
	ContextServer  ContextFlag = 0x01
	ContextChannel ContextFlag = 0x02
	ContextUser    ContextFlag = 0x04
)

// Version is the version of the Murmur daemon.
type Version struct {
	Major, Minor, Patch int
	Text                string
}

func (v Version) String() string {
	if v.Text != "" {
		return v.Text
	}
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

func (v Version) atLeast(major, minor int) bool {
	return v.Major > major || (v.Major == major && v.Minor >= minor)
}

// Permission is a set of ACL permission bits.
type Permission int32

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
	PermListen          Permission = 0x800

	// The following only have an effect on the root channel.

	PermKick             Permission = 0x10000
	PermBan              Permission = 0x20000
	PermRegister         Permission = 0x40000
	PermRegisterSelf     Permission = 0x80000
	PermResetUserContent Permission = 0x100000
)

var permissionNames = []struct {
	p    Permission
	name string
}{
	{PermWrite, "Write"}, {PermTraverse, "Traverse"}, {PermEnter, "Enter"}, {PermSpeak, "Speak"},
	{PermMuteDeafen, "MuteDeafen"}, {PermMove, "Move"}, {PermMakeChannel, "MakeChannel"},
	{PermLinkChannel, "LinkChannel"}, {PermWhisper, "Whisper"}, {PermTextMessage, "TextMessage"},
	{PermMakeTempChannel, "MakeTempChannel"}, {PermListen, "Listen"}, {PermKick, "Kick"}, {PermBan, "Ban"},
	{PermRegister, "Register"}, {PermRegisterSelf, "RegisterSelf"}, {PermResetUserContent, "ResetUserContent"},
}

// Has reports whether every bit in q is set in p.
func (p Permission) Has(q Permission) bool { return p&q == q }

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

// Marshalling. Struct members are written in the order of MumbleServer.ice,
// which is the wire format; Murmur 1.5 and 1.6 use the same layouts.

func writeAddr(e *iceproto.Encoder, a netip.Addr) {
	if !a.IsValid() {
		e.ByteSeq(make([]byte, 16))
		return
	}
	b := a.As16()
	e.ByteSeq(b[:])
}

func readAddr(d *iceproto.Decoder) netip.Addr {
	b := d.ByteSeq()
	a, ok := netip.AddrFromSlice(b)
	if !ok {
		return netip.Addr{}
	}
	return a.Unmap()
}

func writeUser(e *iceproto.Encoder, u User) {
	e.Int(u.Session)
	e.Int(u.UserID)
	e.Bool(u.Mute)
	e.Bool(u.Deaf)
	e.Bool(u.Suppress)
	e.Bool(u.PrioritySpeaker)
	e.Bool(u.SelfMute)
	e.Bool(u.SelfDeaf)
	e.Bool(u.Recording)
	e.Int(u.Channel)
	e.String(u.Name)
	e.Int(u.OnlineSecs)
	e.Int(u.BytesPerSec)
	e.Int(u.Version)
	e.Long(u.Version2)
	e.String(u.Release)
	e.String(u.OS)
	e.String(u.OSVersion)
	e.String(u.Identity)
	e.String(u.Context)
	e.String(u.Comment)
	writeAddr(e, u.Address)
	e.Bool(u.TCPOnly)
	e.Int(u.IdleSecs)
	e.Float(u.UDPPing)
	e.Float(u.TCPPing)
}

func readUser(d *iceproto.Decoder) User {
	return User{
		Session:         d.Int(),
		UserID:          d.Int(),
		Mute:            d.Bool(),
		Deaf:            d.Bool(),
		Suppress:        d.Bool(),
		PrioritySpeaker: d.Bool(),
		SelfMute:        d.Bool(),
		SelfDeaf:        d.Bool(),
		Recording:       d.Bool(),
		Channel:         d.Int(),
		Name:            d.String(),
		OnlineSecs:      d.Int(),
		BytesPerSec:     d.Int(),
		Version:         d.Int(),
		Version2:        d.Long(),
		Release:         d.String(),
		OS:              d.String(),
		OSVersion:       d.String(),
		Identity:        d.String(),
		Context:         d.String(),
		Comment:         d.String(),
		Address:         readAddr(d),
		TCPOnly:         d.Bool(),
		IdleSecs:        d.Int(),
		UDPPing:         d.Float(),
		TCPPing:         d.Float(),
	}
}

func readUsers(d *iceproto.Decoder) []User {
	n := d.Size()
	users := make([]User, 0, min(n, d.Remaining()))
	for i := 0; i < n && d.Err() == nil; i++ {
		users = append(users, readUser(d))
	}
	return users
}

func readTextMessage(d *iceproto.Decoder) TextMessage {
	return TextMessage{Sessions: d.Ints(), Channels: d.Ints(), Trees: d.Ints(), Text: d.String()}
}

func writeChannel(e *iceproto.Encoder, c Channel) {
	e.Int(c.ID)
	e.String(c.Name)
	e.Int(c.Parent)
	e.Ints(c.Links)
	e.String(c.Description)
	e.Bool(c.Temporary)
	e.Int(c.Position)
}

func readChannel(d *iceproto.Decoder) Channel {
	return Channel{
		ID:          d.Int(),
		Name:        d.String(),
		Parent:      d.Int(),
		Links:       d.Ints(),
		Description: d.String(),
		Temporary:   d.Bool(),
		Position:    d.Int(),
	}
}

func writeGroup(e *iceproto.Encoder, g Group) {
	e.String(g.Name)
	e.Bool(g.Inherited)
	e.Bool(g.Inherit)
	e.Bool(g.Inheritable)
	e.Ints(g.Add)
	e.Ints(g.Remove)
	e.Ints(g.Members)
}

func readGroup(d *iceproto.Decoder) Group {
	return Group{
		Name:        d.String(),
		Inherited:   d.Bool(),
		Inherit:     d.Bool(),
		Inheritable: d.Bool(),
		Add:         d.Ints(),
		Remove:      d.Ints(),
		Members:     d.Ints(),
	}
}

func writeACLEntry(e *iceproto.Encoder, a ACLEntry) {
	e.Bool(a.ApplyHere)
	e.Bool(a.ApplySubs)
	e.Bool(a.Inherited)
	e.Int(a.UserID)
	e.String(a.Group)
	e.Int(int32(a.Allow))
	e.Int(int32(a.Deny))
}

func readACLEntry(d *iceproto.Decoder) ACLEntry {
	return ACLEntry{
		ApplyHere: d.Bool(),
		ApplySubs: d.Bool(),
		Inherited: d.Bool(),
		UserID:    d.Int(),
		Group:     d.String(),
		Allow:     Permission(d.Int()),
		Deny:      Permission(d.Int()),
	}
}

func writeBan(e *iceproto.Encoder, b Ban) {
	writeAddr(e, b.Address)
	e.Int(b.Bits)
	e.String(b.Name)
	e.String(b.Hash)
	e.String(b.Reason)
	if b.Start.IsZero() {
		e.Int(0)
	} else {
		e.Int(int32(b.Start.Unix()))
	}
	e.Int(int32(b.Duration / time.Second))
}

func readBan(d *iceproto.Decoder) Ban {
	b := Ban{Address: readAddr(d), Bits: d.Int(), Name: d.String(), Hash: d.String(), Reason: d.String()}
	if start := d.Int(); start != 0 {
		b.Start = time.Unix(int64(start), 0)
	}
	b.Duration = time.Duration(d.Int()) * time.Second
	return b
}

const treeTypeID = "::MumbleServer::Tree"

func readTree(d *iceproto.Decoder) *Tree {
	var t *Tree
	d.Instance(func(string) {
		t = &Tree{Channel: readChannel(d)}
		n := d.Size()
		for i := 0; i < n && d.Err() == nil; i++ {
			if child := readTree(d); child != nil {
				t.Children = append(t.Children, child)
			}
		}
		t.Users = readUsers(d)
	})
	return t
}

func writeUserInfoMap(e *iceproto.Encoder, m UserInfoMap) {
	e.Size(len(m))
	for k, v := range m {
		e.Enum(int32(k))
		e.String(v)
	}
}

func readUserInfoMap(d *iceproto.Decoder) UserInfoMap {
	n := d.Size()
	m := make(UserInfoMap, n)
	for i := 0; i < n && d.Err() == nil; i++ {
		k := UserInfo(d.Enum())
		m[k] = d.String()
	}
	return m
}

func readNameMap(d *iceproto.Decoder) map[int32]string {
	n := d.Size()
	m := make(map[int32]string, min(n, d.Remaining()))
	for i := 0; i < n && d.Err() == nil; i++ {
		k := d.Int()
		m[k] = d.String()
	}
	return m
}

func writeNameMap(e *iceproto.Encoder, m map[int32]string) {
	e.Size(len(m))
	for k, v := range m {
		e.Int(k)
		e.String(v)
	}
}
