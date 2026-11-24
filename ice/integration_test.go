//go:build integration

package ice_test

// Integration tests against a real Murmur over Ice. scripts/integration-test.sh
// ice [1.6|1.5] starts one and sets the variables read below. Each test
// leaves the server as it found it.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/VeryItalianPeople/go-mumble/ice"
	"github.com/VeryItalianPeople/go-mumble/internal/mumbletest"
)

var (
	iceAddr    string
	murmurAddr string
	secret     string
	readSecret string
	client     *ice.Client
	srv        *ice.Server
	version    ice.Version
	admin      *mumbletest.Conn
)

func TestMain(m *testing.M) {
	iceAddr = os.Getenv("MURMUR_ICE_ADDRESS")
	murmurAddr = os.Getenv("MURMUR_ADDRESS")
	secret = os.Getenv("MURMUR_ICE_SECRET")
	readSecret = os.Getenv("MURMUR_ICE_READ_SECRET")
	if iceAddr == "" || murmurAddr == "" {
		fmt.Println("MURMUR_ICE_ADDRESS and MURMUR_ADDRESS not set; run scripts/integration-test.sh ice")
		os.Exit(0)
	}

	// The container may still be starting.
	deadline := time.Now().Add(60 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		c, err := ice.Dial(ctx, iceAddr, ice.WithSecret(secret), ice.WithCallbackSecret(secret))
		var servers []*ice.Server
		if err == nil {
			servers, err = c.BootedServers(ctx)
			if err == nil && len(servers) == 0 {
				err = errors.New("no booted server")
			}
			if err == nil {
				version, err = c.Version(ctx)
			}
		}
		cancel()
		if err == nil {
			client, srv = c, servers[0]
			break
		}
		if c != nil {
			c.Close()
		}
		if time.Now().After(deadline) {
			fmt.Println("Murmur did not come up:", err)
			os.Exit(1)
		}
		time.Sleep(500 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	const adminPassword = "go-mumble-admin"
	err := srv.SetSuperuserPassword(ctx, adminPassword)
	if err == nil {
		admin, err = mumbletest.Dial(ctx, murmurAddr, "SuperUser", adminPassword)
	}
	cancel()
	if err != nil {
		fmt.Println("logging in as SuperUser:", err)
		os.Exit(1)
	}
	fmt.Printf("Murmur %s, server %d\n", version, srv.ID())
	code := m.Run()
	admin.Close()
	client.Close()
	os.Exit(code)
}

func is16() bool { return version.Major > 1 || version.Minor >= 6 }

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
	var e *ice.Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v is not an *ice.Error", err)
	}
}

func expectEvent(t *testing.T, ch <-chan ice.Event, desc string, match func(ice.Event) bool) ice.Event {
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
	t.Cleanup(func() {
		err := srv.RemoveChannel(context.Background(), id)
		if err != nil && !errors.Is(err, ice.ErrInvalidChannel) {
			t.Errorf("removing channel %d: %v", id, err)
		}
	})
	return id
}

func registerUser(t *testing.T, name, password string) int32 {
	t.Helper()
	info := ice.UserInfoMap{ice.UserName: name}
	if password != "" {
		info[ice.UserPassword] = password
	}
	id, err := srv.RegisterUser(testCtx(t), info)
	must(t, err)
	t.Cleanup(func() {
		err := srv.UnregisterUser(context.Background(), id)
		if err != nil && !errors.Is(err, ice.ErrInvalidUser) {
			t.Errorf("unregistering %d: %v", id, err)
		}
	})
	return id
}

func connect(t *testing.T, user, password string, opts ...mumbletest.Options) *mumbletest.Conn {
	t.Helper()
	m, err := mumbletest.Dial(testCtx(t), murmurAddr, user, password, opts...)
	if err != nil {
		t.Fatalf("connecting as %s: %v", user, err)
	}
	t.Cleanup(m.Close)
	return m
}

func session(m *mumbletest.Conn) int32 { return int32(m.Session) }

func waitState(t *testing.T, s int32, cond func(ice.User) bool) ice.User {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		u, err := srv.State(testCtx(t), s)
		must(t, err)
		if cond(u) {
			return u
		}
		if time.Now().After(deadline) {
			t.Fatalf("state never matched, last %+v", u)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func textFrame(want string) func(mumbletest.Frame) bool {
	return func(f mumbletest.Frame) bool {
		return f.Type == mumbletest.MsgTextMessage && strings.Contains(f.Body.Str(5), want)
	}
}

func selfSignedCert(t *testing.T, cn string) (tls.Certificate, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	must(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	must(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	must(t, err)
	return c, certPEM, keyPEM
}

func TestMetaInfo(t *testing.T) {
	ctx := testCtx(t)
	anon, err := ice.Dial(ctx, iceAddr)
	must(t, err)
	defer anon.Close()
	v, err := anon.Version(ctx) // needs no secret
	must(t, err)
	if v.Major != 1 || v.Text == "" {
		t.Errorf("Version = %+v", v)
	}
	slice, err := client.Slice(ctx)
	must(t, err)
	if !strings.Contains(slice, "module MumbleServer") {
		t.Errorf("Slice doesn't look like MumbleServer.ice: %.200q", slice)
	}
	sums, err := client.SliceChecksums(ctx)
	must(t, err)
	if len(sums) == 0 {
		t.Error("no Slice checksums")
	}
	up, err := client.Uptime(ctx)
	must(t, err)
	if up < 0 {
		t.Errorf("Uptime = %v", up)
	}
	def, err := client.DefaultConf(ctx)
	must(t, err)
	if def["welcometext"] == "" {
		t.Errorf("DefaultConf has no welcometext: %v", def)
	}
}

func TestSecrets(t *testing.T) {
	ctx := testCtx(t)
	anon, err := ice.Dial(ctx, iceAddr)
	must(t, err)
	defer anon.Close()
	_, err = anon.BootedServers(ctx)
	wantErr(t, err, ice.ErrInvalidSecret)

	ro, err := ice.Dial(ctx, iceAddr, ice.WithSecret(readSecret))
	must(t, err)
	defer ro.Close()
	_, err = ro.Server(srv.ID()).Users(ctx)
	must(t, err)
	wantErr(t, ro.Server(srv.ID()).SetConf(ctx, "welcometext", "x"), ice.ErrInvalidSecret)
}

func TestServerLookup(t *testing.T) {
	ctx := testCtx(t)
	all, err := client.AllServers(ctx)
	must(t, err)
	if !slices.ContainsFunc(all, func(s *ice.Server) bool { return s.ID() == srv.ID() }) {
		t.Errorf("AllServers doesn't list %d", srv.ID())
	}
	got, err := client.GetServer(ctx, srv.ID())
	must(t, err)
	if got.ID() != srv.ID() {
		t.Errorf("GetServer = %d", got.ID())
	}
	_, err = client.GetServer(ctx, 4242)
	wantErr(t, err, ice.ErrServerNotFound)
	id, err := srv.QueryID(ctx)
	must(t, err)
	if id != srv.ID() {
		t.Errorf("QueryID = %d", id)
	}
	ok, err := srv.IsRunning(ctx)
	must(t, err)
	if !ok {
		t.Error("IsRunning = false")
	}
	up, err := srv.Uptime(ctx)
	must(t, err)
	if up < 0 {
		t.Errorf("server Uptime = %v", up)
	}
	// A server that doesn't exist: 1.6 answers like it was stopped for
	// most operations, 1.5 says it doesn't exist.
	_, err = client.Server(4242).Users(ctx)
	if is16() {
		wantErr(t, err, ice.ErrServerBooted)
	} else {
		wantErr(t, err, ice.ErrServerNotFound)
	}
	_, err = client.Server(4242).Conf(ctx, "welcometext")
	wantErr(t, err, ice.ErrServerNotFound)
}

func TestLifecycle(t *testing.T) {
	ctx := testCtx(t)
	events, err := client.Events(ctx)
	must(t, err)

	s, err := client.NewServer(ctx)
	must(t, err)
	t.Cleanup(func() {
		_ = s.Stop(context.Background())
		_ = s.Delete(context.Background())
	})
	if ok, _ := s.IsRunning(ctx); ok {
		t.Fatal("new server is running")
	}
	// Configuration calls on a stopped server crash Murmur 1.6, so the
	// client refuses them there; 1.5 answers them.
	err = s.SetConf(ctx, "registername", "gm stopped")
	if is16() {
		wantErr(t, err, ice.ErrWouldCrash)
		_, err = s.Conf(ctx, "registername")
		wantErr(t, err, ice.ErrWouldCrash)
		_, err = s.Log(ctx, 0, 1)
		wantErr(t, err, ice.ErrWouldCrash)
	} else {
		must(t, err)
		v, err := s.Conf(ctx, "registername")
		must(t, err)
		if v != "gm stopped" {
			t.Errorf("Conf on a stopped server = %q", v)
		}
	}
	if _, err := client.Version(ctx); err != nil {
		t.Fatalf("Murmur is gone: %v", err)
	}

	must(t, s.Start(ctx))
	expectEvent(t, events, "ServerStarted", func(ev ice.Event) bool {
		return ev.Kind == ice.ServerStarted && ev.ServerID == s.ID()
	})
	wantErr(t, s.Start(ctx), ice.ErrServerBooted)
	wantErr(t, s.Delete(ctx), ice.ErrServerBooted)
	chans, err := s.Channels(ctx)
	must(t, err)
	if _, ok := chans[0]; !ok {
		t.Errorf("new server has no root channel: %v", chans)
	}

	must(t, s.Stop(ctx))
	expectEvent(t, events, "ServerStopped", func(ev ice.Event) bool {
		return ev.Kind == ice.ServerStopped && ev.ServerID == s.ID()
	})
	wantErr(t, s.Stop(ctx), ice.ErrServerBooted)
	_, err = s.Users(ctx)
	wantErr(t, err, ice.ErrServerBooted)

	must(t, s.Delete(ctx))
	_, err = client.GetServer(ctx, s.ID())
	wantErr(t, err, ice.ErrServerNotFound)
}

func TestConf(t *testing.T) {
	ctx := testCtx(t)
	t.Cleanup(func() { _ = srv.SetConf(context.Background(), "registername", "") })
	must(t, srv.SetConf(ctx, "registername", "set by the test"))
	v, err := srv.Conf(ctx, "registername")
	must(t, err)
	if v != "set by the test" {
		t.Errorf("Conf = %q", v)
	}
	all, err := srv.AllConf(ctx)
	must(t, err)
	if all["registername"] != "set by the test" {
		t.Errorf("AllConf[registername] = %q", all["registername"])
	}
	must(t, srv.SetConf(ctx, "registername", ""))
	if v, err := srv.Conf(ctx, "registername"); err != nil || v != "" {
		t.Errorf("after setting \"\": %q, %v", v, err)
	}
	all, err = srv.AllConf(ctx)
	must(t, err)
	// 1.5 deletes the key, 1.6 stores the empty value.
	if _, kept := all["registername"]; kept != is16() {
		t.Errorf("after setting \"\", AllConf has the key: %v", kept)
	}
	_, err = srv.Conf(ctx, "key")
	wantErr(t, err, ice.ErrWriteOnly)
}

func TestLog(t *testing.T) {
	ctx := testCtx(t)
	n, err := srv.LogLen(ctx)
	must(t, err)
	entries, err := srv.Log(ctx, 0, 5)
	must(t, err)
	if len(entries) == 0 || len(entries) > 5 || int32(len(entries)) > n {
		t.Fatalf("Log(0, 5) returned %d of %d", len(entries), n)
	}
	for i, e := range entries {
		if e.Time.IsZero() || e.Text == "" {
			t.Errorf("entry %d = %+v", i, e)
		}
		if i > 0 && e.Time.After(entries[i-1].Time) {
			t.Errorf("not newest first: %v after %v", e.Time, entries[i-1].Time)
		}
	}
	if len(entries) >= 3 {
		got, err := srv.Log(ctx, 1, 2)
		must(t, err)
		if len(got) != 2 || got[0] != entries[1] || got[1] != entries[2] {
			t.Errorf("Log(1, 2) = %+v, want %+v", got, entries[1:3])
		}
	}
}

func TestChannels(t *testing.T) {
	ctx := testCtx(t)
	events, err := srv.Events(ctx)
	must(t, err)

	a := addChannel(t, "gm A", 0)
	b := addChannel(t, "gm B", a)

	// Channels made through Ice produce no ChannelCreated: Murmur only
	// reports channels clients create. The first one to arrive is the
	// admin's.
	must(t, admin.CreateChannel(a, "gm C"))
	ev := expectEvent(t, events, "ChannelCreated", func(ev ice.Event) bool { return ev.Kind == ice.ChannelCreated })
	c := ev.Channel.ID
	t.Cleanup(func() { _ = srv.RemoveChannel(context.Background(), c) })
	if ev.ServerID != srv.ID() || ev.Channel.Name != "gm C" || ev.Channel.Parent != a {
		t.Errorf("ChannelCreated = %+v", ev)
	}

	st, err := srv.ChannelState(ctx, b)
	must(t, err)
	if st.Parent != a || st.Name != "gm B" {
		t.Errorf("ChannelState(b) = %+v", st)
	}
	st.Name, st.Parent, st.Links, st.Description, st.Position = "gm B2", 0, []int32{a}, "described", 7
	must(t, srv.SetChannelState(ctx, st))
	expectEvent(t, events, "ChannelStateChanged", func(ev ice.Event) bool {
		return ev.Kind == ice.ChannelStateChanged && ev.Channel.ID == b && ev.Channel.Name == "gm B2"
	})
	st, err = srv.ChannelState(ctx, b)
	must(t, err)
	if st.Name != "gm B2" || st.Parent != 0 || !slices.Equal(st.Links, []int32{a}) || st.Description != "described" || st.Position != 7 {
		t.Errorf("after SetChannelState: %+v", st)
	}
	all, err := srv.Channels(ctx)
	must(t, err)
	if all[b].Name != "gm B2" || all[0].Parent != -1 {
		t.Errorf("Channels = %+v", all)
	}

	tree, err := srv.Tree(ctx)
	must(t, err)
	if tree.Channel.ID != 0 {
		t.Fatalf("tree root = %+v", tree.Channel)
	}
	var find func(*ice.Tree, int32) *ice.Tree
	find = func(n *ice.Tree, id int32) *ice.Tree {
		if n.Channel.ID == id {
			return n
		}
		for _, ch := range n.Children {
			if f := find(ch, id); f != nil {
				return f
			}
		}
		return nil
	}
	if ta := find(tree, a); ta == nil || find(ta, c) == nil {
		t.Errorf("tree doesn't have %d under the root and %d under it", a, c)
	}
	if !slices.ContainsFunc(tree.Users, func(u ice.User) bool { return u.Name == "SuperUser" }) {
		t.Errorf("SuperUser isn't in the root of the tree: %+v", tree.Users)
	}

	wantErr(t, srv.SetChannelState(ctx, ice.Channel{ID: a, Name: "gm A", Parent: c}), ice.ErrInvalidChannel)
	_, err = srv.AddChannel(ctx, "orphan", 4242)
	wantErr(t, err, ice.ErrInvalidChannel)
	_, err = srv.ChannelState(ctx, 4242)
	wantErr(t, err, ice.ErrInvalidChannel)
	wantErr(t, srv.RemoveChannel(ctx, 0), ice.ErrInvalidChannel)

	// Unlike over D-Bus, removing a channel doesn't hang Murmur.
	must(t, srv.RemoveChannel(ctx, a))
	expectEvent(t, events, "ChannelRemoved", func(ev ice.Event) bool {
		return ev.Kind == ice.ChannelRemoved && ev.Channel.ID == a
	})
	_, err = srv.ChannelState(ctx, c)
	wantErr(t, err, ice.ErrInvalidChannel)
}

func TestACL(t *testing.T) {
	ctx := testCtx(t)
	ch := addChannel(t, "gm acl", 0)
	before, err := srv.ACL(ctx, ch)
	must(t, err)
	inherited := len(before.Entries)
	for _, e := range before.Entries {
		if !e.Inherited {
			t.Errorf("new channel has a rule of its own: %+v", e)
		}
	}

	acl := before
	acl.Entries = append(slices.Clone(acl.Entries), ice.ACLEntry{
		ApplyHere: true, ApplySubs: true, UserID: -1, Group: "gm_test",
		Allow: ice.PermEnter | ice.PermSpeak, Deny: ice.PermMakeChannel,
	})
	acl.Groups = append(slices.Clone(acl.Groups), ice.Group{Name: "gm_test", Inherit: true, Inheritable: true, Add: []int32{0}})
	must(t, srv.SetACL(ctx, ch, acl))

	check := func(when string) {
		t.Helper()
		got, err := srv.ACL(ctx, ch)
		must(t, err)
		var own []ice.ACLEntry
		for _, e := range got.Entries {
			if !e.Inherited {
				own = append(own, e)
			}
		}
		want := ice.ACLEntry{ApplyHere: true, ApplySubs: true, UserID: -1, Group: "gm_test",
			Allow: ice.PermEnter | ice.PermSpeak, Deny: ice.PermMakeChannel}
		if len(own) != 1 || own[0] != want || len(got.Entries) != inherited+1 {
			t.Errorf("%s: entries = %+v", when, got.Entries)
		}
		i := slices.IndexFunc(got.Groups, func(g ice.Group) bool { return g.Name == "gm_test" })
		if i < 0 || !slices.Equal(got.Groups[i].Add, []int32{0}) {
			t.Errorf("%s: groups = %+v", when, got.Groups)
		}
	}
	check("after SetACL")
	got, err := srv.ACL(ctx, ch)
	must(t, err)
	must(t, srv.SetACL(ctx, ch, got))
	check("after writing it back")

	_, err = srv.ACL(ctx, 4242)
	wantErr(t, err, ice.ErrInvalidChannel)
}

func TestBans(t *testing.T) {
	ctx := testCtx(t)
	t.Cleanup(func() { _ = srv.SetBans(context.Background(), nil) })
	start := time.Now().Truncate(time.Second)
	want := []ice.Ban{
		{Address: netip.MustParseAddr("10.1.2.0"), Bits: 120, Name: "gm-banned", Reason: "testing", Start: start, Duration: time.Hour},
		{Address: netip.MustParseAddr("2001:db8::"), Bits: 32, Reason: "v6", Start: start},
	}
	must(t, srv.SetBans(ctx, want))
	got, err := srv.Bans(ctx)
	must(t, err)
	if len(got) != 2 {
		t.Fatalf("Bans = %+v", got)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Address != w.Address || g.Bits != w.Bits || g.Name != w.Name || g.Reason != w.Reason ||
			!g.Start.Equal(w.Start) || g.Duration != w.Duration {
			t.Errorf("ban %d = %+v, want %+v", i, g, w)
		}
	}
	if p := got[0].Prefix(); p != netip.MustParsePrefix("10.1.2.0/24") {
		t.Errorf("Prefix = %v", p)
	}
	must(t, srv.SetBans(ctx, nil))
	got, err = srv.Bans(ctx)
	must(t, err)
	if len(got) != 0 {
		t.Errorf("bans left: %+v", got)
	}
}

func TestRegistration(t *testing.T) {
	ctx := testCtx(t)
	id := registerUser(t, "gm-alice", "pw1")
	_, err := srv.RegisterUser(ctx, ice.UserInfoMap{ice.UserName: "gm-alice"})
	wantErr(t, err, ice.ErrInvalidUser)

	must(t, srv.UpdateRegistration(ctx, id, ice.UserInfoMap{ice.UserEmail: "alice@example.com", ice.UserComment: "hi"}))
	info, err := srv.Registration(ctx, id)
	must(t, err)
	if info[ice.UserName] != "gm-alice" || info[ice.UserEmail] != "alice@example.com" || info[ice.UserComment] != "hi" {
		t.Errorf("Registration = %v", info)
	}
	_, err = srv.Registration(ctx, 4242)
	wantErr(t, err, ice.ErrInvalidUser)

	for name, want := range map[string]int32{"gm-alice": id} {
		got, err := srv.VerifyPassword(ctx, name, "pw1")
		must(t, err)
		if got != want {
			t.Errorf("VerifyPassword(right) = %d, want %d", got, want)
		}
	}
	if got, _ := srv.VerifyPassword(ctx, "gm-alice", "nope"); got != ice.AuthFailed {
		t.Errorf("VerifyPassword(wrong) = %d", got)
	}
	if got, _ := srv.VerifyPassword(ctx, "gm-nobody", "x"); got != ice.Fallthrough {
		t.Errorf("VerifyPassword(unknown) = %d", got)
	}

	users, err := srv.RegisteredUsers(ctx, "gm-alice")
	must(t, err)
	if users[id] != "gm-alice" {
		t.Errorf("RegisteredUsers = %v", users)
	}
	names, err := srv.UserNames(ctx, []int32{id})
	must(t, err)
	if names[id] != "gm-alice" {
		t.Errorf("UserNames = %v", names)
	}
	ids, err := srv.UserIDs(ctx, []string{"gm-alice"})
	must(t, err)
	if ids["gm-alice"] != id {
		t.Errorf("UserIDs = %v", ids)
	}

	tex := []byte("\x89PNG not really")
	if is16() {
		// 1.6 tests the user ID the wrong way round: it refuses every
		// registered user, and setTexture on an unregistered ID would
		// terminate Murmur, so the client refuses that one itself.
		wantErr(t, srv.SetTexture(ctx, id, tex), ice.ErrInvalidUser)
		_, err := srv.Texture(ctx, id)
		wantErr(t, err, ice.ErrInvalidUser)
		wantErr(t, srv.SetTexture(ctx, 4242, tex), ice.ErrWouldCrash)
		if _, err := client.Version(ctx); err != nil {
			t.Fatalf("Murmur is gone: %v", err)
		}
	} else {
		must(t, srv.SetTexture(ctx, id, tex))
		got, err := srv.Texture(ctx, id)
		must(t, err)
		if string(got) != string(tex) {
			t.Errorf("Texture = %q", got)
		}
		_, err = srv.Texture(ctx, 4242)
		wantErr(t, err, ice.ErrInvalidUser)
		wantErr(t, srv.SetTexture(ctx, 4242, tex), ice.ErrInvalidUser)
	}

	must(t, srv.UnregisterUser(ctx, id))
	wantErr(t, srv.UnregisterUser(ctx, id), ice.ErrInvalidUser)
}

func TestUsers(t *testing.T) {
	ctx := testCtx(t)
	events, err := srv.Events(ctx)
	must(t, err)
	cert, _, _ := selfSignedCert(t, "gm-bob")
	m := connect(t, "gm-bob", "", mumbletest.Options{Certificate: &cert})
	s := session(m)
	expectEvent(t, events, "UserConnected", func(ev ice.Event) bool {
		return ev.Kind == ice.UserConnected && ev.User.Session == s
	})

	users, err := srv.Users(ctx)
	must(t, err)
	u, ok := users[s]
	if !ok || u.Name != "gm-bob" || u.UserID != -1 || u.Release != "go-mumble-test" || !u.Address.IsLoopback() {
		t.Errorf("user = %+v", u)
	}
	st, err := srv.State(ctx, s)
	must(t, err)
	if st.Session != s || st.Name != "gm-bob" {
		t.Errorf("State = %+v", st)
	}

	certs, err := srv.CertificateList(ctx, s)
	must(t, err)
	if len(certs) != 1 || string(certs[0]) != string(cert.Certificate[0]) {
		t.Errorf("CertificateList = %d certificates", len(certs))
	}

	ch := addChannel(t, "gm users", 0)
	st.Mute, st.Channel, st.Comment, st.SelfMute = true, ch, "set by the test", true
	must(t, srv.SetState(ctx, st))
	expectEvent(t, events, "UserStateChanged", func(ev ice.Event) bool {
		return ev.Kind == ice.UserStateChanged && ev.User.Session == s && ev.User.Mute
	})
	st = waitState(t, s, func(u ice.User) bool { return u.Channel == ch })
	if !st.Mute || st.SelfMute || st.Comment != "set by the test" {
		t.Errorf("after SetState: %+v", st)
	}

	perms, err := srv.EffectivePermissions(ctx, s, ch)
	must(t, err)
	if !perms.Has(ice.PermEnter) {
		t.Errorf("EffectivePermissions = %v", perms)
	}
	ok, err = srv.HasPermission(ctx, s, 0, ice.PermWrite)
	must(t, err)
	if ok {
		t.Error("an unregistered user has Write on the root")
	}

	must(t, srv.SendMessage(ctx, s, "hello bob"))
	if _, err := m.WaitFor(ctx, textFrame("hello bob")); err != nil {
		t.Error(err)
	}
	must(t, srv.SendMessageChannel(ctx, 0, true, "hello tree"))
	if _, err := m.WaitFor(ctx, textFrame("hello tree")); err != nil {
		t.Error(err)
	}
	must(t, srv.SendWelcomeMessage(ctx, []int32{s}))
	if _, err := m.WaitFor(ctx, func(f mumbletest.Frame) bool {
		return f.Type == mumbletest.MsgServerConfig && strings.Contains(f.Body.Str(2), "go-mumble-welcome")
	}); err != nil {
		t.Errorf("no welcome message: %v", err)
	}

	must(t, m.SendText(ch, "bob speaking"))
	ev := expectEvent(t, events, "UserTextMessage", func(ev ice.Event) bool { return ev.Kind == ice.UserTextMessage })
	if ev.User.Session != s || ev.Message.Text != "bob speaking" || !slices.Equal(ev.Message.Channels, []int32{ch}) {
		t.Errorf("UserTextMessage = %+v", ev)
	}

	must(t, srv.AddUserToGroup(ctx, ch, s, "gm_tmp"))
	must(t, srv.RemoveUserFromGroup(ctx, ch, s, "gm_tmp"))
	must(t, srv.RedirectWhisperGroup(ctx, s, "gm_a", "gm_b"))
	must(t, srv.RedirectWhisperGroup(ctx, s, "gm_a", ""))

	must(t, srv.StartListening(ctx, s, 0))
	if ok, err := srv.IsListening(ctx, s, 0); err != nil || !ok {
		t.Errorf("IsListening = %v, %v", ok, err)
	}
	lc, err := srv.ListeningChannels(ctx, s)
	must(t, err)
	if !slices.Equal(lc, []int32{0}) {
		t.Errorf("ListeningChannels = %v", lc)
	}
	lu, err := srv.ListeningUsers(ctx, 0)
	must(t, err)
	if !slices.Contains(lu, s) {
		t.Errorf("ListeningUsers = %v", lu)
	}
	must(t, srv.SetListenerVolumeAdjustment(ctx, 0, s, 0.5))
	if f, err := srv.ListenerVolumeAdjustment(ctx, 0, s); err != nil || f != 0.5 {
		t.Errorf("ListenerVolumeAdjustment = %v, %v", f, err)
	}
	must(t, srv.StopListening(ctx, s, 0))
	if ok, _ := srv.IsListening(ctx, s, 0); ok {
		t.Error("still listening")
	}

	must(t, srv.KickUser(ctx, s, "bye bob"))
	f, err := m.WaitFor(ctx, func(f mumbletest.Frame) bool {
		return f.Type == mumbletest.MsgUserRemove && f.Body.Uint(1) == uint64(s)
	})
	if err != nil || f.Body.Str(3) != "bye bob" {
		t.Errorf("kick: %v %q", err, f.Body.Str(3))
	}
	expectEvent(t, events, "UserDisconnected", func(ev ice.Event) bool {
		return ev.Kind == ice.UserDisconnected && ev.User.Session == s
	})

	_, err = srv.State(ctx, s)
	wantErr(t, err, ice.ErrInvalidSession)
	wantErr(t, srv.KickUser(ctx, s, "again"), ice.ErrInvalidSession)
	wantErr(t, srv.SendMessage(ctx, s, "gone"), ice.ErrInvalidSession)
	_, err = srv.CertificateList(ctx, s)
	wantErr(t, err, ice.ErrInvalidSession)
}

func TestContextCallback(t *testing.T) {
	ctx := testCtx(t)
	m := connect(t, "gm-carol", "")
	got := make(chan ice.ContextAction, 1)
	h, err := srv.AddContextCallback(ctx, session(m), "gm_poke", "Poke", ice.ContextServer|ice.ContextUser, func(a ice.ContextAction) {
		got <- a
	})
	must(t, err)
	defer h.Close(ctx)

	must(t, m.ContextAction("gm_poke", int64(m.Session), -1))
	select {
	case a := <-got:
		if a.Action != "gm_poke" || a.User.Session != session(m) || a.Session != session(m) {
			t.Errorf("ContextAction = %+v", a)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("context action never arrived")
	}

	_, err = srv.AddContextCallback(ctx, session(m), "gm_bad", "x", 0, func(ice.ContextAction) {})
	wantErr(t, err, ice.ErrInvalidCallback)
	_, err = srv.AddContextCallback(ctx, 999999, "gm_x", "x", ice.ContextServer, func(ice.ContextAction) {})
	wantErr(t, err, ice.ErrInvalidSession)
}

type testAuth struct {
	ice.UnimplementedUpdatingAuthenticator
	daveID    int32
	certHash  chan string
	registers chan ice.UserInfoMap
	reentrant chan error
}

func (a *testAuth) Authenticate(ctx context.Context, req ice.AuthRequest) (ice.AuthResult, error) {
	switch req.Name {
	case "gm-dave":
		if req.Password != "davepw" {
			return ice.AuthResult{UserID: ice.AuthFailed}, nil
		}
		select {
		case a.certHash <- req.CertHash:
		default:
		}
		return ice.AuthResult{UserID: a.daveID, Name: "gm-Dave", Groups: []string{"gm_auth"}}, nil
	case "gm-reentrant":
		// Murmur waits for the answer on the thread that would serve
		// this call, so it can only time out.
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		_, err := srv.Channels(cctx)
		a.reentrant <- err
		return ice.AuthResult{UserID: ice.Fallthrough}, nil
	case "gm-eve":
		return ice.AuthResult{UserID: ice.AuthFailed}, nil
	case "gm-temp":
		return ice.AuthResult{UserID: ice.AuthTemporaryFailure}, nil
	case "gm-err":
		return ice.AuthResult{}, errors.New("backend down")
	}
	return ice.AuthResult{UserID: ice.Fallthrough}, nil
}

func (a *testAuth) IDToName(_ context.Context, id int32) (string, error) {
	if id == a.daveID {
		return "gm-Dave", nil
	}
	return "", nil
}

func (a *testAuth) NameToID(_ context.Context, name string) (int32, error) {
	if strings.EqualFold(name, "gm-Dave") {
		return a.daveID, nil
	}
	return ice.Fallthrough, nil
}

func (a *testAuth) RegisterUser(_ context.Context, info ice.UserInfoMap) (int32, error) {
	select {
	case a.registers <- info:
	default:
	}
	return ice.Fallthrough, nil
}

func TestAuthenticator(t *testing.T) {
	ctx := testCtx(t)
	auth := &testAuth{daveID: 5000, certHash: make(chan string, 1), registers: make(chan ice.UserInfoMap, 1), reentrant: make(chan error, 1)}
	if is16() {
		// 1.6 terminates when an authenticator accepts an ID its own
		// database doesn't have, so give it one that it does.
		auth.daveID = registerUser(t, "gm-dave-db", "")
	}
	daveID := auth.daveID
	h, err := srv.SetAuthenticator(ctx, auth)
	must(t, err)
	t.Cleanup(func() {
		h.Close(context.Background())
		// 1.5 keeps a database row for every user an authenticator
		// accepted.
		_ = srv.UnregisterUser(context.Background(), daveID)
	})

	cert, _, _ := selfSignedCert(t, "gm-dave")
	dave := connect(t, "gm-dave", "davepw", mumbletest.Options{Certificate: &cert})
	u, err := srv.State(ctx, session(dave))
	must(t, err)
	if u.Name != "gm-Dave" || u.UserID != daveID {
		t.Errorf("dave = %+v", u)
	}
	sum := sha1.Sum(cert.Certificate[0])
	if got := <-auth.certHash; got != hex.EncodeToString(sum[:]) {
		t.Errorf("CertHash = %q, want the SHA-1 of the certificate", got)
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
	connect(t, "gm-frank", "").Close()

	connect(t, "gm-reentrant", "").Close()
	if err := <-auth.reentrant; !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("calling Murmur from inside the authenticator: %v, want a timeout", err)
	}

	names, err := srv.UserNames(ctx, []int32{daveID})
	must(t, err)
	if names[daveID] != "gm-Dave" {
		t.Errorf("UserNames = %v", names)
	}

	// An updating authenticator hears about registrations.
	id, err := srv.RegisterUser(ctx, ice.UserInfoMap{ice.UserName: "gm-reg", ice.UserEmail: "reg@example.com"})
	must(t, err)
	t.Cleanup(func() { _ = srv.UnregisterUser(context.Background(), id) })
	select {
	case info := <-auth.registers:
		if info[ice.UserName] != "gm-reg" {
			t.Errorf("RegisterUser got %v", info)
		}
	case <-time.After(5 * time.Second):
		t.Error("the authenticator never saw the registration")
	}

	// After Close, the next call fails and Murmur drops the authenticator,
	// so eve gets in through Murmur's own database.
	must(t, h.Close(ctx))
	connect(t, "gm-eve", "x").Close()
	log, err := srv.Log(ctx, 0, 50)
	must(t, err)
	if !slices.ContainsFunc(log, func(e ice.LogEntry) bool {
		return strings.Contains(e.Text, "Authenticator") && strings.Contains(e.Text, "failed")
	}) {
		t.Errorf("Murmur's log doesn't show the authenticator being dropped: %+v", log[:min(5, len(log))])
	}
}

func TestCallbackSecret(t *testing.T) {
	ctx := testCtx(t)
	wrong, err := ice.Dial(ctx, iceAddr, ice.WithSecret(secret), ice.WithCallbackSecret("not the secret"))
	must(t, err)
	defer wrong.Close()
	// Murmur takes the callback without calling it (its checkedCast to
	// the same type is local), so registering works...
	refused, err := wrong.Server(srv.ID()).Events(ctx)
	must(t, err)
	good, err := srv.Events(ctx)
	must(t, err)

	// ...but the events Murmur then sends carry its real secret and are
	// refused.
	must(t, admin.CreateChannel(0, "gm secret"))
	ev := expectEvent(t, good, "ChannelCreated", func(ev ice.Event) bool { return ev.Kind == ice.ChannelCreated })
	t.Cleanup(func() { _ = srv.RemoveChannel(context.Background(), ev.Channel.ID) })
	select {
	case ev := <-refused:
		t.Errorf("a callback with the wrong secret got %+v", ev)
	case <-time.After(2 * time.Second):
	}
}

func TestEventsEndWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	evs, err := srv.Events(ctx)
	must(t, err)
	cancel()
	select {
	case _, ok := <-evs:
		if ok {
			t.Error("event after cancel")
		}
	case <-time.After(5 * time.Second):
		t.Error("channel not closed by cancel")
	}
}

func TestUpdateCertificate(t *testing.T) {
	ctx := testCtx(t)
	_, certPEM, keyPEM := selfSignedCert(t, "gm-server")
	must(t, srv.UpdateCertificate(ctx, string(certPEM), string(keyPEM), ""))
	wantErr(t, srv.UpdateCertificate(ctx, "not a certificate", string(keyPEM), ""), ice.ErrInvalidInputData)
}

func TestDatabaseState(t *testing.T) {
	ctx := testCtx(t)
	st, err := client.AssumedDatabaseState(ctx)
	if !is16() {
		wantErr(t, err, ice.ErrUnsupported)
		return
	}
	must(t, err)
	if st != ice.DBNormal {
		t.Fatalf("state = %v", st)
	}
	t.Cleanup(func() { _ = client.SetAssumedDatabaseState(context.Background(), ice.DBNormal) })
	must(t, client.SetAssumedDatabaseState(ctx, ice.DBReadOnly))
	wantErr(t, srv.SetConf(ctx, "registername", "x"), ice.ErrReadOnlyMode)
	must(t, client.SetAssumedDatabaseState(ctx, ice.DBNormal))
	must(t, srv.SetConf(ctx, "registername", ""))
}
