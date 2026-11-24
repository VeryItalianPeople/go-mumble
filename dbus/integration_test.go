//go:build integration

package dbus_test

// Integration tests against a real Murmur. scripts/integration-test.sh
// starts one in a container and sets the two variables below; see the
// README. Each test leaves the server as it found it.

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	godbus "github.com/godbus/dbus/v5"

	"github.com/VeryItalianPeople/go-mumble/dbus"
	"github.com/VeryItalianPeople/go-mumble/internal/mumbletest"
)

var (
	client     *dbus.Client
	srv        *dbus.Server
	murmurAddr string
	// admin is a Mumble connection logged in as SuperUser. Tests delete
	// channels through it, since Murmur's D-Bus removeChannel deadlocks.
	admin *mumbletest.Conn
)

const adminPassword = "go-mumble-admin"

// adminLogin (re)connects admin as SuperUser. A new SuperUser login kicks
// the previous one.
func adminLogin(password string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	m, err := mumbletest.Dial(ctx, murmurAddr, "SuperUser", password)
	if err != nil {
		return err
	}
	if admin != nil {
		admin.Close()
	}
	admin = m
	return nil
}

func TestMain(m *testing.M) {
	busAddr := os.Getenv("MURMUR_DBUS_ADDRESS")
	murmurAddr = os.Getenv("MURMUR_ADDRESS")
	if busAddr == "" || murmurAddr == "" {
		fmt.Println("MURMUR_DBUS_ADDRESS and MURMUR_ADDRESS not set; run scripts/integration-test.sh")
		os.Exit(0)
	}

	// The container may still be starting: wait for server 1 to be up.
	deadline := time.Now().Add(60 * time.Second)
	for {
		c, err := dbus.Connect(busAddr, dbus.WithConnOptions(godbus.WithAuth(godbus.AuthAnonymous())))
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			booted, err2 := c.IsBooted(ctx, 1)
			cancel()
			if err2 == nil && booted {
				client = c
				break
			}
			err = err2
			c.Close()
		}
		if time.Now().After(deadline) {
			fmt.Println("Murmur did not come up:", err)
			os.Exit(1)
		}
		time.Sleep(500 * time.Millisecond)
	}
	srv = client.Server(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := client.SetSuperUserPassword(ctx, 1, adminPassword)
	cancel()
	if err == nil {
		err = adminLogin(adminPassword)
	}
	if err != nil {
		fmt.Println("logging in as SuperUser:", err)
		os.Exit(1)
	}
	code := m.Run()
	admin.Close()
	client.Close()
	os.Exit(code)
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("got error %v, want %v", err, target)
	}
	var e *dbus.Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v is not a *dbus.Error", err)
	}
}

// expectEvent reads events until one matches, failing after 10 seconds.
func expectEvent(t *testing.T, ch <-chan dbus.Event, desc string, match func(dbus.Event) bool) dbus.Event {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("event channel closed while waiting for %s", desc)
			}
			if match(ev) {
				return ev
			}
		case <-timeout:
			t.Fatalf("no %s event", desc)
		}
	}
}

func addChannel(t *testing.T, name string, parent int32) int32 {
	t.Helper()
	id, err := srv.AddChannel(testCtx(t), name, parent)
	must(t, err)
	t.Cleanup(func() { removeChannel(t, id) })
	return id
}

// removeChannel deletes a channel through the SuperUser connection and waits
// for it to be gone. A channel that is already gone is fine.
func removeChannel(t *testing.T, id int32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := srv.ChannelState(ctx, id); errors.Is(err, dbus.ErrInvalidChannel) {
		return
	}
	if err := admin.RemoveChannel(id); err != nil {
		t.Errorf("removing channel %d: %v", id, err)
		return
	}
	for {
		_, err := srv.ChannelState(ctx, id)
		if errors.Is(err, dbus.ErrInvalidChannel) {
			return
		}
		if ctx.Err() != nil {
			t.Errorf("channel %d still there: %v", id, err)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func registerPlayer(t *testing.T, name, password string) int32 {
	t.Helper()
	ctx := testCtx(t)
	id, err := srv.RegisterPlayer(ctx, name)
	must(t, err)
	t.Cleanup(func() {
		err := srv.UnregisterPlayer(context.Background(), id)
		if err != nil && !errors.Is(err, dbus.ErrInvalidPlayerID) {
			t.Errorf("unregistering %d: %v", id, err)
		}
	})
	if password != "" {
		must(t, srv.SetRegistration(ctx, id, name, "", password))
	}
	return id
}

func connect(t *testing.T, user, password string) *mumbletest.Conn {
	t.Helper()
	m, err := mumbletest.Dial(testCtx(t), murmurAddr, user, password)
	if err != nil {
		t.Fatalf("connecting as %s: %v", user, err)
	}
	t.Cleanup(m.Close)
	return m
}

func findPlayer(t *testing.T, session uint32) dbus.Player {
	t.Helper()
	players, err := srv.Players(testCtx(t))
	must(t, err)
	for _, p := range players {
		if p.Session == session {
			return p
		}
	}
	t.Fatalf("session %d not in Players: %+v", session, players)
	return dbus.Player{}
}

// waitPlayerState polls until the session's state satisfies cond.
func waitPlayerState(t *testing.T, session uint32, cond func(dbus.PlayerState) bool) dbus.PlayerState {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, err := srv.PlayerState(testCtx(t), session)
		must(t, err)
		if cond(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("player state never matched, last %+v", st)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestServerLists(t *testing.T) {
	ctx := testCtx(t)
	all, err := client.AllServers(ctx)
	must(t, err)
	if !slices.Contains(all, 1) {
		t.Errorf("AllServers = %v, want it to contain 1", all)
	}
	booted, err := client.BootedServers(ctx)
	must(t, err)
	if !slices.Contains(booted, 1) {
		t.Errorf("BootedServers = %v, want it to contain 1", booted)
	}
	if ok, err := client.IsBooted(ctx, 1); err != nil || !ok {
		t.Errorf("IsBooted(1) = %v, %v", ok, err)
	}
	if ok, err := client.IsBooted(ctx, 4242); err != nil || ok {
		t.Errorf("IsBooted(4242) = %v, %v", ok, err)
	}
}

func TestServerLifecycle(t *testing.T) {
	ctx := testCtx(t)
	events, err := client.Events(ctx)
	must(t, err)

	id, err := client.NewServer(ctx)
	must(t, err)
	t.Cleanup(func() {
		_ = client.Stop(context.Background(), id)
		_ = client.DeleteServer(context.Background(), id)
	})
	if ok, _ := client.IsBooted(ctx, id); ok {
		t.Fatal("new server is booted")
	}

	must(t, client.Start(ctx, id))
	expectEvent(t, events, "ServerStarted", func(ev dbus.Event) bool {
		return ev.Kind == dbus.ServerStarted && ev.ServerID == id
	})
	wantErr(t, client.Start(ctx, id), dbus.ErrAlreadyBooted)
	wantErr(t, client.DeleteServer(ctx, id), dbus.ErrServerRunning)

	chans, err := client.Server(id).Channels(ctx)
	must(t, err)
	if len(chans) == 0 || chans[0].ID != 0 {
		t.Errorf("new server's channels = %+v", chans)
	}

	must(t, client.Stop(ctx, id))
	expectEvent(t, events, "ServerStopped", func(ev dbus.Event) bool {
		return ev.Kind == dbus.ServerStopped && ev.ServerID == id
	})
	wantErr(t, client.Stop(ctx, id), dbus.ErrNotBooted)
	_, err = client.Server(id).Channels(ctx)
	wantErr(t, err, dbus.ErrNoServer)

	must(t, client.DeleteServer(ctx, id))
	wantErr(t, client.DeleteServer(ctx, id), dbus.ErrInvalidServer)
	wantErr(t, client.Start(ctx, id), dbus.ErrInvalidServer)
}

func TestConf(t *testing.T) {
	ctx := testCtx(t)
	t.Cleanup(func() { _ = client.SetConf(context.Background(), 1, "welcometext", "") })

	must(t, client.SetConf(ctx, 1, "welcometext", "set by the test"))
	v, err := client.Conf(ctx, 1, "welcometext")
	must(t, err)
	if v != "set by the test" {
		t.Errorf("Conf = %q", v)
	}
	all, err := client.AllConf(ctx, 1)
	must(t, err)
	if all["welcometext"] != "set by the test" {
		t.Errorf("AllConf[welcometext] = %q in %v", all["welcometext"], all)
	}
	def, err := client.DefaultConf(ctx)
	must(t, err)
	if def["welcometext"] != "go-mumble integration test" {
		t.Errorf("DefaultConf[welcometext] = %q in %v", def["welcometext"], def)
	}
	if _, ok := def["key"]; ok {
		t.Error("DefaultConf includes the write-only key")
	}

	_, err = client.Conf(ctx, 1, "key")
	wantErr(t, err, dbus.ErrWriteOnly)
	_, err = client.Conf(ctx, 4242, "welcometext")
	wantErr(t, err, dbus.ErrInvalidServer)
	_, err = client.AllConf(ctx, 4242)
	wantErr(t, err, dbus.ErrInvalidServer)
	wantErr(t, client.SetConf(ctx, 4242, "welcometext", "x"), dbus.ErrInvalidServer)
}

func TestSuperUserPassword(t *testing.T) {
	ctx := testCtx(t)
	t.Cleanup(func() {
		_ = client.SetSuperUserPassword(context.Background(), 1, adminPassword)
		if err := adminLogin(adminPassword); err != nil {
			t.Errorf("logging admin back in: %v", err)
		}
	})
	must(t, client.SetSuperUserPassword(ctx, 1, "supersecret"))
	if ok, err := srv.VerifyPassword(ctx, 0, "supersecret"); err != nil || !ok {
		t.Errorf("VerifyPassword(right) = %v, %v", ok, err)
	}
	if ok, err := srv.VerifyPassword(ctx, 0, "wrong"); err != nil || ok {
		t.Errorf("VerifyPassword(wrong) = %v, %v", ok, err)
	}
	must(t, adminLogin("supersecret"))
	wantErr(t, client.SetSuperUserPassword(ctx, 4242, "x"), dbus.ErrInvalidServer)
}

func TestLog(t *testing.T) {
	ctx := testCtx(t)
	entries, err := client.Log(ctx, 1, 0, 5)
	must(t, err)
	if len(entries) == 0 || len(entries) > 5 {
		t.Fatalf("Log returned %d entries", len(entries))
	}
	for i, e := range entries {
		if e.Time.IsZero() || e.Text == "" {
			t.Errorf("entry %d = %+v", i, e)
		}
		if i > 0 && e.Time.After(entries[i-1].Time) {
			t.Errorf("entries not newest first: %v after %v", e.Time, entries[i-1].Time)
		}
	}
	skipped, err := client.Log(ctx, 1, 1, 1)
	must(t, err)
	if len(entries) > 1 && (len(skipped) != 1 || skipped[0] != entries[1]) {
		t.Errorf("Log(offset 1, count 1) = %+v, want %+v", skipped, entries[1])
	}
	_, err = client.Log(ctx, 4242, 0, 5)
	wantErr(t, err, dbus.ErrInvalidServer)
}

func TestVersion(t *testing.T) {
	v, err := client.Version(testCtx(t))
	if errors.Is(err, dbus.ErrUnsupported) {
		t.Log("getVersion not exported, as on Murmur 1.4+")
		return
	}
	must(t, err)
	if v.Major != 1 || v.Release == "" {
		t.Errorf("Version = %+v", v)
	}
	t.Logf("Murmur %s", v)
}

func TestNoServer(t *testing.T) {
	_, err := client.Server(4242).Players(testCtx(t))
	wantErr(t, err, dbus.ErrNoServer)
}

func TestChannels(t *testing.T) {
	ctx := testCtx(t)
	events, err := srv.Events(ctx)
	must(t, err)

	chans, err := srv.Channels(ctx)
	must(t, err)
	if chans[0].ID != 0 || chans[0].Parent != -1 {
		t.Fatalf("first channel = %+v, want the root", chans[0])
	}

	a := addChannel(t, "gm A", 0)
	b := addChannel(t, "gm B", a)
	st, err := srv.ChannelState(ctx, b)
	must(t, err)
	if st.Parent != a || st.Name != "gm B" || len(st.Links) != 0 {
		t.Errorf("ChannelState(b) = %+v", st)
	}

	// A channel a client creates produces ChannelCreated; the two made over
	// D-Bus above don't, so it is the first one to arrive.
	must(t, admin.CreateChannel(a, "gm C"))
	ev := expectEvent(t, events, "ChannelCreated", func(ev dbus.Event) bool { return ev.Kind == dbus.ChannelCreated })
	c := ev.Channel.ID
	t.Cleanup(func() { removeChannel(t, c) })
	if ev.ServerID != 1 || ev.Channel.Name != "gm C" || ev.Channel.Parent != a {
		t.Errorf("ChannelCreated = %+v", ev)
	}

	must(t, srv.SetChannelState(ctx, dbus.Channel{ID: b, Name: "gm B2", Parent: 0, Links: []int32{a}}))
	expectEvent(t, events, "ChannelStateChanged", func(ev dbus.Event) bool {
		return ev.Kind == dbus.ChannelStateChanged && ev.Channel.ID == b && ev.Channel.Name == "gm B2"
	})
	st, err = srv.ChannelState(ctx, b)
	must(t, err)
	if st.Name != "gm B2" || st.Parent != 0 || !slices.Equal(st.Links, []int32{a}) {
		t.Errorf("after SetChannelState, ChannelState(b) = %+v", st)
	}
	stA, err := srv.ChannelState(ctx, a)
	must(t, err)
	if !slices.Contains(stA.Links, b) {
		t.Errorf("link is not on both ends: ChannelState(a) = %+v", stA)
	}

	// a can't move under its own child.
	wantErr(t, srv.SetChannelState(ctx, dbus.Channel{ID: a, Name: "gm A", Parent: c}), dbus.ErrInvalidChannel)
	wantErr(t, srv.SetChannelState(ctx, dbus.Channel{ID: 4242, Name: "x", Parent: 0}), dbus.ErrInvalidChannel)
	_, err = srv.AddChannel(ctx, "orphan", 4242)
	wantErr(t, err, dbus.ErrInvalidChannel)
	_, err = srv.ChannelState(ctx, 4242)
	wantErr(t, err, dbus.ErrInvalidChannel)

	if err := srv.RemoveChannel(ctx, a); !errors.Is(err, dbus.ErrDeadlock) {
		t.Errorf("RemoveChannel = %v, want ErrDeadlock", err)
	}

	removeChannel(t, a)
	expectEvent(t, events, "ChannelRemoved", func(ev dbus.Event) bool {
		return ev.Kind == dbus.ChannelRemoved && ev.Channel.ID == a
	})
	_, err = srv.ChannelState(ctx, c)
	wantErr(t, err, dbus.ErrInvalidChannel) // went with its parent
}

func TestACL(t *testing.T) {
	ctx := testCtx(t)
	ch := addChannel(t, "gm acl", 0)

	before, err := srv.ACL(ctx, ch)
	must(t, err)
	if !before.Inherit {
		t.Error("new channel doesn't inherit ACLs")
	}
	inherited := 0
	for _, e := range before.Entries {
		if !e.Inherited {
			t.Errorf("new channel has a rule of its own: %+v", e)
		}
		inherited++
	}

	acl := before
	acl.Entries = append(slices.Clone(acl.Entries), dbus.ACLEntry{
		ApplyHere: true, ApplySubs: true, UserID: -1, Group: "gm_test",
		Allow: dbus.PermEnter | dbus.PermSpeak | 0x40000000, // the high bit isn't a permission
		Deny:  dbus.PermMakeChannel,
	})
	acl.Groups = append(slices.Clone(acl.Groups), dbus.Group{Name: "gm_test", Inherit: true, Inheritable: true, Add: []int32{0}})
	must(t, srv.SetACL(ctx, ch, acl))

	check := func(when string) {
		t.Helper()
		got, err := srv.ACL(ctx, ch)
		must(t, err)
		var own []dbus.ACLEntry
		n := 0
		for _, e := range got.Entries {
			if e.Inherited {
				n++
			} else {
				own = append(own, e)
			}
		}
		want := dbus.ACLEntry{ApplyHere: true, ApplySubs: true, UserID: -1, Group: "gm_test",
			Allow: dbus.PermEnter | dbus.PermSpeak, Deny: dbus.PermMakeChannel}
		if len(own) != 1 || own[0] != want {
			t.Errorf("%s: own rules = %+v, want [%+v]", when, own, want)
		}
		if n != inherited {
			t.Errorf("%s: %d inherited rules, want %d", when, n, inherited)
		}
		i := slices.IndexFunc(got.Groups, func(g dbus.Group) bool { return g.Name == "gm_test" })
		if i < 0 {
			t.Fatalf("%s: group gm_test missing from %+v", when, got.Groups)
		}
		g := got.Groups[i]
		if g.Inherited || !slices.Equal(g.Add, []int32{0}) || !slices.Contains(g.Members, 0) {
			t.Errorf("%s: group = %+v", when, g)
		}
	}
	check("after SetACL")

	// Writing back what ACL returned must not turn inherited rules into
	// rules of this channel.
	got, err := srv.ACL(ctx, ch)
	must(t, err)
	must(t, srv.SetACL(ctx, ch, got))
	check("after writing it back")

	// Without inheritance the parent's rules are still listed, as
	// Inherited, though they no longer apply.
	must(t, srv.SetACL(ctx, ch, dbus.ACL{Inherit: false}))
	got, err = srv.ACL(ctx, ch)
	must(t, err)
	if got.Inherit || slices.ContainsFunc(got.Entries, func(e dbus.ACLEntry) bool { return !e.Inherited }) {
		t.Errorf("after clearing: %+v", got)
	}

	_, err = srv.ACL(ctx, 4242)
	wantErr(t, err, dbus.ErrInvalidChannel)
	wantErr(t, srv.SetACL(ctx, 4242, dbus.ACL{}), dbus.ErrInvalidChannel)
}

func TestBans(t *testing.T) {
	ctx := testCtx(t)
	bans, err := srv.Bans(ctx)
	must(t, err)
	if len(bans) != 0 {
		t.Fatalf("fresh server has bans: %+v", bans)
	}
	// setBans exists but is empty in every Murmur.
	must(t, srv.SetBans(ctx, []dbus.Ban{{Address: netip.MustParseAddr("10.1.2.3"), Bits: 128}}))
	bans, err = srv.Bans(ctx)
	must(t, err)
	if len(bans) != 0 {
		t.Errorf("SetBans changed the bans: %+v", bans)
	}
}

func TestRegistration(t *testing.T) {
	ctx := testCtx(t)
	id := registerPlayer(t, "gm-alice", "")
	_, err := srv.RegisterPlayer(ctx, "gm-alice")
	wantErr(t, err, dbus.ErrInvalidPlayerName)

	must(t, srv.SetRegistration(ctx, id, "gm-alice", "alice@example.com", "pw1"))
	r, err := srv.Registration(ctx, id)
	must(t, err)
	if r != (dbus.Registration{ID: id, Name: "gm-alice", Email: "alice@example.com"}) {
		t.Errorf("Registration = %+v", r)
	}
	if ok, err := srv.VerifyPassword(ctx, id, "pw1"); err != nil || !ok {
		t.Errorf("VerifyPassword(right) = %v, %v", ok, err)
	}
	if ok, err := srv.VerifyPassword(ctx, id, "nope"); err != nil || ok {
		t.Errorf("VerifyPassword(wrong) = %v, %v", ok, err)
	}

	// Empty fields are left alone.
	must(t, srv.UpdateRegistration(ctx, dbus.Registration{ID: id, Email: "alice@example.org"}))
	r, err = srv.Registration(ctx, id)
	must(t, err)
	if r.Name != "gm-alice" || r.Email != "alice@example.org" {
		t.Errorf("after UpdateRegistration: %+v", r)
	}
	wantErr(t, srv.UpdateRegistration(ctx, dbus.Registration{ID: id}), dbus.ErrInvalidPlayerID)
	// Murmur doesn't check the user exists.
	must(t, srv.UpdateRegistration(ctx, dbus.Registration{ID: 4242, Name: "gm-ghost"}))
	_, err = srv.Registration(ctx, 4242)
	wantErr(t, err, dbus.ErrInvalidPlayerID)

	for _, filter := range []string{"gm-alice", "GM-ALICE", "gm-a%", ""} {
		list, err := srv.RegisteredPlayers(ctx, filter)
		must(t, err)
		if !slices.Contains(list, dbus.Registration{ID: id, Name: "gm-alice"}) {
			t.Errorf("RegisteredPlayers(%q) = %+v", filter, list)
		}
	}
	list, err := srv.RegisteredPlayers(ctx, "gm-al")
	must(t, err)
	if len(list) != 0 {
		t.Errorf("RegisteredPlayers matched a prefix without a wildcard: %+v", list)
	}

	names, err := srv.PlayerNames(ctx, []int32{id, 4242})
	must(t, err)
	if !slices.Equal(names, []string{"gm-alice", ""}) {
		t.Errorf("PlayerNames = %q", names)
	}
	ids, err := srv.PlayerIDs(ctx, []string{"GM-Alice", "gm-nobody"})
	must(t, err)
	if !slices.Equal(ids, []int32{id, -2}) {
		t.Errorf("PlayerIDs = %v", ids)
	}

	tex := []byte("\x89PNG not really")
	must(t, srv.SetTexture(ctx, id, tex))
	got, err := srv.Texture(ctx, id)
	must(t, err)
	if string(got) != string(tex) {
		t.Errorf("Texture = %q", got)
	}
	_, err = srv.Texture(ctx, 4242)
	wantErr(t, err, dbus.ErrInvalidPlayerID)
	wantErr(t, srv.SetTexture(ctx, 4242, tex), dbus.ErrInvalidPlayerID)

	must(t, srv.UnregisterPlayer(ctx, id))
	wantErr(t, srv.UnregisterPlayer(ctx, id), dbus.ErrInvalidPlayerID)
	_, err = srv.Registration(ctx, id)
	wantErr(t, err, dbus.ErrInvalidPlayerID)
}

func TestPlayers(t *testing.T) {
	ctx := testCtx(t)
	events, err := srv.Events(ctx)
	must(t, err)

	m := connect(t, "gm-bob", "")
	s := m.Session
	expectEvent(t, events, "PlayerConnected", func(ev dbus.Event) bool {
		return ev.Kind == dbus.PlayerConnected && ev.Player.Session == s
	})

	p := findPlayer(t, s)
	if p.Name != "gm-bob" || p.UserID != -1 || p.Channel != 0 {
		t.Errorf("player = %+v", p)
	}
	st, err := srv.PlayerState(ctx, s)
	must(t, err)
	if st != p.PlayerState {
		t.Errorf("PlayerState = %+v, Players had %+v", st, p.PlayerState)
	}

	ch := addChannel(t, "gm players", 0)
	must(t, srv.SetPlayerState(ctx, dbus.PlayerState{Session: s, Mute: true, SelfMute: true, Channel: ch}))
	expectEvent(t, events, "PlayerStateChanged", func(ev dbus.Event) bool {
		return ev.Kind == dbus.PlayerStateChanged && ev.Player.Session == s && ev.Player.Mute
	})
	st = waitPlayerState(t, s, func(st dbus.PlayerState) bool { return st.Channel == ch })
	if !st.Mute || st.SelfMute {
		t.Errorf("after SetPlayerState: %+v (self-mute must be ignored)", st)
	}
	wantErr(t, srv.SetPlayerState(ctx, dbus.PlayerState{Session: s, Channel: 4242}), dbus.ErrInvalidChannel)

	text := func(want string) {
		t.Helper()
		if _, err := m.WaitFor(ctx, func(f mumbletest.Frame) bool {
			return f.Type == mumbletest.MsgTextMessage && f.Body.Str(5) == want
		}); err != nil {
			t.Errorf("no text message %q: %v", want, err)
		}
	}
	must(t, srv.SendMessage(ctx, s, "hello bob"))
	text("hello bob")
	must(t, srv.SendMessageChannel(ctx, ch, false, "hello channel"))
	text("hello channel")
	must(t, srv.SendMessageChannel(ctx, 0, true, "hello tree"))
	text("hello tree")

	must(t, srv.KickPlayer(ctx, s, "bye bob"))
	f, err := m.WaitFor(ctx, func(f mumbletest.Frame) bool {
		return f.Type == mumbletest.MsgUserRemove && f.Body.Uint(1) == uint64(s)
	})
	if err != nil {
		t.Errorf("no UserRemove: %v", err)
	} else if f.Body.Str(3) != "bye bob" {
		t.Errorf("kick reason = %q", f.Body.Str(3))
	}
	expectEvent(t, events, "PlayerDisconnected", func(ev dbus.Event) bool {
		return ev.Kind == dbus.PlayerDisconnected && ev.Player.Session == s
	})

	_, err = srv.PlayerState(ctx, s)
	wantErr(t, err, dbus.ErrInvalidSession)
	wantErr(t, srv.KickPlayer(ctx, s, "again"), dbus.ErrInvalidSession)
	wantErr(t, srv.SendMessage(ctx, s, "gone"), dbus.ErrInvalidSession)
	wantErr(t, srv.SetPlayerState(ctx, dbus.PlayerState{Session: s}), dbus.ErrInvalidSession)
	wantErr(t, srv.SendMessageChannel(ctx, 4242, false, "x"), dbus.ErrInvalidChannel)
}

func TestTemporaryGroups(t *testing.T) {
	ctx := testCtx(t)
	id := registerPlayer(t, "gm-carol", "carolpw")
	ch := addChannel(t, "gm temp", 0)
	acl := dbus.ACL{
		Entries: []dbus.ACLEntry{
			{ApplyHere: true, UserID: -1, Group: "all", Deny: dbus.PermEnter},
			{ApplyHere: true, UserID: -1, Group: "gm_tmp", Allow: dbus.PermEnter},
		},
		// Listed so that SetACL below keeps its temporary members.
		Groups: []dbus.Group{{Name: "gm_tmp", Inherit: true, Inheritable: true}},
	}
	must(t, srv.SetACL(ctx, ch, acl))

	m := connect(t, "gm-carol", "carolpw")
	if p := findPlayer(t, m.Session); p.UserID != id {
		t.Fatalf("carol logged in as %+v, want user %d", p, id)
	}
	denied := func() bool {
		t.Helper()
		must(t, m.JoinChannel(ch))
		jctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		_, err := m.WaitFor(jctx, func(f mumbletest.Frame) bool {
			return f.Type == mumbletest.MsgPermissionDenied ||
				(f.Type == mumbletest.MsgUserState && f.Body.Uint(1) == uint64(m.Session) && f.Body.Has(5))
		})
		must(t, err)
		st, err := srv.PlayerState(ctx, m.Session)
		must(t, err)
		return st.Channel != ch
	}
	if !denied() {
		t.Fatal("carol entered without the group")
	}

	must(t, srv.SetTemporaryGroups(ctx, ch, id, []string{"gm_tmp"}))
	if denied() {
		// Murmur cleared the wrong user's permission cache. Any SetACL
		// clears everyone's.
		t.Log("stale permission cache after SetTemporaryGroups, as documented")
		must(t, srv.SetACL(ctx, ch, acl))
		if denied() {
			t.Fatal("carol can't enter with the temporary group")
		}
	}

	wantErr(t, srv.SetTemporaryGroups(ctx, 4242, id, nil), dbus.ErrInvalidChannel)
}

type testAuth struct {
	dbus.UnimplementedAuthenticator
	srv *dbus.Server
}

const daveID = 5000

func (a *testAuth) Authenticate(ctx context.Context, name, password string) (dbus.AuthResult, error) {
	switch name {
	case "gm-dave":
		if password != "davepw" {
			return dbus.AuthResult{UserID: dbus.AuthFailed}, nil
		}
		// Calling back into Murmur only works with reentrant set.
		if _, err := a.srv.Channels(ctx); err != nil {
			return dbus.AuthResult{}, err
		}
		return dbus.AuthResult{UserID: daveID, Name: "gm-Dave", Groups: []string{"gm_auth"}}, nil
	case "gm-eve":
		return dbus.AuthResult{UserID: dbus.AuthFailed}, nil
	case "gm-temp":
		return dbus.AuthResult{UserID: dbus.AuthTemporaryFailure}, nil
	case "gm-err":
		return dbus.AuthResult{}, errors.New("backend down")
	}
	return dbus.AuthResult{UserID: dbus.Fallthrough}, nil
}

func (a *testAuth) UserName(_ context.Context, id int32) (string, error) {
	if id == daveID {
		return "gm-Dave", nil
	}
	return "", nil
}

func (a *testAuth) UserID(_ context.Context, name string) (int32, error) {
	if strings.EqualFold(name, "gm-Dave") {
		return daveID, nil
	}
	return dbus.Fallthrough, nil
}

func TestAuthenticator(t *testing.T) {
	ctx := testCtx(t)
	reg, err := srv.SetAuthenticator(ctx, &testAuth{srv: srv}, true)
	must(t, err)
	t.Cleanup(func() {
		reg.Close()
		// Murmur keeps a database row for every user an authenticator
		// accepted.
		_ = srv.UnregisterPlayer(context.Background(), daveID)
	})

	dave := connect(t, "gm-dave", "davepw")
	if p := findPlayer(t, dave.Session); p.Name != "gm-Dave" || p.UserID != daveID {
		t.Errorf("dave = %+v", p)
	}
	dave.Close()

	reject := func(user, password string, want uint64) {
		t.Helper()
		m, err := mumbletest.Dial(ctx, murmurAddr, user, password)
		if err == nil {
			m.Close()
			t.Errorf("%s got in", user)
			return
		}
		var rej *mumbletest.RejectError
		if !errors.As(err, &rej) || rej.Type != want {
			t.Errorf("%s: %v, want reject type %d", user, err, want)
		}
	}
	reject("gm-dave", "wrong", mumbletest.RejectWrongUserPW)
	reject("gm-eve", "x", mumbletest.RejectWrongUserPW)
	reject("gm-temp", "x", mumbletest.RejectAuthenticatorFail)
	reject("gm-err", "x", mumbletest.RejectAuthenticatorFail)
	connect(t, "gm-frank", "").Close() // falls through to an unregistered login

	names, err := srv.PlayerNames(ctx, []int32{daveID})
	must(t, err)
	if !slices.Equal(names, []string{"gm-Dave"}) {
		t.Errorf("PlayerNames = %q", names)
	}
	ids, err := srv.PlayerIDs(ctx, []string{"gm-Dave"})
	must(t, err)
	if !slices.Equal(ids, []int32{daveID}) {
		t.Errorf("PlayerIDs = %v", ids)
	}

	// After Close, the next login's call fails, Murmur drops the
	// authenticator and lets eve in through its own database.
	must(t, reg.Close())
	connect(t, "gm-eve", "x").Close()
	connect(t, "gm-eve", "x").Close()

	log, err := client.Log(ctx, 1, 0, 50)
	must(t, err)
	if !slices.ContainsFunc(log, func(e dbus.LogEntry) bool {
		return strings.Contains(e.Text, "DBus Authenticator failed")
	}) {
		t.Error("Murmur's log doesn't show the authenticator being dropped")
	}
}

func TestCloseEndsSubscriptions(t *testing.T) {
	ctx := testCtx(t)
	addr := os.Getenv("MURMUR_DBUS_ADDRESS")
	for _, wrapped := range []bool{false, true} {
		var c *dbus.Client
		if wrapped {
			// New leaves the connection open on Close; the
			// subscription must end all the same.
			conn, err := godbus.Connect(addr, godbus.WithAuth(godbus.AuthAnonymous()))
			must(t, err)
			defer conn.Close()
			c = dbus.New(conn)
		} else {
			var err error
			c, err = dbus.Connect(addr, dbus.WithConnOptions(godbus.WithAuth(godbus.AuthAnonymous())))
			must(t, err)
		}
		evs, err := c.Server(1).Events(ctx)
		must(t, err)
		must(t, c.Close())
		select {
		case _, ok := <-evs:
			if ok {
				t.Error("got an event after Close")
			}
		case <-time.After(5 * time.Second):
			t.Error("channel not closed by Close")
		}
	}
}

func TestContextEndsSubscription(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	evs, err := srv.Events(ctx)
	must(t, err)
	cancel()
	select {
	case _, ok := <-evs:
		if ok {
			t.Error("got an event after cancel")
		}
	case <-time.After(5 * time.Second):
		t.Error("channel not closed by cancel")
	}
}

type wknAuth struct {
	dbus.UnimplementedAuthenticator
}

func (wknAuth) Authenticate(_ context.Context, name, _ string) (dbus.AuthResult, error) {
	if name == "gm-wkn" {
		return dbus.AuthResult{UserID: 5001}, nil
	}
	return dbus.AuthResult{UserID: dbus.Fallthrough}, nil
}

// On a system bus, Murmur can only call an authenticator that owns a name the
// bus policy lets it send to. Murmur looks up the caller's names when the
// authenticator is set and calls back on that name.
func TestAuthenticatorWellKnownName(t *testing.T) {
	ctx := testCtx(t)
	c, err := dbus.Connect(os.Getenv("MURMUR_DBUS_ADDRESS"), dbus.WithConnOptions(godbus.WithAuth(godbus.AuthAnonymous())))
	must(t, err)
	defer c.Close()
	const name = "com.example.GoMumbleTest"
	reply, err := c.Conn().RequestName(name, godbus.NameFlagDoNotQueue)
	must(t, err)
	if reply != godbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("RequestName = %v", reply)
	}

	reg, err := c.Server(1).SetAuthenticator(ctx, wknAuth{}, false)
	must(t, err)
	t.Cleanup(func() {
		reg.Close()
		_ = srv.UnregisterPlayer(context.Background(), 5001)
	})

	m := connect(t, "gm-wkn", "")
	if p := findPlayer(t, m.Session); p.UserID != 5001 {
		t.Errorf("gm-wkn = %+v", p)
	}
	log, err := client.Log(ctx, 1, 0, 50)
	must(t, err)
	if !slices.ContainsFunc(log, func(e dbus.LogEntry) bool {
		return strings.Contains(e.Text, "DBus Authenticator set to "+name+" ")
	}) {
		t.Errorf("Murmur didn't record the well-known name; log: %+v", log[:min(len(log), 5)])
	}
}
