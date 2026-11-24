package iceproto

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestEncodingRoundTrip(t *testing.T) {
	var e Encoder
	e.Bool(true)
	e.Byte(7)
	e.Short(-2)
	e.Int(-3)
	e.Long(1 << 40)
	e.Float(1.5)
	e.Double(-2.25)
	e.Size(300) // needs the long form
	e.String("héllo")
	e.Strings([]string{"a", ""})
	e.Ints([]int32{1, -1})
	e.ByteSeq([]byte{1, 2, 3})
	e.Enum(5)
	e.Identity(Identity{Name: "Meta"})
	e.Context(map[string]string{"secret": "s"})

	d := NewDecoder(e.Bytes())
	if !d.Bool() || d.Byte() != 7 || d.Short() != -2 || d.Int() != -3 || d.Long() != 1<<40 ||
		d.Float() != 1.5 || d.Double() != -2.25 || d.Size() != 300 || d.String() != "héllo" {
		t.Fatal("scalar mismatch")
	}
	if s := d.Strings(); len(s) != 2 || s[0] != "a" || s[1] != "" {
		t.Errorf("Strings = %q", s)
	}
	if v := d.Ints(); len(v) != 2 || v[1] != -1 {
		t.Errorf("Ints = %v", v)
	}
	if b := d.ByteSeq(); !bytes.Equal(b, []byte{1, 2, 3}) {
		t.Errorf("ByteSeq = %v", b)
	}
	if d.Enum() != 5 || d.Identity() != (Identity{Name: "Meta"}) || d.Context()["secret"] != "s" {
		t.Error("enum, identity or context mismatch")
	}
	if d.Err() != nil || d.Remaining() != 0 {
		t.Errorf("err %v, %d bytes left", d.Err(), d.Remaining())
	}
}

func TestDecoderErrorsAreSticky(t *testing.T) {
	d := NewDecoder([]byte{5, 'a'}) // a string of 5 bytes with 1 present
	if d.String() != "" || !errors.Is(d.Err(), ErrShort) {
		t.Fatalf("err = %v", d.Err())
	}
	if d.Int() != 0 || d.Strings() == nil || !errors.Is(d.Err(), ErrShort) {
		t.Error("reads after an error must return zero values")
	}
	// A sequence claiming more elements than the data can hold.
	d = NewDecoder([]byte{200})
	if v := d.Ints(); len(v) != 0 || d.Err() == nil {
		t.Errorf("Ints = %v, err %v", v, d.Err())
	}
}

func TestProxyRoundTrip(t *testing.T) {
	p := &Proxy{
		Identity:  Identity{Name: "cb", Category: "x"},
		Endpoints: []Endpoint{{Host: "10.0.0.1", Port: 6502, TimeoutMs: 60000}},
	}
	var e Encoder
	e.Proxy(p)
	e.Proxy(nil)
	d := NewDecoder(e.Bytes())
	got := d.Proxy()
	if got == nil || got.Identity != p.Identity || len(got.Endpoints) != 1 || got.Endpoints[0] != p.Endpoints[0] {
		t.Errorf("Proxy = %+v", got)
	}
	if d.Proxy() != nil {
		t.Error("null proxy decoded as non-nil")
	}
	if d.Err() != nil || d.Remaining() != 0 {
		t.Errorf("err %v, %d bytes left", d.Err(), d.Remaining())
	}
}

func TestEncaps(t *testing.T) {
	var e Encoder
	pos := e.StartEncaps()
	e.Int(42)
	e.EndEncaps(pos)
	d := NewDecoder(e.Bytes()).Encaps()
	if d.Encoding() != Encoding11 || d.Int() != 42 || d.Err() != nil {
		t.Errorf("encaps: %v", d.Err())
	}
	bad := NewDecoder([]byte{6, 0, 0, 0, 2, 0}).Encaps()
	if bad.Err() == nil {
		t.Error("encoding 2.0 accepted")
	}
}

// The bytes Ice 3.7 writes for a user exception without members in the
// compact format: slice flags (type ID string | last slice), type ID.
func TestExceptionTypeID(t *testing.T) {
	var e Encoder
	e.Byte(flagTypeIDString | flagLastSlice)
	e.String("::MumbleServer::InvalidSessionException")
	if got := NewDecoder(e.Bytes()).ExceptionTypeID(); got != "::MumbleServer::InvalidSessionException" {
		t.Errorf("ExceptionTypeID = %q", got)
	}
}

// Two nested class instances in the compact format: the second refers to
// the type ID the first one introduced.
func TestInstance(t *testing.T) {
	var e Encoder
	e.Size(1) // inline instance
	e.Byte(flagTypeIDString | flagLastSlice)
	e.String("::T")
	e.Int(1)
	e.Size(1) // one child
	e.Size(1)
	e.Byte(flagTypeIDIndex | flagLastSlice | flagOptionalMembers)
	e.Size(1) // type ID #1
	e.Int(2)
	e.Size(0)    // no children
	e.Byte(0xFF) // end of optional members
	e.Size(0)    // a null instance

	type node struct {
		v        int32
		children []*node
	}
	d := NewDecoder(e.Bytes())
	var read func() *node
	read = func() *node {
		var n *node
		d.Instance(func(typeID string) {
			if typeID != "::T" {
				t.Errorf("typeID = %q", typeID)
			}
			n = &node{v: d.Int()}
			for i, c := 0, d.Size(); i < c; i++ {
				n.children = append(n.children, read())
			}
		})
		return n
	}
	root := read()
	if root == nil || root.v != 1 || len(root.children) != 1 || root.children[0].v != 2 {
		t.Fatalf("tree = %+v, err %v", root, d.Err())
	}
	if read() != nil || d.Err() != nil || d.Remaining() != 0 {
		t.Errorf("null instance: err %v, %d left", d.Err(), d.Remaining())
	}
}

type echoServant struct{}

func (echoServant) TypeIDs() []string { return []string{"::Test::Echo"} }

func (echoServant) Dispatch(_ context.Context, cur *Current, in *Decoder, out *Encoder) error {
	switch cur.Operation {
	case "echo":
		out.String(in.String() + "|" + cur.Context["k"])
		return in.Err()
	case "fail":
		return errors.New("boom")
	}
	return ErrOperationNotExist
}

func TestClientAgainstAdapter(t *testing.T) {
	a, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	id := Identity{Name: "echo"}
	a.Add(id, echoServant{})

	c := &Client{Addr: a.Addr().String()}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	call := func(op string, write func(*Encoder)) (*Decoder, error) {
		var e Encoder
		if write != nil {
			write(&e)
		}
		return c.Invoke(ctx, Request{Identity: id, Operation: op, Context: map[string]string{"k": "v"}, Params: e.Bytes()})
	}

	d, err := call("echo", func(e *Encoder) { e.String("hi") })
	if err != nil || d.String() != "hi|v" {
		t.Errorf("echo: %v", err)
	}
	d, err = call("ice_isA", func(e *Encoder) { e.String("::Test::Echo") })
	if err != nil || !d.Bool() {
		t.Errorf("ice_isA: %v", err)
	}
	d, err = call("ice_ids", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ids := d.Strings(); len(ids) != 2 || ids[0] != "::Ice::Object" || ids[1] != "::Test::Echo" {
		t.Errorf("ice_ids = %q", ids)
	}
	if _, err := call("ice_ping", nil); err != nil {
		t.Errorf("ice_ping: %v", err)
	}

	var de *DispatchError
	if _, err := call("nope", nil); !errors.As(err, &de) || de.Status != OperationNotExist {
		t.Errorf("unknown operation: %v", err)
	}
	if _, err := call("fail", nil); !errors.As(err, &de) || de.Status != Unknown || de.Text != "boom" {
		t.Errorf("failing operation: %v", err)
	}
	_, err = c.Invoke(ctx, Request{Identity: Identity{Name: "missing"}, Operation: "x"})
	if !errors.As(err, &de) || de.Status != ObjectNotExist || de.Identity.Name != "missing" {
		t.Errorf("missing object: %v", err)
	}

	a.Check = func(cur *Current) error {
		if cur.Context["k"] != "right" {
			return errors.New("nope")
		}
		return nil
	}
	if _, err := call("echo", func(e *Encoder) { e.String("hi") }); !errors.As(err, &de) || de.Status != Unknown {
		t.Errorf("Check didn't refuse: %v", err)
	}
}

// A server that closes the connection gracefully instead of answering must
// get the request again on a new connection.
func TestRetryAfterGracefulClose(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	conns := 0
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			conns++
			first := conns == 1
			go func() {
				defer nc.Close()
				nc.Write(header(msgValidate, headerSize))
				for {
					typ, body, err := readMessage(nc)
					if err != nil {
						return
					}
					if typ != msgRequest {
						continue
					}
					if first {
						nc.Write(header(msgClose, headerSize))
						return
					}
					d := NewDecoder(body)
					var out Encoder
					out.buf = header(msgReply, headerSize)
					out.Int(d.Int())
					out.Byte(replyOK)
					pos := out.StartEncaps()
					out.String("second")
					out.EndEncaps(pos)
					copy(out.buf[10:14], []byte{byte(len(out.buf)), byte(len(out.buf) >> 8), 0, 0})
					nc.Write(out.buf)
				}
			}()
		}
	}()

	c := &Client{Addr: ln.Addr().String()}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d, err := c.Invoke(ctx, Request{Identity: Identity{Name: "x"}, Operation: "op"})
	if err != nil {
		t.Fatal(err)
	}
	if s := d.String(); s != "second" {
		t.Errorf("reply = %q", s)
	}
}

func TestClientClose(t *testing.T) {
	a, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	c := &Client{Addr: a.Addr().String()}
	ctx := context.Background()
	if _, err := c.LocalAddr(ctx); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if _, err := c.Invoke(ctx, Request{Operation: "x"}); !errors.Is(err, ErrClientClosed) {
		t.Errorf("Invoke after Close: %v", err)
	}
}
