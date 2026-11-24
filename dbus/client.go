package dbus

import (
	"context"
	"errors"
	"fmt"
	"sync"

	godbus "github.com/godbus/dbus/v5"
)

// DefaultService is the bus name Murmur registers unless its configuration
// sets dbusservice.
const DefaultService = "net.sourceforge.mumble.murmur"

const (
	metaInterface   = "net.sourceforge.mumble.Meta"
	serverInterface = "net.sourceforge.mumble.Murmur"
	metaPath        = godbus.ObjectPath("/")
)

// Client talks to one Murmur daemon over D-Bus. The methods defined on Client
// itself are the daemon-wide "Meta" interface: they create, start and
// configure virtual servers. Use [Client.Server] for the methods of a single
// virtual server.
//
// A Client is safe for concurrent use.
type Client struct {
	conn     *godbus.Conn
	ownsConn bool
	service  string
	meta     godbus.BusObject

	mu       sync.Mutex
	closed   bool
	sigCh    chan *godbus.Signal
	subs     map[*subscription]struct{}
	authN    int
	authRegs map[*AuthenticatorRegistration]struct{}
	done     chan struct{}
	wg       sync.WaitGroup
}

// Option configures a [Client].
type Option func(*options)

type options struct {
	service  string
	connOpts []godbus.ConnOption
}

// WithService sets the bus name Murmur is registered under, for a Murmur
// configured with a non-default dbusservice. The default is [DefaultService].
func WithService(name string) Option {
	return func(o *options) { o.service = name }
}

// WithConnOptions passes options to godbus when the connection is opened by
// [Connect], [ConnectSystemBus] or [ConnectSessionBus]. For example,
// godbus.WithAuth(godbus.AuthAnonymous()) authenticates to a bus that accepts
// anonymous clients over TCP. It has no effect on [New].
func WithConnOptions(opts ...godbus.ConnOption) Option {
	return func(o *options) { o.connOpts = append(o.connOpts, opts...) }
}

func buildOptions(opts []Option) options {
	o := options{service: DefaultService}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// ConnectSystemBus connects to Murmur on the system bus, which is where a
// Murmur configured with dbus=system registers.
func ConnectSystemBus(opts ...Option) (*Client, error) {
	o := buildOptions(opts)
	conn, err := godbus.ConnectSystemBus(o.connOpts...)
	if err != nil {
		return nil, fmt.Errorf("murmur: connecting to the system bus: %w", err)
	}
	return newClient(conn, true, o), nil
}

// ConnectSessionBus connects to Murmur on the session bus (dbus=session).
func ConnectSessionBus(opts ...Option) (*Client, error) {
	o := buildOptions(opts)
	conn, err := godbus.ConnectSessionBus(o.connOpts...)
	if err != nil {
		return nil, fmt.Errorf("murmur: connecting to the session bus: %w", err)
	}
	return newClient(conn, true, o), nil
}

// Connect connects to Murmur on the bus at address, a D-Bus address such as
// "unix:path=/run/murmur/bus" (Murmur accepts a bus address in its dbus
// setting too).
func Connect(address string, opts ...Option) (*Client, error) {
	o := buildOptions(opts)
	conn, err := godbus.Connect(address, o.connOpts...)
	if err != nil {
		return nil, fmt.Errorf("murmur: connecting to %s: %w", address, err)
	}
	return newClient(conn, true, o), nil
}

// New returns a Client that uses an existing connection. The connection must
// already be authenticated and have sent Hello, as the ones returned by
// godbus.ConnectSystemBus and godbus.Connect have. [Client.Close] leaves it
// open.
func New(conn *godbus.Conn, opts ...Option) *Client {
	return newClient(conn, false, buildOptions(opts))
}

func newClient(conn *godbus.Conn, owns bool, o options) *Client {
	return &Client{
		conn:     conn,
		ownsConn: owns,
		service:  o.service,
		meta:     conn.Object(o.service, metaPath),
		subs:     make(map[*subscription]struct{}),
		authRegs: make(map[*AuthenticatorRegistration]struct{}),
		done:     make(chan struct{}),
	}
}

// Conn returns the underlying godbus connection.
func (c *Client) Conn() *godbus.Conn {
	return c.conn
}

// Service returns the bus name the client sends calls to.
func (c *Client) Service() string {
	return c.service
}

// Close ends every event subscription, withdraws every authenticator this
// client registered, and closes the connection if the client opened it.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	regs := make([]*AuthenticatorRegistration, 0, len(c.authRegs))
	for r := range c.authRegs {
		regs = append(regs, r)
	}
	subs := make([]*subscription, 0, len(c.subs))
	for s := range c.subs {
		subs = append(subs, s)
	}
	sigCh := c.sigCh
	c.mu.Unlock()

	for _, r := range regs {
		r.unexport()
	}
	for _, s := range subs {
		s.cancel()
	}
	if sigCh != nil {
		c.conn.RemoveSignal(sigCh)
	}
	close(c.done)
	c.wg.Wait()

	if c.ownsConn {
		return c.conn.Close()
	}
	return nil
}

var errClosed = errors.New("murmur: client is closed")

// call runs a method and stores its reply in out. A nil error from godbus
// with a Store error means Murmur answered with a body of the wrong shape.
func call(ctx context.Context, obj godbus.BusObject, iface, method string, args []any, out ...any) error {
	c := obj.CallWithContext(ctx, iface+"."+method, 0, args...)
	if c.Err != nil {
		return wrapError(method, c.Err)
	}
	if len(out) == 0 {
		return nil
	}
	if err := c.Store(out...); err != nil {
		return fmt.Errorf("murmur: %s: decoding reply: %w", method, err)
	}
	return nil
}

func (c *Client) metaCall(ctx context.Context, method string, args []any, out ...any) error {
	return call(ctx, c.meta, metaInterface, method, args, out...)
}

// Start boots the virtual server with the given ID.
func (c *Client) Start(ctx context.Context, serverID int32) error {
	return c.metaCall(ctx, "start", []any{serverID})
}

// Stop shuts down the virtual server with the given ID.
func (c *Client) Stop(ctx context.Context, serverID int32) error {
	return c.metaCall(ctx, "stop", []any{serverID})
}

// NewServer creates a virtual server and returns its ID. The new server is
// not started.
func (c *Client) NewServer(ctx context.Context) (int32, error) {
	var id int32
	err := c.metaCall(ctx, "newServer", nil, &id)
	return id, err
}

// DeleteServer deletes a stopped virtual server and all of its data.
func (c *Client) DeleteServer(ctx context.Context, serverID int32) error {
	return c.metaCall(ctx, "deleteServer", []any{serverID})
}

// BootedServers returns the IDs of the running virtual servers.
func (c *Client) BootedServers(ctx context.Context) ([]int32, error) {
	var ids []int32
	err := c.metaCall(ctx, "getBootedServers", nil, &ids)
	return nonNil(ids), err
}

// AllServers returns the IDs of every virtual server, running or not.
func (c *Client) AllServers(ctx context.Context) ([]int32, error) {
	var ids []int32
	err := c.metaCall(ctx, "getAllServers", nil, &ids)
	return nonNil(ids), err
}

// IsBooted reports whether a virtual server is running. It returns false for
// an ID that doesn't exist.
func (c *Client) IsBooted(ctx context.Context, serverID int32) (bool, error) {
	var booted bool
	err := c.metaCall(ctx, "isBooted", []any{serverID}, &booted)
	return booted, err
}

// Conf returns a configuration value stored for a virtual server. It returns
// an empty string for a key that has no value of its own, in which case the
// server uses the default from [Client.DefaultConf]. The keys "key" and
// "passphrase" can be written but not read ([ErrWriteOnly]).
func (c *Client) Conf(ctx context.Context, serverID int32, key string) (string, error) {
	var v string
	err := c.metaCall(ctx, "getConf", []any{serverID, key}, &v)
	return v, err
}

// AllConf returns every configuration value stored for a virtual server,
// without the write-only "key" and "passphrase".
func (c *Client) AllConf(ctx context.Context, serverID int32) (map[string]string, error) {
	var m map[string]string
	err := c.metaCall(ctx, "getAllConf", []any{serverID}, &m)
	return nonNilMap(m), err
}

// DefaultConf returns the defaults that apply to every virtual server, read
// from Murmur's ini file.
func (c *Client) DefaultConf(ctx context.Context) (map[string]string, error) {
	var m map[string]string
	err := c.metaCall(ctx, "getDefaultConf", nil, &m)
	return nonNilMap(m), err
}

// SetConf stores a configuration value for a virtual server. A running
// server applies it immediately where it can; some keys, such as the port,
// need a restart of the virtual server.
func (c *Client) SetConf(ctx context.Context, serverID int32, key, value string) error {
	return c.metaCall(ctx, "setConf", []any{serverID, key, value})
}

// SetSuperUserPassword sets the password of the SuperUser account of a
// virtual server.
func (c *Client) SetSuperUserPassword(ctx context.Context, serverID int32, password string) error {
	return c.metaCall(ctx, "setSuperUserPassword", []any{serverID, password})
}

// Log returns up to count lines of a virtual server's log, newest first,
// skipping the newest offset lines.
//
// Murmur names the two arguments min_offset and max_offset, but passes the
// second straight to the SQL LIMIT, so it is a count.
func (c *Client) Log(ctx context.Context, serverID, offset, count int32) ([]LogEntry, error) {
	var w []wireLogEntry
	if err := c.metaCall(ctx, "getLog", []any{serverID, offset, count}, &w); err != nil {
		return nil, err
	}
	return mapSlice(w, wireLogEntry.entry), nil
}

// Version returns the version of the Murmur daemon.
//
// Only Murmur 1.3 and earlier export getVersion over D-Bus. Later versions
// declare it with a type Qt can't marshal, so it never appears on the bus and
// Version returns [ErrUnsupported].
func (c *Client) Version(ctx context.Context) (Version, error) {
	res := c.meta.CallWithContext(ctx, metaInterface+".getVersion", 0)
	if res.Err != nil {
		return Version{}, wrapError("getVersion", res.Err)
	}
	if len(res.Body) != 4 {
		return Version{}, fmt.Errorf("murmur: getVersion: reply has %d values, want 4", len(res.Body))
	}
	var v Version
	for i, dst := range []*int{&v.Major, &v.Minor, &v.Patch} {
		n, ok := toInt(res.Body[i])
		if !ok {
			return Version{}, fmt.Errorf("murmur: getVersion: value %d is %T, want an integer", i, res.Body[i])
		}
		*dst = n
	}
	s, ok := res.Body[3].(string)
	if !ok {
		return Version{}, fmt.Errorf("murmur: getVersion: value 3 is %T, want a string", res.Body[3])
	}
	v.Release = s
	return v, nil
}

// Quit tells the Murmur daemon to exit.
func (c *Client) Quit(ctx context.Context) error {
	return c.metaCall(ctx, "quit", nil)
}

// toInt accepts the integer types a version component has been marshalled
// as: int32 up to Murmur 1.3, and uint16 should a later Qt export it.
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int32:
		return int(n), true
	case uint32:
		return int(n), true
	case int16:
		return int(n), true
	case uint16:
		return int(n), true
	case int64:
		return int(n), true
	case uint64:
		return int(n), true
	case byte:
		return int(n), true
	}
	return 0, false
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
