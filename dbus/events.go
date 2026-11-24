package dbus

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	godbus "github.com/godbus/dbus/v5"
)

// EventKind says which signal an [Event] came from.
type EventKind int

// Murmur emits the player and channel signals from its own handlers, so not
// every change produces one: ChannelCreated is only sent for channels clients
// create, never for [Server.AddChannel]. Changes made with
// [Server.SetPlayerState] and [Server.SetChannelState] do produce
// PlayerStateChanged and ChannelStateChanged.
const (
	ServerStarted EventKind = iota + 1
	ServerStopped
	PlayerConnected
	PlayerDisconnected
	PlayerStateChanged
	ChannelCreated
	ChannelRemoved
	ChannelStateChanged
)

var eventSignals = map[string]EventKind{
	metaInterface + ".started":               ServerStarted,
	metaInterface + ".stopped":               ServerStopped,
	serverInterface + ".playerConnected":     PlayerConnected,
	serverInterface + ".playerDisconnected":  PlayerDisconnected,
	serverInterface + ".playerStateChanged":  PlayerStateChanged,
	serverInterface + ".channelCreated":      ChannelCreated,
	serverInterface + ".channelRemoved":      ChannelRemoved,
	serverInterface + ".channelStateChanged": ChannelStateChanged,
}

func (k EventKind) String() string {
	switch k {
	case ServerStarted:
		return "ServerStarted"
	case ServerStopped:
		return "ServerStopped"
	case PlayerConnected:
		return "PlayerConnected"
	case PlayerDisconnected:
		return "PlayerDisconnected"
	case PlayerStateChanged:
		return "PlayerStateChanged"
	case ChannelCreated:
		return "ChannelCreated"
	case ChannelRemoved:
		return "ChannelRemoved"
	case ChannelStateChanged:
		return "ChannelStateChanged"
	}
	return "EventKind(" + strconv.Itoa(int(k)) + ")"
}

// Event is a signal Murmur emitted.
type Event struct {
	Kind EventKind
	// ServerID is the virtual server the event is about.
	ServerID int32
	// Player is set for the Player* kinds.
	Player PlayerState
	// Channel is set for the Channel* kinds. For ChannelRemoved it is the
	// channel as it was before removal.
	Channel Channel
}

// Events subscribes to everything the daemon emits: virtual servers starting
// and stopping, and the player and channel events of every running server.
//
// The channel is closed when ctx is done, when the client is closed, or when
// the bus connection is lost. Events are queued without limit and never
// dropped, so keep reading or cancel ctx.
func (c *Client) Events(ctx context.Context) (<-chan Event, error) {
	return c.subscribe(ctx, -1, [][]godbus.MatchOption{
		{godbus.WithMatchSender(c.service), godbus.WithMatchInterface(metaInterface), godbus.WithMatchObjectPath(metaPath)},
		{godbus.WithMatchSender(c.service), godbus.WithMatchInterface(serverInterface)},
	})
}

// Events subscribes to this virtual server's player and channel events, and
// to ServerStarted and ServerStopped for it. The subscription survives the
// server being stopped and started again. See [Client.Events] for when the
// channel is closed.
func (s *Server) Events(ctx context.Context) (<-chan Event, error) {
	return s.c.subscribe(ctx, s.id, [][]godbus.MatchOption{
		{godbus.WithMatchSender(s.c.service), godbus.WithMatchInterface(metaInterface), godbus.WithMatchObjectPath(metaPath)},
		{godbus.WithMatchSender(s.c.service), godbus.WithMatchInterface(serverInterface), godbus.WithMatchObjectPath(serverPath(s.id))},
	})
}

type subscription struct {
	c        *Client
	serverID int32 // -1 for every server
	rules    [][]godbus.MatchOption
	out      chan Event

	mu     sync.Mutex
	queue  []Event
	wake   chan struct{}
	stop   chan struct{}
	once   sync.Once
	cancel func()
}

func (c *Client) subscribe(ctx context.Context, serverID int32, rules [][]godbus.MatchOption) (<-chan Event, error) {
	s := &subscription{
		c:        c,
		serverID: serverID,
		rules:    rules,
		out:      make(chan Event),
		wake:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
	}
	s.cancel = func() { s.once.Do(s.shutdown) }

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errClosed
	}
	if c.sigCh == nil {
		c.sigCh = make(chan *godbus.Signal, 256)
		c.conn.Signal(c.sigCh)
		c.wg.Add(1)
		go c.dispatch(c.sigCh)
	}
	// Registered before the match rules are added, so nothing that arrives
	// once AddMatch returns can be missed.
	c.subs[s] = struct{}{}
	c.wg.Add(1)
	c.mu.Unlock()

	go s.run()

	for i, r := range rules {
		if err := c.conn.AddMatchSignalContext(ctx, r...); err != nil {
			s.rules = rules[:i]
			s.cancel()
			return nil, wrapError("AddMatch", err)
		}
	}
	stopAfter := context.AfterFunc(ctx, s.cancel)
	go func() {
		<-s.stop
		stopAfter()
	}()
	return s.out, nil
}

func (s *subscription) shutdown() {
	c := s.c
	c.mu.Lock()
	delete(c.subs, s)
	closed := c.closed
	c.mu.Unlock()
	close(s.stop)

	if closed && c.ownsConn {
		return // the connection is about to go, and its rules with it
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, r := range s.rules {
		_ = c.conn.RemoveMatchSignalContext(ctx, r...)
	}
}

func (s *subscription) push(ev Event) {
	if s.serverID >= 0 && ev.ServerID != s.serverID {
		return
	}
	s.mu.Lock()
	s.queue = append(s.queue, ev)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *subscription) run() {
	defer s.c.wg.Done()
	defer close(s.out)
	for {
		s.mu.Lock()
		if len(s.queue) == 0 {
			s.mu.Unlock()
			select {
			case <-s.wake:
				continue
			case <-s.stop:
				return
			}
		}
		ev := s.queue[0]
		s.queue[0] = Event{}
		s.queue = s.queue[1:]
		s.mu.Unlock()

		select {
		case s.out <- ev:
		case <-s.stop:
			return
		}
	}
}

// dispatch reads every signal the connection receives and hands Murmur's to
// the subscriptions. It never blocks on a subscriber.
func (c *Client) dispatch(ch <-chan *godbus.Signal) {
	defer c.wg.Done()
	for {
		select {
		case sig, ok := <-ch:
			if !ok {
				// The connection went away: end every subscription so
				// readers see a closed channel.
				c.mu.Lock()
				subs := make([]*subscription, 0, len(c.subs))
				for s := range c.subs {
					subs = append(subs, s)
				}
				c.mu.Unlock()
				for _, s := range subs {
					go s.cancel()
				}
				return
			}
			ev, ok := parseSignal(sig)
			if !ok {
				continue
			}
			c.mu.Lock()
			for s := range c.subs {
				s.push(ev)
			}
			c.mu.Unlock()
		case <-c.done:
			return
		}
	}
}

func parseSignal(sig *godbus.Signal) (Event, bool) {
	kind, ok := eventSignals[sig.Name]
	if !ok {
		return Event{}, false
	}
	ev := Event{Kind: kind}
	switch kind {
	case ServerStarted, ServerStopped:
		if sig.Path != metaPath {
			return Event{}, false
		}
		if err := godbus.Store(sig.Body, &ev.ServerID); err != nil {
			return Event{}, false
		}
		return ev, true
	}

	id, ok := parseServerPath(sig.Path)
	if !ok {
		return Event{}, false
	}
	ev.ServerID = id
	switch kind {
	case PlayerConnected, PlayerDisconnected, PlayerStateChanged:
		var w wirePlayerState
		if err := godbus.Store(sig.Body, &w); err != nil {
			return Event{}, false
		}
		ev.Player = w.state()
	default:
		var w wireChannel
		if err := godbus.Store(sig.Body, &w); err != nil {
			return Event{}, false
		}
		ev.Channel = w.channel()
	}
	return ev, true
}

func parseServerPath(p godbus.ObjectPath) (int32, bool) {
	s, ok := strings.CutPrefix(string(p), "/")
	if !ok || s == "" || s[0] == '+' || s[0] == '-' {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil || n < 0 {
		return 0, false
	}
	return int32(n), true
}
