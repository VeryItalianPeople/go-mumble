package iceproto

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Message types of protocol 1.0.
const (
	msgRequest      = 0
	msgBatchRequest = 1
	msgReply        = 2
	msgValidate     = 3
	msgClose        = 4
)

const headerSize = 14

// maxMessageSize bounds what is read from a peer. Ice's own default is 1 MiB;
// a big getTree or getLog reply can go past that.
const maxMessageSize = 64 << 20

// Reply statuses.
const (
	replyOK                    = 0
	replyUserException         = 1
	replyObjectNotExist        = 2
	replyFacetNotExist         = 3
	replyOperationNotExist     = 4
	replyUnknownLocalException = 5
	replyUnknownUserException  = 6
	replyUnknownException      = 7
)

// Operation modes. An Ice server rejects a request whose mode doesn't match
// the operation's Slice definition, so callers must get this right.
const (
	ModeNormal     byte = 0
	ModeIdempotent byte = 2
)

func header(typ byte, size int) []byte {
	h := make([]byte, headerSize, size)
	copy(h, "IceP")
	h[4], h[5] = 1, 0 // protocol 1.0
	h[6], h[7] = 1, 0 // protocol encoding 1.0
	h[8] = typ
	h[9] = 0 // not compressed
	binary.LittleEndian.PutUint32(h[10:], uint32(size))
	return h
}

func readMessage(r io.Reader) (byte, []byte, error) {
	var h [headerSize]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	if string(h[:4]) != "IceP" {
		return 0, nil, fmt.Errorf("iceproto: bad magic %q", h[:4])
	}
	if h[4] != 1 {
		return 0, nil, fmt.Errorf("iceproto: unsupported protocol %d.%d", h[4], h[5])
	}
	if h[9] == 2 {
		return 0, nil, errors.New("iceproto: compressed messages are not supported")
	}
	size := int(binary.LittleEndian.Uint32(h[10:]))
	if size < headerSize || size > maxMessageSize {
		return 0, nil, fmt.Errorf("iceproto: message size %d", size)
	}
	body := make([]byte, size-headerSize)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return h[8], body, nil
}

// UserException is a Slice exception thrown by the remote operation.
type UserException struct {
	TypeID string // e.g. "::MumbleServer::InvalidChannelException"
}

func (e *UserException) Error() string { return "ice: user exception " + e.TypeID }

// DispatchError is one of Ice's dispatch failures: no such object, facet or
// operation, or an exception the server could not pass on as is.
type DispatchError struct {
	Status    byte
	Identity  Identity
	Facet     string
	Operation string
	Text      string
}

// Dispatch error statuses, for DispatchError.Status.
const (
	ObjectNotExist    = replyObjectNotExist
	FacetNotExist     = replyFacetNotExist
	OperationNotExist = replyOperationNotExist
	UnknownLocal      = replyUnknownLocalException
	UnknownUser       = replyUnknownUserException
	Unknown           = replyUnknownException
)

func (e *DispatchError) Error() string {
	switch e.Status {
	case replyObjectNotExist:
		return fmt.Sprintf("ice: object %s does not exist", e.Identity)
	case replyFacetNotExist:
		return fmt.Sprintf("ice: facet %q of %s does not exist", e.Facet, e.Identity)
	case replyOperationNotExist:
		return fmt.Sprintf("ice: %s has no operation %s", e.Identity, e.Operation)
	}
	return "ice: remote exception: " + e.Text
}

// Request is an outgoing invocation.
type Request struct {
	Identity  Identity
	Operation string
	Mode      byte
	Context   map[string]string
	// Params are the encoded in-parameters, without the encapsulation.
	Params []byte
}

// errConnClosed marks a connection that died. graceful means the server
// announced the close, so a request it never answered was not dispatched
// and can be sent again.
type errConnClosed struct {
	graceful bool
	cause    error
}

func (e *errConnClosed) Error() string {
	if e.graceful {
		return "iceproto: connection closed by the server"
	}
	return "iceproto: connection lost: " + e.cause.Error()
}

func (e *errConnClosed) Unwrap() error { return e.cause }

type result struct {
	body *Decoder // the reply after the request ID
	err  error
}

type clientConn struct {
	nc      net.Conn
	wmu     sync.Mutex
	mu      sync.Mutex
	pending map[int32]chan result
	nextID  int32
	dead    error
	done    chan struct{}
}

// Client sends requests to one Ice endpoint over TCP. It dials on first use,
// keeps one connection, sends heartbeats so the server's idle timeout leaves
// it open, and dials again after the connection is lost. It is safe for
// concurrent use.
type Client struct {
	Addr string
	// Dial opens the TCP connection. The default is a net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// Heartbeat is the interval between heartbeats; 0 means 30 seconds.
	Heartbeat time.Duration

	mu     sync.Mutex
	cur    *clientConn
	closed bool
}

// ErrClientClosed is returned by Invoke after Close.
var ErrClientClosed = errors.New("iceproto: client closed")

// Close closes the connection. Requests in flight fail.
func (c *Client) Close() error {
	c.mu.Lock()
	c.closed = true
	cur := c.cur
	c.cur = nil
	c.mu.Unlock()
	if cur != nil {
		cur.shutdown(ErrClientClosed, true)
	}
	return nil
}

// LocalAddr returns the local address of the current connection, dialling
// one if needed. Callback adapters use it to find an address the server can
// reach.
func (c *Client) LocalAddr(ctx context.Context) (net.Addr, error) {
	cc, err := c.conn(ctx)
	if err != nil {
		return nil, err
	}
	return cc.nc.LocalAddr(), nil
}

func (c *Client) conn(ctx context.Context) (*clientConn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClientClosed
	}
	if c.cur != nil {
		c.cur.mu.Lock()
		dead := c.cur.dead
		c.cur.mu.Unlock()
		if dead == nil {
			return c.cur, nil
		}
		c.cur = nil
	}
	dial := c.Dial
	if dial == nil {
		var d net.Dialer
		dial = d.DialContext
	}
	nc, err := dial(ctx, "tcp", c.Addr)
	if err != nil {
		return nil, err
	}
	// The server speaks first: wait for its ValidateConnection.
	if dl, ok := ctx.Deadline(); ok {
		nc.SetReadDeadline(dl)
	} else {
		nc.SetReadDeadline(time.Now().Add(30 * time.Second))
	}
	typ, _, err := readMessage(nc)
	nc.SetReadDeadline(time.Time{})
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("iceproto: waiting for the server to validate the connection: %w", err)
	}
	if typ != msgValidate {
		nc.Close()
		return nil, fmt.Errorf("iceproto: expected ValidateConnection, got message type %d", typ)
	}
	cc := &clientConn{nc: nc, pending: make(map[int32]chan result), nextID: 1, done: make(chan struct{})}
	go cc.readLoop()
	hb := c.Heartbeat
	if hb <= 0 {
		hb = 30 * time.Second
	}
	go cc.heartbeat(hb)
	c.cur = cc
	return cc, nil
}

// Invoke sends a twoway request and returns a decoder for the reply's
// out-parameters and return value. A user exception comes back as
// *UserException, a dispatch failure as *DispatchError.
//
// A request is sent again on a new connection if the server closed the old
// one before answering, or if the connection was lost and the operation is
// idempotent, as Ice itself does.
func (c *Client) Invoke(ctx context.Context, req Request) (*Decoder, error) {
	for attempt := 0; ; attempt++ {
		cc, err := c.conn(ctx)
		if err != nil {
			return nil, err
		}
		d, err := cc.invoke(ctx, req)
		var closed *errConnClosed
		if attempt == 0 && errors.As(err, &closed) && (closed.graceful || req.Mode == ModeIdempotent) && ctx.Err() == nil {
			continue
		}
		return d, err
	}
}

func (cc *clientConn) invoke(ctx context.Context, req Request) (*Decoder, error) {
	ch := make(chan result, 1)
	cc.mu.Lock()
	if cc.dead != nil {
		err := cc.dead
		cc.mu.Unlock()
		return nil, err
	}
	id := cc.nextID
	cc.nextID++
	if cc.nextID <= 0 {
		cc.nextID = 1
	}
	cc.pending[id] = ch
	cc.mu.Unlock()
	defer func() {
		cc.mu.Lock()
		delete(cc.pending, id)
		cc.mu.Unlock()
	}()

	var e Encoder
	e.buf = header(msgRequest, headerSize)
	e.Int(id)
	e.Identity(req.Identity)
	e.Size(0) // facet
	e.String(req.Operation)
	e.Byte(req.Mode)
	e.Context(req.Context)
	pos := e.StartEncaps()
	e.buf = append(e.buf, req.Params...)
	e.EndEncaps(pos)
	binary.LittleEndian.PutUint32(e.buf[10:], uint32(len(e.buf)))

	cc.wmu.Lock()
	if dl, ok := ctx.Deadline(); ok {
		cc.nc.SetWriteDeadline(dl)
	} else {
		cc.nc.SetWriteDeadline(time.Time{})
	}
	_, err := cc.nc.Write(e.buf)
	cc.wmu.Unlock()
	if err != nil {
		cc.shutdown(err, false)
		// Nothing reached the server for sure, but a partial write may
		// have; only retry what Ice would.
		return nil, &errConnClosed{cause: err}
	}

	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		return decodeReply(r.body)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func decodeReply(d *Decoder) (*Decoder, error) {
	status := d.Byte()
	switch status {
	case replyOK:
		enc := d.Encaps()
		return enc, enc.Err()
	case replyUserException:
		enc := d.Encaps()
		id := enc.ExceptionTypeID()
		if err := enc.Err(); err != nil {
			return nil, fmt.Errorf("iceproto: decoding user exception: %w", err)
		}
		return nil, &UserException{TypeID: id}
	case replyObjectNotExist, replyFacetNotExist, replyOperationNotExist:
		e := &DispatchError{Status: status, Identity: d.Identity()}
		if f := d.Strings(); len(f) > 0 {
			e.Facet = f[0]
		}
		e.Operation = d.String()
		return nil, e
	case replyUnknownLocalException, replyUnknownUserException, replyUnknownException:
		return nil, &DispatchError{Status: status, Text: d.String()}
	}
	return nil, fmt.Errorf("iceproto: unknown reply status %d", status)
}

func (cc *clientConn) readLoop() {
	for {
		typ, body, err := readMessage(cc.nc)
		if err != nil {
			cc.shutdown(err, false)
			return
		}
		switch typ {
		case msgReply:
			d := NewDecoder(body)
			id := d.Int()
			cc.mu.Lock()
			ch := cc.pending[id]
			cc.mu.Unlock()
			if ch != nil {
				select {
				case ch <- result{body: d}:
				default: // a duplicate reply
				}
			}
		case msgClose:
			cc.shutdown(errors.New("closed by the server"), true)
			return
		case msgValidate:
			// heartbeat
		default:
			// Requests on a client connection would need a
			// bidirectional adapter, which we don't offer.
		}
	}
}

func (cc *clientConn) heartbeat(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			cc.wmu.Lock()
			cc.nc.SetWriteDeadline(time.Now().Add(10 * time.Second))
			_, err := cc.nc.Write(header(msgValidate, headerSize))
			cc.wmu.Unlock()
			if err != nil {
				cc.shutdown(err, false)
				return
			}
		case <-cc.done:
			return
		}
	}
}

// shutdown marks the connection dead and fails every pending request. When
// local is true, a CloseConnection message is sent first so the server knows
// the close was intended.
func (cc *clientConn) shutdown(cause error, graceful bool) {
	cc.mu.Lock()
	if cc.dead != nil {
		cc.mu.Unlock()
		return
	}
	var err error = &errConnClosed{graceful: graceful, cause: cause}
	if errors.Is(cause, ErrClientClosed) {
		err = ErrClientClosed
		cc.wmu.Lock()
		cc.nc.SetWriteDeadline(time.Now().Add(time.Second))
		cc.nc.Write(header(msgClose, headerSize))
		cc.wmu.Unlock()
	}
	cc.dead = err
	pending := cc.pending
	cc.pending = map[int32]chan result{}
	close(cc.done)
	cc.mu.Unlock()
	cc.nc.Close()
	for _, ch := range pending {
		select {
		case ch <- result{err: err}:
		default: // already answered
		}
	}
}
