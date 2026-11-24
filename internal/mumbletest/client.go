// Package mumbletest is a minimal Mumble control-protocol client for the
// integration tests. It puts users on a server so that the user methods,
// the events and the authenticators have someone to act on.
//
// Messages are protobuf, encoded by hand for the handful of fields used;
// field numbers are from Mumble's Mumble.proto. Add a constant for a new
// field rather than a bare number.
package mumbletest

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// Message types.
const (
	MsgVersion          = 0
	MsgUDPTunnel        = 1
	MsgAuthenticate     = 2
	MsgPing             = 3
	MsgReject           = 4
	MsgServerSync       = 5
	MsgChannelRemove    = 6
	MsgChannelState     = 7
	MsgUserRemove       = 8
	MsgUserState        = 9
	MsgTextMessage      = 11
	MsgPermissionDenied = 12
	MsgContextAction    = 17
	MsgServerConfig     = 24
)

// Reject types.
const (
	RejectWrongUserPW       = 3
	RejectAuthenticatorFail = 8
)

// Frame is a received message.
type Frame struct {
	Type uint16
	Body Msg
}

// Conn is a logged-in client.
type Conn struct {
	Session uint32

	conn   *tls.Conn
	mu     sync.Mutex
	frames []Frame
	wake   chan struct{}
	closed chan struct{}
	err    error
}

// RejectError is a Reject from the server.
type RejectError struct {
	Type   uint64
	Reason string
}

func (e *RejectError) Error() string {
	return fmt.Sprintf("rejected (type %d): %s", e.Type, e.Reason)
}

// Options are optional login settings.
type Options struct {
	// Certificate is presented as the client certificate.
	Certificate *tls.Certificate
}

// Dial connects and authenticates, returning once the server has sent
// ServerSync. A Reject comes back as *RejectError.
func Dial(ctx context.Context, addr, user, password string, opts ...Options) (*Conn, error) {
	cfg := &tls.Config{InsecureSkipVerify: true}
	for _, o := range opts {
		if o.Certificate != nil {
			cfg.Certificates = []tls.Certificate{*o.Certificate}
		}
	}
	d := tls.Dialer{Config: cfg}
	nc, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	m := &Conn{conn: nc.(*tls.Conn), wake: make(chan struct{}, 1), closed: make(chan struct{})}

	var v []byte
	v = Varint(v, 1, 1<<16|4<<8) // legacy version field: 1.4.0
	v = Bytes(v, 2, []byte("go-mumble-test"))
	var a []byte
	a = Bytes(a, 1, []byte(user))
	a = Bytes(a, 2, []byte(password))
	a = Varint(a, 5, 1) // opus
	if err := m.Send(MsgVersion, v); err != nil {
		nc.Close()
		return nil, err
	}
	if err := m.Send(MsgAuthenticate, a); err != nil {
		nc.Close()
		return nil, err
	}
	go m.readLoop()
	go m.pingLoop()

	f, err := m.WaitFor(ctx, func(f Frame) bool { return f.Type == MsgServerSync || f.Type == MsgReject })
	if err != nil {
		m.Close()
		return nil, err
	}
	if f.Type == MsgReject {
		m.Close()
		return nil, &RejectError{Type: f.Body.Uint(1), Reason: f.Body.Str(2)}
	}
	m.Session = uint32(f.Body.Uint(1))
	return m, nil
}

// Send writes one message.
func (m *Conn) Send(typ uint16, body []byte) error {
	var hdr [6]byte
	binary.BigEndian.PutUint16(hdr[:2], typ)
	binary.BigEndian.PutUint32(hdr[2:], uint32(len(body)))
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.conn.Write(hdr[:]); err != nil {
		return err
	}
	_, err := m.conn.Write(body)
	return err
}

func (m *Conn) readLoop() {
	defer close(m.closed)
	for {
		var hdr [6]byte
		if _, err := io.ReadFull(m.conn, hdr[:]); err != nil {
			m.setErr(err)
			return
		}
		body := make([]byte, binary.BigEndian.Uint32(hdr[2:]))
		if _, err := io.ReadFull(m.conn, body); err != nil {
			m.setErr(err)
			return
		}
		typ := binary.BigEndian.Uint16(hdr[:2])
		if typ == MsgUDPTunnel { // raw audio, not protobuf
			continue
		}
		msg, err := Parse(body)
		if err != nil {
			continue
		}
		m.mu.Lock()
		if len(m.frames) >= 1000 { // nobody is reading this connection
			m.frames = m.frames[1:]
		}
		m.frames = append(m.frames, Frame{Type: typ, Body: msg})
		m.mu.Unlock()
		select {
		case m.wake <- struct{}{}:
		default:
		}
	}
}

func (m *Conn) setErr(err error) {
	m.mu.Lock()
	m.err = err
	m.mu.Unlock()
}

// pingLoop keeps the connection past Murmur's idle timeout.
func (m *Conn) pingLoop() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			_ = m.Send(MsgPing, Varint(nil, 1, uint64(time.Now().UnixNano())))
		case <-m.closed:
			return
		}
	}
}

// WaitFor returns the first received frame matching pred, consuming it and
// everything before it.
func (m *Conn) WaitFor(ctx context.Context, pred func(Frame) bool) (Frame, error) {
	for {
		m.mu.Lock()
		for i, f := range m.frames {
			if pred(f) {
				m.frames = m.frames[i+1:]
				m.mu.Unlock()
				return f, nil
			}
		}
		m.frames = m.frames[:0]
		err := m.err
		m.mu.Unlock()
		select {
		case <-m.wake:
		case <-m.closed:
			m.mu.Lock()
			for i, f := range m.frames {
				if pred(f) {
					m.frames = m.frames[i+1:]
					m.mu.Unlock()
					return f, nil
				}
			}
			m.mu.Unlock()
			if err == nil {
				err = errors.New("connection closed")
			}
			return Frame{}, err
		case <-ctx.Done():
			return Frame{}, ctx.Err()
		}
	}
}

// CreateChannel asks the server for a new channel, as a user would.
func (m *Conn) CreateChannel(parent int32, name string) error {
	var b []byte
	b = Varint(b, 2, uint64(parent))
	b = Bytes(b, 3, []byte(name))
	return m.Send(MsgChannelState, b)
}

// RemoveChannel asks the server to delete a channel.
func (m *Conn) RemoveChannel(id int32) error {
	return m.Send(MsgChannelRemove, Varint(nil, 1, uint64(id)))
}

// JoinChannel asks to move into a channel.
func (m *Conn) JoinChannel(channelID int32) error {
	var b []byte
	b = Varint(b, 1, uint64(m.Session))
	b = Varint(b, 5, uint64(channelID))
	return m.Send(MsgUserState, b)
}

// SendText sends a text message to a channel.
func (m *Conn) SendText(channelID int32, text string) error {
	var b []byte
	b = Varint(b, 3, uint64(channelID))
	b = Bytes(b, 5, []byte(text))
	return m.Send(MsgTextMessage, b)
}

// ContextAction clicks a context menu entry, on a session and/or channel
// (pass -1 to leave one out).
func (m *Conn) ContextAction(action string, session, channelID int64) error {
	var b []byte
	if session >= 0 {
		b = Varint(b, 1, uint64(session))
	}
	if channelID >= 0 {
		b = Varint(b, 2, uint64(channelID))
	}
	b = Bytes(b, 3, []byte(action))
	return m.Send(MsgContextAction, b)
}

// Close disconnects and waits for the reader to stop.
func (m *Conn) Close() {
	m.conn.Close()
	<-m.closed
}

// Msg is a parsed protobuf message: values by field number.
type Msg map[int][]Val

// Val is one field value.
type Val struct {
	U uint64
	B []byte
}

// Uint returns the last value of a varint field.
func (m Msg) Uint(field int) uint64 {
	if v := m[field]; len(v) > 0 {
		return v[len(v)-1].U
	}
	return 0
}

// Str returns the last value of a bytes field.
func (m Msg) Str(field int) string {
	if v := m[field]; len(v) > 0 {
		return string(v[len(v)-1].B)
	}
	return ""
}

// Has reports whether a field is present.
func (m Msg) Has(field int) bool { return len(m[field]) > 0 }

// Varint appends a varint field.
func Varint(b []byte, field int, v uint64) []byte {
	b = binary.AppendUvarint(b, uint64(field)<<3)
	return binary.AppendUvarint(b, v)
}

// Bytes appends a length-delimited field.
func Bytes(b []byte, field int, v []byte) []byte {
	b = binary.AppendUvarint(b, uint64(field)<<3|2)
	b = binary.AppendUvarint(b, uint64(len(v)))
	return append(b, v...)
}

// Parse decodes a protobuf message.
func Parse(b []byte) (Msg, error) {
	m := Msg{}
	for len(b) > 0 {
		key, n := binary.Uvarint(b)
		if n <= 0 {
			return nil, errors.New("bad key")
		}
		b = b[n:]
		field := int(key >> 3)
		switch key & 7 {
		case 0:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return nil, errors.New("bad varint")
			}
			b = b[n:]
			m[field] = append(m[field], Val{U: v})
		case 1:
			if len(b) < 8 {
				return nil, errors.New("short fixed64")
			}
			m[field] = append(m[field], Val{U: binary.LittleEndian.Uint64(b)})
			b = b[8:]
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || uint64(len(b)-n) < l {
				return nil, errors.New("bad length")
			}
			b = b[n:]
			m[field] = append(m[field], Val{B: b[:l]})
			b = b[l:]
		case 5:
			if len(b) < 4 {
				return nil, errors.New("short fixed32")
			}
			m[field] = append(m[field], Val{U: uint64(binary.LittleEndian.Uint32(b))})
			b = b[4:]
		default:
			return nil, fmt.Errorf("unsupported wire type %d", key&7)
		}
	}
	return m, nil
}
