package ice

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/VeryItalianPeople/go-mumble/internal/iceproto"
)

// EventKind says which callback an [Event] came from.
type EventKind int

const (
	ServerStarted EventKind = iota + 1
	ServerStopped
	UserConnected
	UserDisconnected
	UserStateChanged
	UserTextMessage
	ChannelCreated
	ChannelRemoved
	ChannelStateChanged
)

func (k EventKind) String() string {
	switch k {
	case ServerStarted:
		return "ServerStarted"
	case ServerStopped:
		return "ServerStopped"
	case UserConnected:
		return "UserConnected"
	case UserDisconnected:
		return "UserDisconnected"
	case UserStateChanged:
		return "UserStateChanged"
	case UserTextMessage:
		return "UserTextMessage"
	case ChannelCreated:
		return "ChannelCreated"
	case ChannelRemoved:
		return "ChannelRemoved"
	case ChannelStateChanged:
		return "ChannelStateChanged"
	}
	return "EventKind(" + strconv.Itoa(int(k)) + ")"
}

// Event is a callback from Murmur.
type Event struct {
	Kind EventKind
	// ServerID is the virtual server the event is about.
	ServerID int32
	// User is set for the User* kinds: the user's state after the change,
	// and for UserTextMessage the sender.
	User User
	// Message is set for UserTextMessage.
	Message TextMessage
	// Channel is set for the Channel* kinds.
	Channel Channel
}

// Handle is a callback, context action or authenticator registered with
// Murmur.
type Handle struct {
	c      *Client
	id     iceproto.Identity
	remove func(ctx context.Context) error
	stop   func()
	once   sync.Once
	err    error
}

// Close unregisters the callback from Murmur and stops serving it. It is
// safe to call more than once.
func (h *Handle) Close(ctx context.Context) error {
	h.once.Do(func() {
		if h.remove != nil {
			h.err = h.remove(ctx)
		}
		h.c.mu.Lock()
		delete(h.c.handles, h)
		a := h.c.adapter
		h.c.mu.Unlock()
		if a != nil {
			a.Remove(h.id)
		}
		if h.stop != nil {
			h.stop()
		}
	})
	return h.err
}

// adapter returns the callback adapter, starting it on first use, and the
// host to advertise in proxies.
func (c *Client) ensureAdapter(ctx context.Context) (*iceproto.Adapter, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, "", errClosed
	}
	if c.adapter != nil {
		return c.adapter, c.advHost, nil
	}
	listen, host := c.callbackListen, c.callbackHost
	if listen == "" || host == "" {
		la, err := c.rpc.LocalAddr(ctx)
		if err != nil {
			return nil, "", err
		}
		ip := la.String()
		if t, ok := la.(*net.TCPAddr); ok {
			ip = t.IP.String()
		}
		if listen == "" {
			listen = net.JoinHostPort(ip, "0")
		}
		if host == "" {
			host = ip
			if h, _, err := net.SplitHostPort(listen); err == nil && h != "" {
				if a := net.ParseIP(h); a == nil || !a.IsUnspecified() {
					host = h
				}
			}
		}
	}
	a, err := iceproto.Listen(listen)
	if err != nil {
		return nil, "", fmt.Errorf("murmur: listening for callbacks on %s: %w", listen, err)
	}
	if secret := c.callbackSecret; secret != "" {
		a.Check = func(cur *iceproto.Current) error {
			if cur.Context["secret"] != secret {
				return errors.New("wrong or missing secret")
			}
			return nil
		}
	}
	c.adapter, c.advHost = a, host
	return a, host, nil
}

// export registers a servant and returns a handle and its proxy. The
// caller fills in the handle's remove and stop functions.
func (c *Client) export(ctx context.Context, s iceproto.Servant) (*Handle, *iceproto.Proxy, error) {
	a, host, err := c.ensureAdapter(ctx)
	if err != nil {
		return nil, nil, err
	}
	var b [8]byte
	rand.Read(b[:])
	c.mu.Lock()
	c.identity++
	id := iceproto.Identity{Name: fmt.Sprintf("gomumble-%d-%s", c.identity, hex.EncodeToString(b[:]))}
	h := &Handle{c: c, id: id}
	c.handles[h] = struct{}{}
	c.mu.Unlock()
	a.Add(id, s)
	return h, a.Proxy(id, host), nil
}

// queue feeds a channel from an unbounded buffer, so a dispatch never blocks
// on a slow reader and nothing is dropped.
type queue[T any] struct {
	mu    sync.Mutex
	items []T
	wake  chan struct{}
	stop  chan struct{}
	once  sync.Once
	out   chan T
}

func newQueue[T any]() *queue[T] {
	q := &queue[T]{wake: make(chan struct{}, 1), stop: make(chan struct{}), out: make(chan T)}
	go q.run()
	return q
}

func (q *queue[T]) push(v T) {
	q.mu.Lock()
	q.items = append(q.items, v)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *queue[T]) close() { q.once.Do(func() { close(q.stop) }) }

func (q *queue[T]) run() {
	defer close(q.out)
	for {
		q.mu.Lock()
		if len(q.items) == 0 {
			q.mu.Unlock()
			select {
			case <-q.wake:
				continue
			case <-q.stop:
				return
			}
		}
		v := q.items[0]
		var zero T
		q.items[0] = zero
		q.items = q.items[1:]
		q.mu.Unlock()
		select {
		case q.out <- v:
		case <-q.stop:
			return
		}
	}
}

// subscribe exports s, registers it with register, and ties its lifetime
// to ctx.
func (c *Client) subscribe(ctx context.Context, s iceproto.Servant, q *queue[Event],
	register func(context.Context, *iceproto.Proxy) error, unregister func(context.Context, *iceproto.Proxy) error) (<-chan Event, error) {
	h, proxy, err := c.export(ctx, s)
	if err != nil {
		q.close()
		return nil, err
	}
	h.stop = q.close
	h.remove = func(ctx context.Context) error { return unregister(ctx, proxy) }
	if err := register(ctx, proxy); err != nil {
		h.remove = nil
		h.Close(ctx)
		return nil, err
	}
	stopAfter := context.AfterFunc(ctx, func() {
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		h.Close(rctx)
	})
	go func() {
		<-q.stop
		stopAfter()
	}()
	return q.out, nil
}

const (
	serverCallbackTypeID  = "::MumbleServer::ServerCallback"
	metaCallbackTypeID    = "::MumbleServer::MetaCallback"
	contextCallbackTypeID = "::MumbleServer::ServerContextCallback"
)

type serverCallback struct {
	serverID int32
	q        *queue[Event]
}

func (serverCallback) TypeIDs() []string { return []string{serverCallbackTypeID} }

func (s serverCallback) Dispatch(_ context.Context, cur *iceproto.Current, in *iceproto.Decoder, _ *iceproto.Encoder) error {
	ev := Event{ServerID: s.serverID}
	switch cur.Operation {
	case "userConnected":
		ev.Kind, ev.User = UserConnected, readUser(in)
	case "userDisconnected":
		ev.Kind, ev.User = UserDisconnected, readUser(in)
	case "userStateChanged":
		ev.Kind, ev.User = UserStateChanged, readUser(in)
	case "userTextMessage":
		ev.Kind, ev.User = UserTextMessage, readUser(in)
		ev.Message = readTextMessage(in)
	case "channelCreated":
		ev.Kind, ev.Channel = ChannelCreated, readChannel(in)
	case "channelRemoved":
		ev.Kind, ev.Channel = ChannelRemoved, readChannel(in)
	case "channelStateChanged":
		ev.Kind, ev.Channel = ChannelStateChanged, readChannel(in)
	default:
		return iceproto.ErrOperationNotExist
	}
	if err := in.Err(); err != nil {
		return err
	}
	s.q.push(ev)
	return nil
}

// Events registers a callback with the server and delivers what it reports:
// users connecting, disconnecting, changing state and sending text
// messages, and channels being created, removed and changed. It needs the
// write secret.
//
// The channel is closed when ctx is done or the client is closed; the
// callback is removed from Murmur then. Events are queued without limit and
// never dropped, so keep reading. Murmur only delivers to running servers
// and forgets callbacks when the server stops.
func (s *Server) Events(ctx context.Context) (<-chan Event, error) {
	q := newQueue[Event]()
	return s.c.subscribe(ctx, serverCallback{serverID: s.id, q: q}, q,
		func(ctx context.Context, p *iceproto.Proxy) error {
			return s.call(ctx, "addCallback", normal, func(e *iceproto.Encoder) { e.Proxy(p) }, nil)
		},
		func(ctx context.Context, p *iceproto.Proxy) error {
			return s.call(ctx, "removeCallback", normal, func(e *iceproto.Encoder) { e.Proxy(p) }, nil)
		})
}

type metaCallback struct {
	q *queue[Event]
}

func (metaCallback) TypeIDs() []string { return []string{metaCallbackTypeID} }

func (m metaCallback) Dispatch(_ context.Context, cur *iceproto.Current, in *iceproto.Decoder, _ *iceproto.Encoder) error {
	var kind EventKind
	switch cur.Operation {
	case "started":
		kind = ServerStarted
	case "stopped":
		kind = ServerStopped
	default:
		return iceproto.ErrOperationNotExist
	}
	p := in.Proxy()
	if err := in.Err(); err != nil {
		return err
	}
	if p == nil {
		return errors.New("null server proxy")
	}
	id, err := strconv.ParseInt(p.Identity.Name, 10, 32)
	if err != nil {
		return fmt.Errorf("server proxy %s: %w", p.Identity, err)
	}
	m.q.push(Event{Kind: kind, ServerID: int32(id)})
	return nil
}

// Events registers a Meta callback and delivers ServerStarted and
// ServerStopped for every virtual server. Use [Server.Events] for what
// happens on a server. See [Server.Events] for when the channel is closed.
func (c *Client) Events(ctx context.Context) (<-chan Event, error) {
	q := newQueue[Event]()
	return c.subscribe(ctx, metaCallback{q: q}, q,
		func(ctx context.Context, p *iceproto.Proxy) error {
			return c.metaCall(ctx, "addCallback", normal, func(e *iceproto.Encoder) { e.Proxy(p) }, nil)
		},
		func(ctx context.Context, p *iceproto.Proxy) error {
			return c.metaCall(ctx, "removeCallback", normal, func(e *iceproto.Encoder) { e.Proxy(p) }, nil)
		})
}

// ContextAction is a click on a context menu entry added with
// [Server.AddContextCallback].
type ContextAction struct {
	Action string
	// User is the user who clicked.
	User User
	// Session is the user the action was invoked on, for ContextUser.
	Session int32
	// Channel is the channel the action was invoked on, for
	// ContextChannel.
	Channel int32
}

type contextCallback struct {
	q *queue[ContextAction]
}

func (contextCallback) TypeIDs() []string { return []string{contextCallbackTypeID} }

func (c contextCallback) Dispatch(_ context.Context, cur *iceproto.Current, in *iceproto.Decoder, _ *iceproto.Encoder) error {
	if cur.Operation != "contextAction" {
		return iceproto.ErrOperationNotExist
	}
	a := ContextAction{Action: in.String(), User: readUser(in), Session: in.Int(), Channel: in.Int()}
	if err := in.Err(); err != nil {
		return err
	}
	c.q.push(a)
	return nil
}

// AddContextCallback adds an entry to one session's context menu, shown with
// text where says (any combination of ContextServer, ContextChannel and
// ContextUser), and calls fn each time the user clicks it. Calls to fn are
// made one at a time, in order.
//
// Murmur keeps the entry until the session disconnects or Close is called on
// the handle; adding the same action for the same session again replaces
// it. It needs the write secret.
func (s *Server) AddContextCallback(ctx context.Context, session int32, action, text string, where ContextFlag, fn func(ContextAction)) (*Handle, error) {
	q := newQueue[ContextAction]()
	h, proxy, err := s.c.export(ctx, contextCallback{q: q})
	if err != nil {
		q.close()
		return nil, err
	}
	h.stop = q.close
	h.remove = func(ctx context.Context) error {
		return s.call(ctx, "removeContextCallback", normal, func(e *iceproto.Encoder) { e.Proxy(proxy) }, nil)
	}
	err = s.call(ctx, "addContextCallback", normal, func(e *iceproto.Encoder) {
		e.Int(session)
		e.String(action)
		e.String(text)
		e.Proxy(proxy)
		e.Int(int32(where))
	}, nil)
	if err != nil {
		h.remove = nil
		h.Close(ctx)
		return nil, err
	}
	go func() {
		for a := range q.out {
			fn(a)
		}
	}()
	return h, nil
}
