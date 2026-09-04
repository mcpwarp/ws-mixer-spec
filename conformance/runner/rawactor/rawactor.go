// Package rawactor is the conformance runner's raw wire actor
// (docs/CONFORMANCE.md section 2): a WebSocket peer, in either role, that
// speaks the ws-mixer.v1 subprotocol at the transport level but has zero
// mux-level behaviour of its own. It never auto-pongs, never credits,
// never validates a frame it receives -- it just records what it observes
// and sends exactly the bytes a fixture step names. It uses this repo's own
// independent codec (wire package), not go/wsmixer's.
package rawactor

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/mcpwarp/ws-mixer-spec/conformance/runner/wire"
)

const subprotocol = "ws-mixer.v1"

// Observation is one entry in the actor's append-only log: a decoded frame
// plus the time it was observed, or (once) the WebSocket close.
type Observation struct {
	At    time.Time
	Frame *wire.Frame // nil for a Close or DecodeFailed observation
	Close *CloseInfo
	// DecodeFailed carries the raw bytes of a WebSocket message wire.Decode
	// rejected (too short to have a header) -- under --strict this fails the
	// step with a hex dump rather than being silently swallowed.
	DecodeFailed []byte
}

// CloseInfo captures the WebSocket-level close code/reason.
type CloseInfo struct {
	Code   int
	Reason string
}

// Actor is one raw WebSocket peer, server or client role, for one
// connection.
type Actor struct {
	conn *websocket.Conn
	ctx  context.Context

	mu      sync.Mutex
	log     []Observation
	closed  bool
	closeAt *CloseInfo
	waiters []chan struct{}
	// consumed: see the identical field/comment on adapter.Adapter.
	consumed map[int]bool
	// strict: see SetStrict.
	strict bool

	httpServer *http.Server
	listener   net.Listener
	// connReady is closed once Serve's background accept has produced a
	// connection (or given up); nil for a Dial-built Actor, which already
	// has one. See awaitConn.
	connReady chan struct{}
}

// Serve accepts exactly one WebSocket upgrade at /v1/tunnel on addr (a
// docs/CONFORMANCE.md section 2 "role: server" raw actor -- used to run a
// role:"client" fixture against the real client-under-test). It does not
// check the Authorization header or hello contents: any hostility in the
// corpus is bytes on the mux channel, not the outer HTTP handshake. Returns
// once a connection is accepted; call Close when done.
func Serve(ctx context.Context, addr string) (*Actor, string, error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", fmt.Errorf("rawactor: listen: %w", err)
	}

	a := &Actor{ctx: ctx, listener: ln, connReady: make(chan struct{}), strict: true}
	connCh := make(chan *websocket.Conn, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/tunnel", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols:    []string{subprotocol},
			CompressionMode: websocket.CompressionDisabled,
		})
		if err != nil {
			select {
			case errCh <- err:
			default:
			}
			return
		}
		select {
		case connCh <- c:
		default:
			_ = c.Close(websocket.StatusPolicyViolation, "rawactor: one connection per Actor")
		}
	})
	a.httpServer = &http.Server{Handler: mux}
	go func() { _ = a.httpServer.Serve(ln) }()

	// Accept happens in the background: Serve must return the URL right
	// away so the caller can hand it to the adapter-under-test's `connect`
	// command -- the very thing that causes a connection to ever arrive
	// here. Send/SendFrame/SendControl/Expect all wait on connReady.
	go func() {
		select {
		case c := <-connCh:
			a.conn = c
			close(a.connReady)
			a.readLoop()
		case err := <-errCh:
			a.mu.Lock()
			a.closed = true
			a.log = append(a.log, Observation{At: time.Now(), Close: &CloseInfo{Reason: err.Error()}})
			waiters := a.waiters
			a.waiters = nil
			a.mu.Unlock()
			close(a.connReady)
			for _, w := range waiters {
				close(w)
			}
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
		}
	}()

	return a, "ws://" + ln.Addr().String() + "/v1/tunnel", nil
}

// awaitConn blocks until Serve's background accept has produced a
// connection (or failed), so Send/Expect never race the accept.
func (a *Actor) awaitConn() error {
	if a.connReady == nil {
		return nil // Dial-built actors have a connection from construction
	}
	select {
	case <-a.connReady:
		if a.conn == nil {
			return fmt.Errorf("rawactor: accept failed before a connection arrived")
		}
		return nil
	case <-a.ctx.Done():
		return a.ctx.Err()
	case <-time.After(35 * time.Second):
		return fmt.Errorf("rawactor: timed out waiting for a connection")
	}
}

// Dial connects to url as a raw client (a docs/CONFORMANCE.md section 2
// "role: client" raw actor -- used to run a role:"server" fixture against
// the real server-under-test). header carries whatever Authorization value
// the caller wants sent (fixture driving controls this; see
// docs/CONFORMANCE.md open question territory around auth_failure).
func Dial(ctx context.Context, url string, header http.Header) (*Actor, error) {
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		Subprotocols:    []string{subprotocol},
		HTTPHeader:      header,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, fmt.Errorf("rawactor: dial: %w", err)
	}
	a := &Actor{ctx: ctx, conn: c, strict: true}
	go a.readLoop()
	return a, nil
}

func (a *Actor) readLoop() {
	for {
		_, data, err := a.conn.Read(a.ctx)
		if err != nil {
			code, reason := closeCodeOf(err)
			a.mu.Lock()
			a.closed = true
			a.closeAt = &CloseInfo{Code: code, Reason: reason}
			a.log = append(a.log, Observation{At: time.Now(), Close: a.closeAt})
			waiters := a.waiters
			a.waiters = nil
			a.mu.Unlock()
			for _, w := range waiters {
				close(w)
			}
			return
		}
		f, ok := wire.Decode(data)
		a.mu.Lock()
		if ok {
			a.log = append(a.log, Observation{At: time.Now(), Frame: f})
		} else {
			a.log = append(a.log, Observation{At: time.Now(), DecodeFailed: append([]byte(nil), data...)})
		}
		waiters := a.waiters
		a.waiters = nil
		a.mu.Unlock()
		for _, w := range waiters {
			close(w)
		}
	}
}

func closeCodeOf(err error) (int, string) {
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		return int(ce.Code), ce.Reason
	}
	return 0, err.Error()
}

// Send writes raw bytes as one binary WebSocket message -- exactly the
// bytes a fixture step names, no framing logic applied.
func (a *Actor) Send(b []byte) error {
	if err := a.awaitConn(); err != nil {
		return err
	}
	return a.conn.Write(a.ctx, websocket.MessageBinary, b)
}

// SendFrame is Send(wire.Encode(f)).
func (a *Actor) SendFrame(f *wire.Frame) error { return a.Send(wire.Encode(f)) }

// SendControl marshals msg to JSON and sends it as a stream-0 DATA frame --
// the standard way to inject a hello/welcome/ping/pong/drain/error/app
// control message.
func (a *Actor) SendControl(msg any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return a.Send(wire.Data(0, b))
}

// Log returns a snapshot of every observation so far, in order.
func (a *Actor) Log() []Observation {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Observation, len(a.log))
	copy(out, a.log)
	return out
}

// Closed reports the WebSocket close this actor observed, if any.
func (a *Actor) Closed() (*CloseInfo, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closeAt == nil {
		return nil, false
	}
	c := *a.closeAt
	return &c, true
}

// CloseObservation returns the full Observation for the WebSocket close this
// actor logged, if any -- unlike Closed, this carries the timestamp, needed
// to check the "error{} precedes the close" invariant (docs/CONFORMANCE.md
// section 4).
func (a *Actor) CloseObservation() (Observation, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := len(a.log) - 1; i >= 0; i-- {
		if a.log[i].Close != nil {
			return a.log[i], true
		}
	}
	return Observation{}, false
}

// SetStrict toggles strict mode (default true from Serve/Dial). Under
// strict mode, Expect fails immediately -- rather than skipping past it --
// on any observed frame the caller's predicate doesn't match, unless it is
// one of the frames a real SDK-under-test may legitimately emit without a
// preceding command (see isAllowedAutonomous), or an undecodable message
// (reported as a FAIL with a hex dump, never silently dropped).
func (a *Actor) SetStrict(strict bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.strict = strict
}

// isAllowedAutonomous enumerates the wire-level frames a real SDK-under-test
// may legitimately emit without the current step expecting them, per
// docs/CONFORMANCE.md section 2/OVERVIEW.md section 2.10:
//   - ping/pong control messages (stream-0 DATA), from the SDK's own
//     keepalive loop -- may arrive in any state a connection is open.
//   - WINDOW, from the SDK crediting back consumed receive-buffer bytes as a
//     side effect of its own flow-control bookkeeping (docs/CONFORMANCE.md
//     section 3.3: "No command issues WINDOW directly ... always a side
//     effect") -- may arrive in any state a stream can still receive DATA
//     (open, half_closed_local; never after RESET/full close, but this
//     runner does not track per-stream state at the rawactor layer, so that
//     narrower gate is left to whatever `expect stream_state` step follows).
//
// Anything else observed without the current step expecting it is a
// wire-level conformance failure under --strict.
func isAllowedAutonomous(f *wire.Frame) bool {
	if f == nil {
		return false
	}
	if f.Type == wire.TypeWindow {
		return true
	}
	if f.Type == wire.TypeData && f.StreamID == 0 {
		var m map[string]any
		if json.Unmarshal(f.Payload, &m) == nil {
			t, _ := m["t"].(string)
			return t == "ping" || t == "pong"
		}
	}
	return false
}

// Expect waits (up to timeout) for a new observation matching pred. Under
// strict mode (the default, see SetStrict), any unmatched observation
// encountered while scanning fails the wait immediately -- a decode failure
// with a hex dump, an unmatched frame with its decoded form, unless it is an
// allowed autonomous frame (isAllowedAutonomous), which is silently
// consumed and scanning continues. Non-strict mode instead tolerates (skips
// over) any unmatched observation, per conformance/README.md's original
// resolution of docs/CONFORMANCE.md section 7 open question 4. Either way,
// a WS close observation is never itself treated as an unexpected frame --
// it's the terminal state, not an errant one -- and Expect fails immediately
// once the connection is closed with nothing left to wait for.
func (a *Actor) Expect(pred func(Observation) bool, timeout time.Duration) (Observation, error) {
	deadline := time.Now().Add(timeout)
	for {
		a.mu.Lock()
		for i := 0; i < len(a.log); i++ {
			if a.consumed[i] {
				continue
			}
			obs := a.log[i]
			if pred(obs) {
				if a.consumed == nil {
					a.consumed = map[int]bool{}
				}
				a.consumed[i] = true
				a.mu.Unlock()
				return obs, nil
			}
			if a.strict && obs.Close == nil {
				if obs.DecodeFailed != nil {
					a.mu.Unlock()
					return Observation{}, fmt.Errorf("rawactor: strict mode: undecodable WebSocket message while waiting for a match:\n%s", hex.Dump(obs.DecodeFailed))
				}
				if obs.Frame != nil && !isAllowedAutonomous(obs.Frame) {
					a.mu.Unlock()
					return Observation{}, fmt.Errorf("rawactor: strict mode: unexpected %s frame (stream %d, flags %#x, %d byte payload) while waiting for a match: %+v",
						wire.TypeName(obs.Frame.Type), obs.Frame.StreamID, obs.Frame.Flags, len(obs.Frame.Payload), obs.Frame)
				}
				if a.consumed == nil {
					a.consumed = map[int]bool{}
				}
				a.consumed[i] = true
			}
		}
		closed := a.closed
		ch := make(chan struct{})
		a.waiters = append(a.waiters, ch)
		a.mu.Unlock()

		if closed {
			return Observation{}, fmt.Errorf("rawactor: connection closed before a matching observation arrived")
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return Observation{}, fmt.Errorf("rawactor: timed out after %s waiting for a matching observation", timeout)
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ch:
			timer.Stop()
		case <-timer.C:
			return Observation{}, fmt.Errorf("rawactor: timed out after %s waiting for a matching observation", timeout)
		}
	}
}

// Close tears down the underlying transport/listener.
func (a *Actor) Close() {
	if a.conn != nil {
		_ = a.conn.Close(websocket.StatusNormalClosure, "rawactor: done")
	}
	if a.httpServer != nil {
		_ = a.httpServer.Close()
	}
	if a.listener != nil {
		_ = a.listener.Close()
	}
}

// BearerHeader builds an "Authorization: Bearer <token>" header.
func BearerHeader(token string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	return h
}
