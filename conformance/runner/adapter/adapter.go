// Package adapter spawns one SDK adapter process (docs/CONFORMANCE.md
// section 1), speaks its JSON-lines command/event protocol, and tears it
// down. It knows nothing about fixtures or scenarios -- only the wire shape
// of that protocol.
package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// ErrUnsupported wraps a command failure whose error event marked itself
// unsupported (`{"ok":false,"error":"unsupported"}` or `unsupported:true`,
// per the coordination contract with conformance/adapters/**): a
// client-only adapter declining a server-role command, or an adapter that
// hasn't implemented a given command yet. Callers use errors.Is against
// this to turn the cell into a SKIP with reason rather than a FAIL.
var ErrUnsupported = errors.New("adapter: command unsupported")

func isUnsupported(ev Event) bool {
	if b, ok := ev.Raw["unsupported"].(bool); ok && b {
		return true
	}
	if s, ok := ev.Raw["error"].(string); ok && s == "unsupported" {
		return true
	}
	return false
}

// Command is one JSON-lines command sent to an adapter's stdin. Fields not
// relevant to a given cmd are simply omitted on marshal (via omitempty).
type Command struct {
	Seq  int64  `json:"seq"`
	Cmd  string `json:"cmd"`
	Args map[string]any
}

// MarshalJSON flattens Args alongside seq/cmd into one JSON object, so the
// wire shape matches docs/CONFORMANCE.md section 1.1's per-command field
// tables exactly (no nested "args" envelope).
func (c Command) MarshalJSON() ([]byte, error) {
	m := map[string]any{"seq": c.Seq, "cmd": c.Cmd}
	for k, v := range c.Args {
		m[k] = v
	}
	return json.Marshal(m)
}

// Event is one JSON-lines event read from an adapter's stdout. Raw carries
// the full decoded object so callers can pull out event-specific fields
// without this package needing a case for every one of the 12 event shapes.
type Event struct {
	Raw   map[string]any
	Event string // Raw["event"]
	Seq   int64  // Raw["seq"], 0 if absent
}

func (e Event) String(key string) string {
	if v, ok := e.Raw[key].(string); ok {
		return v
	}
	return ""
}

func (e Event) Int(key string) (int64, bool) {
	switch v := e.Raw[key].(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	}
	return 0, false
}

func (e Event) Bool(key string) bool {
	v, _ := e.Raw[key].(bool)
	return v
}

// Adapter is one spawned adapter process: a fresh process per test case,
// killed after (docs/CONFORMANCE.md section 1).
type Adapter struct {
	Name string // "go" or "js", for reporting

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *lockedBuffer

	mu      sync.Mutex
	events  []Event
	waiters []chan struct{}
	closed  bool
	readErr error
	// consumed marks event indices a previous WaitFor call already matched,
	// so a strictly sequential replay never re-matches the same event twice.
	// Deliberately not a monotonic cursor: pair mode's multi-stream
	// scenarios issue several awaits whose underlying events can be emitted
	// out of the DSL's own step order (e.g. one goroutine per stream), and a
	// cursor would permanently skip an earlier-arriving-but-later-awaited
	// event out from under a still-pending WaitFor call.
	consumed map[int]bool

	nextSeq atomic.Int64
}

// Spawn starts the adapter at path (argv[0] plus extraArgs), in its own
// process group so Kill can take down every descendant it spawns, and
// begins reading its stdout in the background. name is set on the Adapter
// before the read loop goroutine starts, so it is never written after
// another goroutine may be reading it (Adapter.Name is otherwise
// immutable post-Spawn).
func Spawn(name, path string, extraArgs ...string) (*Adapter, error) {
	cmd := exec.Command(path, extraArgs...)
	cmd.SysProcAttr = setpgid()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("adapter: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("adapter: stdout pipe: %w", err)
	}
	stderrBuf := &lockedBuffer{}
	cmd.Stderr = stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("adapter: start %s: %w", path, err)
	}

	a := &Adapter{Name: name, cmd: cmd, stdin: stdin, stderr: stderrBuf}
	go a.readLoop(stdout)
	return a, nil
}

func (a *Adapter) readLoop(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytesTrimSpace(line)) == 0 {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal(line, &raw); err != nil {
			a.mu.Lock()
			a.readErr = fmt.Errorf("adapter %s emitted an unparseable line: %q: %w", a.Name, string(line), err)
			a.mu.Unlock()
			continue
		}
		ev := Event{Raw: raw, Event: stringField(raw, "event")}
		if n, ok := numberField(raw, "seq"); ok {
			ev.Seq = n
		}
		a.mu.Lock()
		a.events = append(a.events, ev)
		waiters := a.waiters
		a.waiters = nil
		a.mu.Unlock()
		for _, w := range waiters {
			close(w)
		}
	}
	a.mu.Lock()
	a.closed = true
	waiters := a.waiters
	a.waiters = nil
	a.mu.Unlock()
	for _, w := range waiters {
		close(w)
	}
}

func stringField(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func numberField(m map[string]any, k string) (int64, bool) {
	f, ok := m[k].(float64)
	if !ok {
		return 0, false
	}
	return int64(f), true
}

func bytesTrimSpace(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r' || b[i] == '\n') {
		i++
	}
	for j > i && (b[j-1] == ' ' || b[j-1] == '\t' || b[j-1] == '\r' || b[j-1] == '\n') {
		j--
	}
	return b[i:j]
}

// NextSeq allocates the next monotonic command seq.
func (a *Adapter) NextSeq() int64 { return a.nextSeq.Add(1) }

// Send writes one command as a JSON line to the adapter's stdin.
func (a *Adapter) Send(cmd Command) error {
	b, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = a.stdin.Write(b)
	return err
}

// SendCmd is Send with an auto-allocated seq; returns the seq used.
func (a *Adapter) SendCmd(name string, args map[string]any) (int64, error) {
	seq := a.NextSeq()
	return seq, a.Send(Command{Seq: seq, Cmd: name, Args: args})
}

// Events returns a snapshot of every event observed so far, in order.
func (a *Adapter) Events() []Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]Event, len(a.events))
	copy(out, a.events)
	return out
}

// LastDisconnected returns the `disconnected` event if one has been
// observed so far (docs/CONFORMANCE.md section 1.2: emitted exactly once
// per connection, whatever the reason). Unlike WaitFor, this is a
// non-consuming peek at the full log -- it does not advance the cursor.
func (a *Adapter) LastDisconnected() (Event, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.events {
		if e.Event == "disconnected" {
			return e, true
		}
	}
	return Event{}, false
}

// Stderr returns everything the adapter has written to stderr so far.
func (a *Adapter) Stderr() string { return a.stderr.String() }

// WaitFor blocks until an event matching pred has been observed (searching
// from the start of the log each time it is newly woken, so late-arriving
// events already logged before this call are found too), or ctx is done.
func (a *Adapter) WaitFor(ctx context.Context, pred func(Event) bool) (Event, error) {
	for {
		a.mu.Lock()
		for i := 0; i < len(a.events); i++ {
			if a.consumed[i] {
				continue
			}
			if pred(a.events[i]) {
				if a.consumed == nil {
					a.consumed = map[int]bool{}
				}
				a.consumed[i] = true
				ev := a.events[i]
				a.mu.Unlock()
				return ev, nil
			}
		}
		if a.closed {
			readErr := a.readErr
			a.mu.Unlock()
			if readErr != nil {
				return Event{}, readErr
			}
			return Event{}, fmt.Errorf("adapter %s exited without emitting a matching event", a.Name)
		}
		ch := make(chan struct{})
		a.waiters = append(a.waiters, ch)
		a.mu.Unlock()

		select {
		case <-ch:
		case <-ctx.Done():
			return Event{}, ctx.Err()
		}
	}
}

// WaitForTimeout is WaitFor with a plain timeout, for callers that don't
// already have a context.
func (a *Adapter) WaitForTimeout(pred func(Event) bool, timeout time.Duration) (Event, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return a.WaitFor(ctx, pred)
}

// WaitAbsentTimeout is WaitFor's negation: it watches for window and reports
// whether an unconsumed event matching pred showed up in that time. Unlike
// WaitFor it never blocks past window (there is nothing to wait "until" when
// the whole point is proving absence), and unlike WaitFor it never marks a
// matching event consumed -- on a pass there was nothing to consume, and on
// a violation the caller fails the scenario immediately, so leaving the
// event in place keeps it visible in Events()/Stderr() dumps for debugging
// rather than silently swallowing it. A pred that never matches leaves every
// other pending WaitFor call on this adapter completely unaffected: this
// call only reads the log, it never advances a cursor other calls share.
//
// Three outcomes, mirroring WaitFor's own dead-adapter handling:
//   - (ev, true, nil): a matching event was observed -- the negative
//     assertion is violated.
//   - (Event{}, false, nil): the window elapsed with the adapter still alive
//     and nothing matching arrived -- a genuine pass.
//   - (Event{}, false, err): the adapter exited -- before this call even
//     started, or partway through the window -- without ever producing a
//     matching event. This is deliberately NOT the same as a pass: a
//     negative assertion proves "nothing happened" only by watching a live
//     process for the whole window, and a dead process proves nothing at
//     all (docs/CONFORMANCE.md section 3.2) -- e.g. an SDK that crashes
//     right after an application close would otherwise turn
//     application_close_client.json's trailing await_none steps into a
//     trivial PASS instead of the FAIL a crash deserves. err is the
//     adapter's own read error when it has one, else the same
//     "exited without emitting a matching event" shape WaitFor uses.
func (a *Adapter) WaitAbsentTimeout(pred func(Event) bool, window time.Duration) (Event, bool, error) {
	deadline := time.Now().Add(window)
	for {
		a.mu.Lock()
		for i := 0; i < len(a.events); i++ {
			if a.consumed[i] {
				continue
			}
			if pred(a.events[i]) {
				ev := a.events[i]
				a.mu.Unlock()
				return ev, true, nil
			}
		}
		if a.closed {
			readErr := a.readErr
			a.mu.Unlock()
			if readErr != nil {
				return Event{}, false, readErr
			}
			return Event{}, false, fmt.Errorf("adapter %s exited before the await_none window (%v) elapsed, without emitting a matching event -- a negative assertion cannot pass against a dead process", a.Name, window)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			a.mu.Unlock()
			return Event{}, false, nil
		}
		ch := make(chan struct{})
		a.waiters = append(a.waiters, ch)
		a.mu.Unlock()

		select {
		case <-ch:
		case <-time.After(remaining):
			return Event{}, false, nil
		}
	}
}

// SendAndAck sends cmd and waits for its ack{seq} (or an error{seq}, which
// is returned as an error).
func (a *Adapter) SendAndAck(name string, args map[string]any, timeout time.Duration) (Event, error) {
	seq, err := a.SendCmd(name, args)
	if err != nil {
		return Event{}, err
	}
	ev, err := a.WaitForTimeout(func(e Event) bool {
		return (e.Event == "ack" || e.Event == "error") && e.Seq == seq
	}, timeout)
	if err != nil {
		return Event{}, fmt.Errorf("waiting for ack of %s(seq=%d): %w", name, seq, err)
	}
	if ev.Event == "error" {
		if isUnsupported(ev) {
			return ev, fmt.Errorf("%s(seq=%d): %w: %s", name, seq, ErrUnsupported, ev.String("message"))
		}
		return ev, fmt.Errorf("%s(seq=%d) failed: %s", name, seq, ev.String("message"))
	}
	return ev, nil
}

// Kill terminates the adapter process (and its process group, so a JS
// adapter's own child processes don't orphan) and waits for exit.
func (a *Adapter) Kill() {
	if a.cmd.Process == nil {
		return
	}
	killGroup(a.cmd.Process.Pid)
	_ = a.stdin.Close()
	done := make(chan struct{})
	go func() {
		_ = a.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = a.cmd.Process.Kill()
		<-done
	}
}
