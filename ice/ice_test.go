package ice

import (
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/VeryItalianPeople/go-mumble/internal/iceproto"
)

func TestWrapError(t *testing.T) {
	cases := []struct {
		err  error
		want error
	}{
		{&iceproto.UserException{TypeID: "::MumbleServer::InvalidSessionException"}, ErrInvalidSession},
		{&iceproto.UserException{TypeID: "::MumbleServer::ReadOnlyModeException"}, ErrReadOnlyMode},
		{&iceproto.DispatchError{Status: iceproto.ObjectNotExist, Identity: iceproto.Identity{Category: "s", Name: "9"}}, ErrServerNotFound},
		{&iceproto.DispatchError{Status: iceproto.OperationNotExist}, ErrUnsupported},
	}
	for _, c := range cases {
		err := wrapError("op", c.err)
		if !errors.Is(err, c.want) {
			t.Errorf("%v: got %v, want %v", c.err, err, c.want)
		}
		var e *Error
		if !errors.As(err, &e) || e.Operation != "op" || !errors.Is(err, c.err) {
			t.Errorf("%v: not an *Error wrapping the Ice error", c.err)
		}
	}
	other := errors.New("broken pipe")
	if wrapError("op", other) != other {
		t.Error("non-Ice errors must pass through")
	}
	unknown := wrapError("op", &iceproto.UserException{TypeID: "::Other::Thing"})
	for _, s := range []error{ErrInvalidSession, ErrServerBooted, ErrUnsupported} {
		if errors.Is(unknown, s) {
			t.Errorf("unknown exception matched %v", s)
		}
	}
}

func TestBanPrefix(t *testing.T) {
	b := Ban{Address: netip.MustParseAddr("10.1.2.3"), Bits: 120}
	if p := b.Prefix(); p != netip.MustParsePrefix("10.1.2.0/24") {
		t.Errorf("IPv4 prefix = %v", p)
	}
	b = Ban{Address: netip.MustParseAddr("2001:db8::1"), Bits: 32}
	if p := b.Prefix(); p != netip.MustParsePrefix("2001:db8::/32") {
		t.Errorf("IPv6 prefix = %v", p)
	}
}

func TestBanRoundTrip(t *testing.T) {
	want := Ban{Address: netip.MustParseAddr("192.0.2.1"), Bits: 128, Name: "n", Hash: "h", Reason: "r",
		Start: time.Unix(1700000000, 0), Duration: time.Hour}
	var e iceproto.Encoder
	writeBan(&e, want)
	got := readBan(iceproto.NewDecoder(e.Bytes()))
	if got.Address != want.Address || got.Bits != want.Bits || got.Name != want.Name || got.Hash != want.Hash ||
		got.Reason != want.Reason || !got.Start.Equal(want.Start) || got.Duration != want.Duration {
		t.Errorf("round trip = %+v", got)
	}
}

func TestUserRoundTrip(t *testing.T) {
	want := User{Session: 3, UserID: -1, Mute: true, Channel: 2, Name: "bob", Version2: 1 << 48,
		Address: netip.MustParseAddr("127.0.0.1"), UDPPing: 1.5, Comment: "c"}
	var e iceproto.Encoder
	writeUser(&e, want)
	d := iceproto.NewDecoder(e.Bytes())
	if got := readUser(d); got != want || d.Remaining() != 0 {
		t.Errorf("round trip = %+v (%d left)", got, d.Remaining())
	}
}

func TestPermissionString(t *testing.T) {
	if s := (PermEnter | PermKick | 0x40000000).String(); s != "Enter|Kick|0x40000000" {
		t.Errorf("String = %q", s)
	}
}

func TestVersionAtLeast(t *testing.T) {
	v := Version{Major: 1, Minor: 6}
	if !v.atLeast(1, 6) || !v.atLeast(1, 5) || v.atLeast(1, 7) || v.atLeast(2, 0) {
		t.Error("atLeast is wrong")
	}
}
