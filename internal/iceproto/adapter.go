package iceproto

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"slices"
	"strconv"
	"sync"
	"time"
)

// Current describes the request a servant is dispatching.
type Current struct {
	Identity  Identity
	Operation string
	Mode      byte
	Context   map[string]string
}

// Servant is an Ice object implemented in Go.
type Servant interface {
	// TypeIDs lists the Slice type IDs the object implements, for
	// ice_isA, ice_id and ice_ids. The first is the most derived one.
	// "::Ice::Object" is implied.
	TypeIDs() []string
	// Dispatch runs one operation, reading in-parameters from in and
	// writing out-parameters and the return value to out. It returns
	// ErrOperationNotExist for an operation it doesn't have; any other
	// error goes back to the caller as an unknown exception.
	Dispatch(ctx context.Context, cur *Current, in *Decoder, out *Encoder) error
}

// ErrOperationNotExist makes the adapter answer OperationNotExist.
var ErrOperationNotExist = errors.New("iceproto: operation does not exist")

// Adapter is a minimal Ice object adapter: it accepts TCP connections and
// dispatches requests to servants by identity. It has no locators, facets
// or bidirectional connections.
type Adapter struct {
	// Check, if set, vets every request before it is dispatched; an error
	// is sent back as an unknown exception.
	Check func(*Current) error

	ln     net.Listener
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	servants map[Identity]Servant
	conns    map[net.Conn]struct{}
	closed   bool
	wg       sync.WaitGroup
}

// Listen starts an adapter on a TCP address such as "127.0.0.1:0".
func Listen(addr string) (*Adapter, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &Adapter{
		ln:       ln,
		ctx:      ctx,
		cancel:   cancel,
		servants: make(map[Identity]Servant),
		conns:    make(map[net.Conn]struct{}),
	}
	a.wg.Add(1)
	go a.serve()
	return a, nil
}

// Addr returns the address the adapter listens on.
func (a *Adapter) Addr() net.Addr { return a.ln.Addr() }

// Add registers a servant under id, replacing any servant already there.
func (a *Adapter) Add(id Identity, s Servant) {
	a.mu.Lock()
	a.servants[id] = s
	a.mu.Unlock()
}

// Remove unregisters a servant. Later requests for it get ObjectNotExist.
func (a *Adapter) Remove(id Identity) {
	a.mu.Lock()
	delete(a.servants, id)
	a.mu.Unlock()
}

// Proxy returns a proxy for id that reaches this adapter at host, which must
// be an address the caller can connect to.
func (a *Adapter) Proxy(id Identity, host string) *Proxy {
	port := 0
	if t, ok := a.ln.Addr().(*net.TCPAddr); ok {
		port = t.Port
	} else if _, p, err := net.SplitHostPort(a.ln.Addr().String()); err == nil {
		port, _ = strconv.Atoi(p)
	}
	return &Proxy{
		Identity:  id,
		Endpoints: []Endpoint{{Host: host, Port: int32(port), TimeoutMs: 60000}},
	}
}

// Close stops listening, closes every connection and waits for the
// dispatches in progress to return.
func (a *Adapter) Close() error {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	conns := make([]net.Conn, 0, len(a.conns))
	for c := range a.conns {
		conns = append(conns, c)
	}
	a.mu.Unlock()

	a.cancel()
	err := a.ln.Close()
	for _, c := range conns {
		c.SetWriteDeadline(time.Now().Add(time.Second))
		c.Write(header(msgClose, headerSize))
		c.Close()
	}
	a.wg.Wait()
	return err
}

func (a *Adapter) serve() {
	defer a.wg.Done()
	for {
		nc, err := a.ln.Accept()
		if err != nil {
			return
		}
		a.mu.Lock()
		if a.closed {
			a.mu.Unlock()
			nc.Close()
			return
		}
		a.conns[nc] = struct{}{}
		a.wg.Add(1)
		a.mu.Unlock()
		go a.handle(nc)
	}
}

type serverConn struct {
	nc  net.Conn
	wmu sync.Mutex
}

func (sc *serverConn) write(b []byte) {
	sc.wmu.Lock()
	defer sc.wmu.Unlock()
	sc.nc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := sc.nc.Write(b); err != nil {
		sc.nc.Close()
	}
}

func (a *Adapter) handle(nc net.Conn) {
	defer a.wg.Done()
	defer func() {
		a.mu.Lock()
		delete(a.conns, nc)
		a.mu.Unlock()
		nc.Close()
	}()
	sc := &serverConn{nc: nc}
	sc.write(header(msgValidate, headerSize))

	var dispatches sync.WaitGroup
	defer dispatches.Wait()
	for {
		typ, body, err := readMessage(nc)
		if err != nil {
			return
		}
		switch typ {
		case msgRequest:
			// Oneway requests (ID 0) are what Murmur sends its events
			// as; dispatch them in order. Twoway ones may take a
			// while (an authenticator), so they don't hold up the
			// connection.
			if len(body) >= 4 && binary.LittleEndian.Uint32(body) == 0 {
				a.dispatch(sc, body)
				break
			}
			dispatches.Add(1)
			go func() {
				defer dispatches.Done()
				a.dispatch(sc, body)
			}()
		case msgClose:
			return
		case msgValidate, msgBatchRequest, msgReply:
			// heartbeat; batches and replies have no business here
		}
	}
}

func (a *Adapter) dispatch(sc *serverConn, body []byte) {
	d := NewDecoder(body)
	reqID := d.Int()
	cur := &Current{Identity: d.Identity()}
	facets := d.Strings()
	cur.Operation = d.String()
	cur.Mode = d.Byte()
	cur.Context = d.Context()
	in := d.Encaps()
	if d.Err() != nil {
		return // garbled: nothing sensible to answer
	}

	var out Encoder
	out.buf = header(msgReply, headerSize)
	out.Int(reqID)

	a.mu.Lock()
	s := a.servants[cur.Identity]
	a.mu.Unlock()

	notExist := func(status byte) {
		out.Byte(status)
		out.Identity(cur.Identity)
		out.Strings(facets)
		out.String(cur.Operation)
	}
	switch {
	case s == nil:
		notExist(replyObjectNotExist)
	case len(facets) > 0 && facets[0] != "":
		notExist(replyFacetNotExist)
	default:
		var check error
		if a.Check != nil {
			check = a.Check(cur)
		}
		if check != nil {
			out.Byte(replyUnknownException)
			out.String(check.Error())
			break
		}
		var params Encoder
		err := dispatchBuiltin(s, cur, in, &params)
		if errors.Is(err, ErrOperationNotExist) {
			err = s.Dispatch(a.ctx, cur, in, &params)
		}
		switch {
		case errors.Is(err, ErrOperationNotExist):
			notExist(replyOperationNotExist)
		case err != nil:
			out.Byte(replyUnknownException)
			out.String(err.Error())
		default:
			out.Byte(replyOK)
			pos := len(out.buf)
			out.Int(0)
			v := in.Encoding()
			out.Byte(v.Major)
			out.Byte(v.Minor)
			out.buf = append(out.buf, params.buf...)
			out.EndEncaps(pos)
		}
	}
	if reqID == 0 {
		return // oneway
	}
	binary.LittleEndian.PutUint32(out.buf[10:], uint32(len(out.buf)))
	sc.write(out.buf)
}

const objectTypeID = "::Ice::Object"

// dispatchBuiltin answers the operations every Ice object has.
func dispatchBuiltin(s Servant, cur *Current, in *Decoder, out *Encoder) error {
	ids := append(slices.Clone(s.TypeIDs()), objectTypeID)
	switch cur.Operation {
	case "ice_ping":
		return nil
	case "ice_isA":
		id := in.String()
		if in.Err() != nil {
			return in.Err()
		}
		out.Bool(slices.Contains(ids, id))
		return nil
	case "ice_id":
		out.String(ids[0])
		return nil
	case "ice_ids":
		slices.Sort(ids)
		out.Strings(ids)
		return nil
	}
	return ErrOperationNotExist
}
