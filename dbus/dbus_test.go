package dbus

import (
	"errors"
	"net/netip"
	"testing"

	godbus "github.com/godbus/dbus/v5"
)

func TestPermissionString(t *testing.T) {
	cases := []struct {
		p    Permission
		want string
	}{
		{0, "None"},
		{PermEnter, "Enter"},
		{PermEnter | PermSpeak | PermKick, "Enter|Speak|Kick"},
		{PermWrite | 0x40000000, "Write|0x40000000"},
	}
	for _, c := range cases {
		if got := c.p.String(); got != c.want {
			t.Errorf("%#x.String() = %q, want %q", uint32(c.p), got, c.want)
		}
	}
	if !(PermEnter | PermSpeak).Has(PermSpeak) || PermEnter.Has(PermEnter|PermSpeak) {
		t.Error("Has is wrong")
	}
}

func TestBan(t *testing.T) {
	w := wireBan{Address: 0x0a010203, Bits: 120}
	b := w.ban()
	if b.Address != netip.MustParseAddr("10.1.2.3") {
		t.Errorf("Address = %v", b.Address)
	}
	if got := b.Prefix(); got != netip.MustParsePrefix("10.1.2.0/24") {
		t.Errorf("Prefix = %v", got)
	}
	if b.wire() != w {
		t.Errorf("round trip = %+v, want %+v", b.wire(), w)
	}
	mapped := Ban{Address: netip.MustParseAddr("::ffff:10.1.2.3"), Bits: 128}
	if mapped.wire().Address != 0x0a010203 {
		t.Errorf("IPv4-mapped address encodes as %#x", mapped.wire().Address)
	}
	if (Ban{Address: b.Address, Bits: 32}).Prefix().IsValid() {
		t.Error("Prefix accepted a mask below 96")
	}
}

func TestWrapError(t *testing.T) {
	cases := []struct {
		method, name string
		want         error
	}{
		{"getChannelState", "net.sourceforge.mumble.Error.channel", ErrInvalidChannel},
		{"kickPlayer", "net.sourceforge.mumble.Error.session", ErrInvalidSession},
		{"start", "net.sourceforge.mumble.Error.booted", ErrAlreadyBooted},
		{"stop", "net.sourceforge.mumble.Error.booted", ErrNotBooted},
		{"deleteServer", "net.sourceforge.mumble.Error.booted", ErrServerRunning},
		{"getVersion", "org.freedesktop.DBus.Error.UnknownMethod", ErrUnsupported},
		{"getPlayers", "org.freedesktop.DBus.Error.UnknownObject", ErrNoServer},
	}
	for _, c := range cases {
		err := wrapError(c.method, godbus.Error{Name: c.name, Body: []any{"text"}})
		if !errors.Is(err, c.want) {
			t.Errorf("%s %s: got %v, want %v", c.method, c.name, err, c.want)
		}
		var e *Error
		if !errors.As(err, &e) || e.Message != "text" || e.Method != c.method {
			t.Errorf("%s %s: *Error = %+v", c.method, c.name, e)
		}
	}

	unknown := wrapError("getPlayers", &godbus.Error{Name: "org.example.Weird"})
	var e *Error
	if !errors.As(unknown, &e) || e.Unwrap() != nil {
		t.Errorf("unknown error name: %v", unknown)
	}

	other := errors.New("broken pipe")
	if wrapError("getPlayers", other) != other {
		t.Error("non-D-Bus errors must pass through untouched")
	}
}

func TestParseSignal(t *testing.T) {
	state := []any{uint32(7), true, false, false, true, false, int32(3)}
	ev, ok := parseSignal(&godbus.Signal{
		Path: "/12",
		Name: serverInterface + ".playerStateChanged",
		Body: []any{state},
	})
	want := Event{Kind: PlayerStateChanged, ServerID: 12, Player: PlayerState{Session: 7, Mute: true, SelfMute: true, Channel: 3}}
	if !ok || ev.Kind != want.Kind || ev.ServerID != want.ServerID || ev.Player != want.Player {
		t.Errorf("player signal = %+v, %v", ev, ok)
	}

	ev, ok = parseSignal(&godbus.Signal{
		Path: "/1",
		Name: serverInterface + ".channelRemoved",
		Body: []any{[]any{int32(5), "gone", int32(0), []int32{2}}},
	})
	if !ok || ev.Kind != ChannelRemoved || ev.Channel.ID != 5 || ev.Channel.Name != "gone" || len(ev.Channel.Links) != 1 {
		t.Errorf("channel signal = %+v, %v", ev, ok)
	}

	ev, ok = parseSignal(&godbus.Signal{Path: "/", Name: metaInterface + ".started", Body: []any{int32(4)}})
	if !ok || ev.Kind != ServerStarted || ev.ServerID != 4 {
		t.Errorf("meta signal = %+v, %v", ev, ok)
	}

	for _, sig := range []*godbus.Signal{
		{Path: "/1", Name: "org.example.Other.thing"},
		{Path: "/x", Name: serverInterface + ".channelCreated", Body: []any{[]any{int32(1), "a", int32(0), []int32{}}}},
		{Path: "/1", Name: serverInterface + ".playerConnected", Body: []any{"wrong shape"}},
		{Path: "/1", Name: metaInterface + ".started", Body: []any{int32(1)}},
	} {
		if ev, ok := parseSignal(sig); ok {
			t.Errorf("parsed %+v as %+v", sig, ev)
		}
	}
}

func TestParseServerPath(t *testing.T) {
	for p, want := range map[godbus.ObjectPath]int32{"/1": 1, "/2147483647": 1<<31 - 1} {
		if got, ok := parseServerPath(p); !ok || got != want {
			t.Errorf("parseServerPath(%q) = %d, %v", p, got, ok)
		}
	}
	for _, p := range []godbus.ObjectPath{"/", "/-1", "/+1", "/1/2", "/2147483648", "1"} {
		if got, ok := parseServerPath(p); ok {
			t.Errorf("parseServerPath(%q) = %d, want rejection", p, got)
		}
	}
}

func TestToInt(t *testing.T) {
	for _, v := range []any{int32(5), uint16(5), uint32(5), int64(5)} {
		if n, ok := toInt(v); !ok || n != 5 {
			t.Errorf("toInt(%T) = %d, %v", v, n, ok)
		}
	}
	if _, ok := toInt("5"); ok {
		t.Error("toInt accepted a string")
	}
}
