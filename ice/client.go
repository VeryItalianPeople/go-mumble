package ice

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/VeryItalianPeople/go-mumble/internal/iceproto"
)

const (
	normal     = iceproto.ModeNormal
	idempotent = iceproto.ModeIdempotent
)

var metaIdentity = iceproto.Identity{Name: "Meta"}

// Client talks to one Murmur daemon over Ice. The methods defined on Client
// itself are the daemon-wide Meta interface; [Client.Server] returns a handle
// for one virtual server.
//
// A Client is safe for concurrent use.
type Client struct {
	rpc            *iceproto.Client
	secret         string
	callbackListen string
	callbackHost   string
	callbackSecret string
	noGuards       bool

	mu       sync.Mutex
	closed   bool
	adapter  *iceproto.Adapter
	advHost  string
	version  *Version
	handles  map[*Handle]struct{}
	identity int
}

// Option configures a [Client].
type Option func(*Client)

// WithSecret sends secret with every call, as Murmur's icesecretread or
// icesecretwrite requires. Calls that change anything, registering callbacks
// and authenticators included, need the write secret.
func WithSecret(secret string) Option {
	return func(c *Client) { c.secret = secret }
}

// WithDialer replaces the function that opens the TCP connection to Murmur.
func WithDialer(dial func(ctx context.Context, network, addr string) (net.Conn, error)) Option {
	return func(c *Client) { c.rpc.Dial = dial }
}

// WithCallbackAddress sets where the client listens for Murmur's calls to
// callbacks and authenticators, and the host Murmur is told to connect to.
//
// listen is a TCP address such as "0.0.0.0:6503". advertise is a host name
// or IP address that Murmur can reach the listener at. By default the client
// listens on a random port of the local address its connection to Murmur
// uses, and advertises that address, which works whenever Murmur can connect
// back the way the client connected to it.
func WithCallbackAddress(listen, advertise string) Option {
	return func(c *Client) {
		c.callbackListen = listen
		c.callbackHost = advertise
	}
}

// WithCallbackSecret makes the client refuse incoming calls that don't carry
// secret. Murmur sends its icesecretwrite with every call it makes, so set
// this to that value: otherwise anyone who can reach the callback port can
// call your authenticator or send fake events.
func WithCallbackSecret(secret string) Option {
	return func(c *Client) { c.callbackSecret = secret }
}

// WithoutCrashGuards turns off the checks that keep the client from making
// calls known to crash Murmur 1.6 (see [Server.Conf] and
// [Server.SetTexture]). Use it with a Murmur that has the bugs fixed, to
// save the extra round trips.
func WithoutCrashGuards() Option {
	return func(c *Client) { c.noGuards = true }
}

// Dial connects to Murmur's Ice endpoint, a "host:port" address (Murmur's
// ice setting looks like "tcp -h 127.0.0.1 -p 6502"). Only plain TCP is
// supported, not the ssl transport.
//
// The client reconnects by itself when Murmur closes an idle connection or
// the connection drops.
func Dial(ctx context.Context, addr string, opts ...Option) (*Client, error) {
	c := &Client{rpc: &iceproto.Client{Addr: addr}, handles: make(map[*Handle]struct{})}
	for _, o := range opts {
		o(c)
	}
	if _, err := c.rpc.LocalAddr(ctx); err != nil {
		return nil, fmt.Errorf("murmur: connecting to %s: %w", addr, err)
	}
	return c, nil
}

// Close removes every callback and authenticator the client registered,
// stops listening for Murmur's calls and closes the connection.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	handles := make([]*Handle, 0, len(c.handles))
	for h := range c.handles {
		handles = append(handles, h)
	}
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, h := range handles {
		h.Close(ctx)
	}
	c.mu.Lock()
	a := c.adapter
	c.mu.Unlock()
	if a != nil {
		a.Close()
	}
	return c.rpc.Close()
}

var errClosed = errors.New("murmur: client is closed")

func (c *Client) invoke(ctx context.Context, id iceproto.Identity, op string, mode byte, write func(*iceproto.Encoder)) (*iceproto.Decoder, error) {
	var e iceproto.Encoder
	if write != nil {
		write(&e)
	}
	var cctx map[string]string
	if c.secret != "" {
		cctx = map[string]string{"secret": c.secret}
	}
	d, err := c.rpc.Invoke(ctx, iceproto.Request{Identity: id, Operation: op, Mode: mode, Context: cctx, Params: e.Bytes()})
	if err != nil {
		return nil, wrapError(op, err)
	}
	return d, nil
}

func decodeErr(op string, d *iceproto.Decoder) error {
	if err := d.Err(); err != nil {
		return fmt.Errorf("murmur: %s: decoding the reply: %w", op, err)
	}
	return nil
}

// call runs an operation and decodes its reply with read, which may be nil.
func (c *Client) call(ctx context.Context, id iceproto.Identity, op string, mode byte, write func(*iceproto.Encoder), read func(*iceproto.Decoder)) error {
	d, err := c.invoke(ctx, id, op, mode, write)
	if err != nil {
		return err
	}
	if read != nil {
		read(d)
	}
	return decodeErr(op, d)
}

func (c *Client) metaCall(ctx context.Context, op string, mode byte, write func(*iceproto.Encoder), read func(*iceproto.Decoder)) error {
	return c.call(ctx, metaIdentity, op, mode, write, read)
}

// Server returns a handle on the virtual server with the given ID without
// calling Murmur. Mumble 1.6 starts with server 0, earlier versions with 1.
func (c *Client) Server(id int32) *Server {
	return &Server{c: c, id: id, ident: iceproto.Identity{Category: "s", Name: strconv.Itoa(int(id))}}
}

func (c *Client) serverFromProxy(op string, p *iceproto.Proxy) (*Server, error) {
	if p == nil {
		return nil, &Error{Operation: op, Err: errors.New("null proxy"), sentinel: ErrServerNotFound}
	}
	id, err := strconv.ParseInt(p.Identity.Name, 10, 32)
	if err != nil || p.Identity.Category != "s" {
		return nil, fmt.Errorf("murmur: %s: unexpected server proxy %s", op, p.Identity)
	}
	return c.Server(int32(id)), nil
}

func (c *Client) serverList(ctx context.Context, op string) ([]*Server, error) {
	var proxies []*iceproto.Proxy
	err := c.metaCall(ctx, op, idempotent, nil, func(d *iceproto.Decoder) {
		n := d.Size()
		for i := 0; i < n && d.Err() == nil; i++ {
			proxies = append(proxies, d.Proxy())
		}
	})
	if err != nil {
		return nil, err
	}
	servers := make([]*Server, 0, len(proxies))
	for _, p := range proxies {
		s, err := c.serverFromProxy(op, p)
		if err != nil {
			return nil, err
		}
		servers = append(servers, s)
	}
	return servers, nil
}

// GetServer returns the virtual server with the given ID, or
// [ErrServerNotFound].
func (c *Client) GetServer(ctx context.Context, id int32) (*Server, error) {
	var p *iceproto.Proxy
	err := c.metaCall(ctx, "getServer", idempotent, func(e *iceproto.Encoder) { e.Int(id) }, func(d *iceproto.Decoder) { p = d.Proxy() })
	if err != nil {
		return nil, err
	}
	return c.serverFromProxy("getServer", p)
}

// NewServer creates a virtual server. It is not started.
func (c *Client) NewServer(ctx context.Context) (*Server, error) {
	var p *iceproto.Proxy
	if err := c.metaCall(ctx, "newServer", normal, nil, func(d *iceproto.Decoder) { p = d.Proxy() }); err != nil {
		return nil, err
	}
	return c.serverFromProxy("newServer", p)
}

// BootedServers returns the running virtual servers.
func (c *Client) BootedServers(ctx context.Context) ([]*Server, error) {
	return c.serverList(ctx, "getBootedServers")
}

// AllServers returns every virtual server, running or not.
func (c *Client) AllServers(ctx context.Context) ([]*Server, error) {
	return c.serverList(ctx, "getAllServers")
}

// DefaultConf returns the configuration defaults every virtual server
// starts from.
func (c *Client) DefaultConf(ctx context.Context) (map[string]string, error) {
	var m map[string]string
	err := c.metaCall(ctx, "getDefaultConf", idempotent, nil, func(d *iceproto.Decoder) { m = d.Context() })
	return m, err
}

// Version returns the version of the Murmur daemon. It needs no secret.
func (c *Client) Version(ctx context.Context) (Version, error) {
	var v Version
	err := c.metaCall(ctx, "getVersion", idempotent, nil, func(d *iceproto.Decoder) {
		v = Version{Major: int(d.Int()), Minor: int(d.Int()), Patch: int(d.Int()), Text: d.String()}
	})
	if err == nil {
		c.mu.Lock()
		c.version = &v
		c.mu.Unlock()
	}
	return v, err
}

// cachedVersion returns the daemon's version, asking once per client.
func (c *Client) cachedVersion(ctx context.Context) (Version, error) {
	c.mu.Lock()
	v := c.version
	c.mu.Unlock()
	if v != nil {
		return *v, nil
	}
	return c.Version(ctx)
}

// Uptime returns how long the daemon has been running.
func (c *Client) Uptime(ctx context.Context) (time.Duration, error) {
	var s int32
	err := c.metaCall(ctx, "getUptime", idempotent, nil, func(d *iceproto.Decoder) { s = d.Int() })
	return time.Duration(s) * time.Second, err
}

// Slice returns the Slice definition (MumbleServer.ice) the daemon was
// built with.
func (c *Client) Slice(ctx context.Context) (string, error) {
	var s string
	err := c.metaCall(ctx, "getSlice", idempotent, nil, func(d *iceproto.Decoder) { s = d.String() })
	return s, err
}

// SliceChecksums returns the checksums of the daemon's Slice definitions, by
// type ID.
func (c *Client) SliceChecksums(ctx context.Context) (map[string]string, error) {
	var m map[string]string
	err := c.metaCall(ctx, "getSliceChecksums", idempotent, nil, func(d *iceproto.Decoder) { m = d.Context() })
	return m, err
}

// AssumedDatabaseState reports whether Murmur treats its database as
// read-only. Mumble 1.6 and later; earlier versions return [ErrUnsupported].
func (c *Client) AssumedDatabaseState(ctx context.Context) (DBState, error) {
	var s DBState
	err := c.metaCall(ctx, "getAssumedDatabaseState", idempotent, nil, func(d *iceproto.Decoder) { s = DBState(d.Enum()) })
	return s, err
}

// SetAssumedDatabaseState switches Murmur's database between normal and
// read-only. In read-only mode, calls that would write fail with
// [ErrReadOnlyMode]. Mumble 1.6 and later.
func (c *Client) SetAssumedDatabaseState(ctx context.Context, state DBState) error {
	return c.metaCall(ctx, "setAssumedDatabaseState", idempotent, func(e *iceproto.Encoder) { e.Enum(int32(state)) }, nil)
}
