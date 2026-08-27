// Package driver drives spec/fixtures/sequences/*.json (fixture mode) and
// conformance/scenarios/*.json (pair mode) against real adapters/raw actors,
// per docs/CONFORMANCE.md sections 3-4.
package driver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mcpwarp/ws-mixer/conformance/runner/adapter"
	"github.com/mcpwarp/ws-mixer/conformance/runner/fixture"
	"github.com/mcpwarp/ws-mixer/conformance/runner/rawactor"
	"github.com/mcpwarp/ws-mixer/conformance/runner/wire"
)

// Result is one (sdk, fixture-or-scenario) cell of the report.
type Result struct {
	Mode    string // "fixture" | "pair"
	SDK     string
	Name    string
	Status  string // PASS | FAIL | SKIP
	Message string
	Detail  string // interleaved log, printed on failure
}

// Fixtures whose transcript intentionally starts before any hello ever
// arrives (docs/CONFORMANCE.md section 3.3's driving table doesn't cover
// this -- it's about classifying `send` steps, not the handshake). See
// conformance/README.md for the reasoning.
var noHandshakeFixtures = map[string]bool{
	"frame_before_hello": true,
	"hello_timeout":      true,
	"app_before_welcome": true,
}

// Fixtures whose transcript starts after a handshake that already
// completed, with no hello/welcome step of its own -- the runner performs a
// synthetic handshake first so the replay finds a live connection.
var preHandshakeFixtures = map[string]bool{
	"drain_with_inflight_timeout":      true,
	"late_data_after_reset":            true,
	"data_for_never_opened_stream":     true,
	"pong_for_unsent_id":               true,
	"ping_pong_then_dead_peer_timeout": true,
	"request_response_half_close":      true,
	"window_after_close":               true,
}

const defaultToken = "conformance-default-token"

// watchdogFixtures need a scaled ping_interval/ping_timeout: either to
// exercise the keepalive watchdog itself in reasonable test time
// (ping_pong_then_dead_peer_timeout), or because the fixture's own
// transcript waits on an autonomous `ping` the SDK's internal ping loop
// only emits every ping_interval (pong_for_unsent_id) -- a real 30s
// ping_interval would make that fixture time out waiting for it.
var watchdogFixtures = map[string]bool{
	"ping_pong_then_dead_peer_timeout": true,
	"pong_for_unsent_id":               true,
	"duplicate_pong_from_server":       true,
	"client_dead_peer_timeout":         true,
}

// floorEnforcedFixtures deliberately test that the client REJECTS a welcome
// whose ping_interval/ping_timeout violate OVERVIEW.md section 2.9/2.10's
// floor -- allow_subfloor_timing:true (needed by every other fixture so a
// scaled-clock run's own sub-floor internal timers are accepted) would
// defeat the entire point of these by disabling the very check under test.
var floorEnforcedFixtures = map[string]bool{
	"welcome_ping_interval_below_floor": true,
}

// RunFixture drives one sequence fixture against ad (already spawned, with
// `ready` already observed and its role checked against f.Role by the
// caller).
func RunFixture(ctx context.Context, f *fixture.Fixture, ad *adapter.Adapter, timing Timing, sd *StepDriving) Result {
	res := Result{Mode: "fixture", SDK: ad.Name, Name: f.Name}
	var detail strings.Builder
	fail := func(format string, args ...any) Result {
		res.Status = "FAIL"
		res.Message = fmt.Sprintf(format, args...)
		var evDump strings.Builder
		for _, e := range ad.Events() {
			fmt.Fprintf(&evDump, "%+v\n", e.Raw)
		}
		res.Detail = detail.String() + "\n--- adapter events ---\n" + evDump.String() + "\n--- adapter stderr ---\n" + ad.Stderr()
		return res
	}
	logf := func(format string, args ...any) {
		fmt.Fprintf(&detail, format+"\n", args...)
	}

	// window/max_streams: a server-role fixture configures the SDK-under-test
	// server's own advertised welcome to match what the fixture's transcript
	// assumes (peekWindowMaxStreams, scanning for a welcome payload). A
	// client-role fixture must NOT be pre-seeded from that same welcome
	// scan -- the client's set_options carries only timing plus the client's
	// own hello values from the fixture (peekClientHello): the client's
	// window/max_streams are what *it* advertises in its hello, unrelated to
	// whatever window a `recv welcome` step's raw-actor-authored payload
	// happens to name.
	var window, maxStreams int64
	if f.Role == "client" {
		window, maxStreams = peekClientHello(f)
	} else {
		window, maxStreams = peekWindowMaxStreams(f)
	}
	// Only fixtures that actually exercise the keepalive watchdog need a
	// scaled ping_interval/ping_timeout; every other fixture's raw actor
	// never auto-pongs (docs/CONFORMANCE.md section 2: "no autonomous
	// behaviour"), so a scaled-down keepalive would otherwise race real
	// per-step process/IPC overhead and spuriously fire KEEPALIVE_TIMEOUT
	// midway through an unrelated fixture (conformance/README.md documents
	// this). Realistic, un-scaled values keep the watchdog from ever firing
	// except where the fixture deliberately waits past it.
	pingIntervalMs, pingTimeoutMs := int64(30000), int64(90000)
	if watchdogFixtures[f.Name] {
		pingIntervalMs, pingTimeoutMs = timing.ScaleMs(30000), timing.ScaleMs(90000)
	}

	// welcomeTimeScale is the ratio actually applied to a client-role
	// watchdog fixture's own scripted welcome (duplicate_pong_from_server,
	// client_dead_peer_timeout): unlike pingIntervalMs/pingTimeoutMs just
	// above (a SERVER-role SDK's own outgoing values, never validated by
	// anyone since the raw actor playing client does no validation at all),
	// a CLIENT-role SDK genuinely validates a *received* welcome's
	// ping_interval against the real, un-bypassable 5000ms wire floor --
	// and not every SDK has Go's allow_subfloor_timing escape hatch (JS's
	// control.ts enforces it unconditionally). So --time-scale's raw ratio
	// is clamped so the scaled ping_interval never drops below that floor,
	// and wait_ms steps in the same fixture use this same clamped ratio
	// (not raw timing.Scale) so they stay proportionate to the interval
	// that actually governs the real ping timer.
	welcomeTimeScale := timing.Scale
	if f.Role == "client" && watchdogFixtures[f.Name] {
		if origInterval := peekWelcomePingInterval(f); origInterval > 0 {
			scaledInterval := timing.ScaleMs(origInterval)
			if scaledInterval < wireFloorPingIntervalMs {
				scaledInterval = wireFloorPingIntervalMs
			}
			welcomeTimeScale = float64(scaledInterval) / float64(origInterval)
		}
	}
	if _, err := ad.SendAndAck("set_options", map[string]any{
		"window":                window,
		"max_streams":           maxStreams,
		"ping_interval_ms":      pingIntervalMs,
		"ping_timeout_ms":       pingTimeoutMs,
		"hello_timeout_ms":      timing.ScaleMs(10000),
		"time_scale":            timing.Scale,
		"floor_ms":              timing.FloorMs(),
		"allow_subfloor_timing": !floorEnforcedFixtures[f.Name],
	}, 5*time.Second); err != nil {
		if errors.Is(err, adapter.ErrUnsupported) {
			res.Status = "SKIP"
			res.Message = "set_options: " + err.Error()
			return res
		}
		return fail("set_options: %v", err)
	}

	var ra *rawactor.Actor
	// skipNextWelcomeObserve: when a synthetic handshake already waited for
	// (and consumed) the welcome observation, a fixture whose own transcript
	// happens to start with an observe-only `send welcome` step (e.g.
	// pong_for_unsent_id.json) would otherwise wait forever for a second,
	// nonexistent welcome. Cleared the first time it is used.
	skipNextWelcomeObserve := false
	// skipNextHelloObserve: role:"client" fixtures pre-consume the client's
	// hello as a synchronization point (see below) before replay starts, so
	// a fixture whose own transcript separately observes `send hello` (e.g.
	// frame_before_welcome.json's step 0) must not wait for a second one.
	skipNextHelloObserve := false
	defer func() {
		if ra != nil {
			ra.Close()
		}
	}()

	helloToken := peekHelloToken(f)
	if helloToken == "" {
		helloToken = defaultToken
	}
	headerToken := helloToken
	if f.Name == "auth_failure" {
		headerToken = helloToken + "-mismatched-on-purpose"
	}

	if f.Role == "server" {
		lst, err := ad.SendAndAck("listen", map[string]any{"addr": "127.0.0.1:0"}, 5*time.Second)
		if err != nil {
			if errors.Is(err, adapter.ErrUnsupported) {
				res.Status = "SKIP"
				res.Message = "listen: " + err.Error()
				return res
			}
			return fail("listen: %v", err)
		}
		listening, err := ad.WaitForTimeout(func(e adapter.Event) bool { return e.Event == "listening" }, 5*time.Second)
		if err != nil {
			// Some adapters may fold listening into the ack itself.
			if u := lst.String("url"); u != "" {
				listening = lst
			} else {
				return fail("waiting for listening: %v", err)
			}
		}
		url := listening.String("url")
		if url == "" {
			return fail("listening event carried no url")
		}
		logf("listening at %s", url)

		a, err := rawactor.Dial(ctx, url, rawactor.BearerHeader(headerToken))
		if err != nil {
			return fail("raw actor dial: %v", err)
		}
		ra = a
		ra.SetStrict(timing.Strict)

		if preHandshakeFixtures[f.Name] {
			skipNextWelcomeObserve = true
			if err := performSyntheticHandshake(ctx, ra, helloToken, timing); err != nil {
				return fail("synthetic handshake: %v", err)
			}
			logf("synthetic handshake completed")
		}
	} else {
		a, url, err := rawactor.Serve(ctx, "127.0.0.1:0")
		if err != nil {
			return fail("raw actor serve: %v", err)
		}
		ra = a
		ra.SetStrict(timing.Strict)
		// Fire-and-forget, deliberately not SendAndAck: several role:"client"
		// fixtures (frame_before_welcome, max_streams_exceeded) never send a
		// welcome at all, so the underlying Dial() may not return (success or
		// failure) until well after the steps below have driven the raw actor
		// through the very bytes that determine its outcome. Blocking here
		// would deadlock the replay against its own precondition.
		if _, err := ad.SendCmd("connect", map[string]any{"url": url, "token": helloToken}); err != nil {
			return fail("connect: %v", err)
		}
		// Every real client sends hello as its very first action once the WS
		// is open, well before this fixture's own first scripted step -- so
		// waiting for it here (silently; it is not itself asserted) is a
		// legitimate synchronization point, not a fixture-content check: it
		// proves the client's read/dispatch pipeline is fully live before
		// this replay starts pushing welcome/OPEN/etc at it. Without this,
		// welcome+OPEN sent back to back immediately after accept can
		// occasionally reach the client before its own async setup
		// (post-'open', pre-dispatch) has settled, which otherwise surfaces
		// as a flaky "OPEN received before welcome completed the handshake"
		// (conformance/README.md documents this).
		_, _ = ra.Expect(func(o rawactor.Observation) bool { return isControl(o, "hello") }, defaultTimeout)
		skipNextHelloObserve = true
	}

	streamStates := map[uint32]string{}
	// resetObserved marks a stream id whose "closed" state was already
	// confirmed directly (a wire-level RESET the raw actor observed, or the
	// adapter's own stream_reset event) rather than derived from
	// stream_opened/stream_closed bookkeeping. applyAdapterStreamEvents
	// below replays the *entire* accumulated event log every time it's
	// called (ad.Events() is not a delta), so without this guard a stream
	// that was legitimately opened and half-closed *before* an autonomous
	// RESET (e.g. data_after_close_toward_client.json: OPEN, CLOSE, then an
	// illegal DATA autonomously RESET) would have its "closed" set by the
	// RESET-step fallback below immediately clobbered back to
	// "half_closed_remote" on a later replay if that particular adapter's
	// stream_reset event for the case happens to lag behind (both reference
	// adapters do eventually emit one here -- the Go adapter's
	// waitForLateStreamReset polls Stream.LastError() after read-EOF -- but
	// this guard doesn't depend on the timing of when that event lands).
	resetObserved := map[uint32]bool{}
	var lastStreamID uint32
	var lastResetName string
	var lastError ObservedError

	applyAdapterStreamEvents := func() {
		for _, e := range ad.Events() {
			id64, _ := e.Int("id")
			id := uint32(id64)
			if resetObserved[id] && e.Event != "stream_reset" {
				continue
			}
			switch e.Event {
			case "stream_opened":
				streamStates[id] = "open"
			case "stream_closed":
				dir := e.String("direction")
				switch streamStates[id] {
				case "open":
					if dir == "write" {
						streamStates[id] = "half_closed_local"
					} else if dir == "read" {
						streamStates[id] = "half_closed_remote"
					} else {
						streamStates[id] = "closed"
					}
				case "half_closed_local":
					if dir == "read" || dir == "both" {
						streamStates[id] = "closed"
					}
				case "half_closed_remote":
					if dir == "write" || dir == "both" {
						streamStates[id] = "closed"
					}
				}
			case "stream_reset":
				streamStates[id] = "closed"
				resetObserved[id] = true
				lastResetName = e.String("name")
			}
		}
	}

	for i, st := range f.Steps {
		switch st.Kind() {
		case "recv":
			if err := driveRecv(ra, st.Recv, &lastStreamID, watchdogFixtures[f.Name], welcomeTimeScale); err != nil {
				return fail("step %d (recv): %v", i, err)
			}
			logf("step %d: recv %v (raw actor sent it)", i, st.Recv)

		case "send":
			if skipNextWelcomeObserve && fixture.ControlType(st.Send) == "welcome" {
				skipNextWelcomeObserve = false
				logf("step %d: send %v (already observed by the synthetic handshake)", i, st.Send)
				continue
			}
			if skipNextHelloObserve && fixture.ControlType(st.Send) == "hello" {
				skipNextHelloObserve = false
				logf("step %d: send %v (already observed as the pre-replay sync point)", i, st.Send)
				continue
			}
			if err := driveSend(ctx, ad, ra, f.Role, st.Send, i, sd, f.Name, timing, &lastStreamID, &lastResetName, &lastError); err != nil {
				if errors.Is(err, adapter.ErrUnsupported) {
					res.Status = "SKIP"
					res.Message = fmt.Sprintf("step %d (send): %v", i, err)
					return res
				}
				return fail("step %d (send): %v", i, err)
			}
			if fixture.IsFrame(st.Send) && fixture.FrameType(st.Send) == "RESET" {
				// A RESET the SDK emitted autonomously (STREAM_LIMIT/STREAM_CLOSED,
				// or a sidecar-forced "auto") may never have become an
				// app-visible stream at all -- go/wsmixer's client, e.g.,
				// never fires OnStream for an OPEN it refuses (dispatch.go's
				// handleRemoteOpen), so there is no adapter event to replay
				// into streamStates for it. The raw actor's own confirmation
				// that the RESET went out on the wire (driveSend, just above,
				// which also sets lastResetName from the observed frame) is
				// wire-level truth this runner observed directly, so
				// streamStates is updated here regardless of whether the
				// adapter also reported it.
				streamStates[fixture.StreamID(st.Send)] = "closed"
				resetObserved[fixture.StreamID(st.Send)] = true
			}
			logf("step %d: send %v", i, st.Send)

		case "wait_ms":
			d := timing.Scale_(*st.WaitMs)
			if f.Role == "client" && watchdogFixtures[f.Name] {
				// Stay proportionate to the (possibly floor-clamped)
				// welcome timing this same fixture just fed the client --
				// see welcomeTimeScale above.
				d = time.Duration(float64(*st.WaitMs)*welcomeTimeScale) * time.Millisecond
			}
			logf("step %d: wait_ms %d (scaled to %s)", i, *st.WaitMs, d)
			time.Sleep(d)

		case "expect":
			// stream_state/stream_reset_code are derived from the adapter's
			// own event stream, which updates asynchronously relative to
			// whatever raw-actor byte or command just preceded this step
			// (e.g. the adapter's auto-read goroutine noticing a just-sent
			// CLOSE) -- so poll for up to defaultTimeout rather than judging
			// on a single snapshot.
			deadline := time.Now().Add(defaultTimeout)
			var lastErr error
			for {
				applyAdapterStreamEvents()
				lastErr = checkExpect(ctx, ad, ra, st.Expect, streamStates, lastStreamID, lastResetName, lastError, timing)
				if lastErr == nil || time.Now().After(deadline) {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if lastErr != nil {
				return fail("step %d (expect %v): %v", i, st.Expect, lastErr)
			}
			logf("step %d: expect %v OK", i, st.Expect)
		}
	}

	res.Status = "PASS"
	res.Detail = detail.String()
	return res
}

// peekWindowMaxStreams scans the fixture for a welcome payload's
// window/max_streams -- the value the adapter-under-test's own `listen`
// (or `connect`) must be configured with so its *own* advertised welcome
// (server role) or negotiated cap (client role) matches what the fixture's
// transcript assumes (e.g. credit_violation.json and
// window_exhaustion_then_resume.json need the server's real receive window
// to be 16384, not the 262144 default, or their credit-violation numbers
// stop meaning anything). Defaults to the wire defaults (262144, 64) when
// the fixture never mentions welcome at all.
func peekWindowMaxStreams(f *fixture.Fixture) (window, maxStreams int64) {
	window, maxStreams = 262144, 64
	for _, st := range f.Steps {
		payload := st.Recv
		if payload == nil {
			payload = st.Send
		}
		if payload != nil && fixture.ControlType(payload) == "welcome" {
			if w, ok := payload["window"].(float64); ok {
				window = int64(w)
			}
			if m, ok := payload["max_streams"].(float64); ok {
				maxStreams = int64(m)
			}
			return
		}
	}
	return
}

// peekClientHello scans a role:"client" fixture for the client-under-test's
// own hello payload (a `send` step, since the client emits it) for the
// window/max_streams it advertises, so the client's own set_options matches
// what its transcript assumes it will hello with. Defaults to the wire
// defaults (262144, 64) when the fixture's hello step never mentions them --
// mirroring peekWindowMaxStreams' defaults for the server-role case.
func peekClientHello(f *fixture.Fixture) (window, maxStreams int64) {
	window, maxStreams = 262144, 64
	for _, st := range f.Steps {
		payload := st.Send
		if payload == nil {
			payload = st.Recv
		}
		if payload != nil && fixture.ControlType(payload) == "hello" {
			if w, ok := payload["window"].(float64); ok {
				window = int64(w)
			}
			if m, ok := payload["max_streams"].(float64); ok {
				maxStreams = int64(m)
			}
			return
		}
	}
	return
}

func peekHelloToken(f *fixture.Fixture) string {
	for _, st := range f.Steps {
		payload := st.Recv
		if payload == nil {
			payload = st.Send
		}
		if payload != nil && fixture.ControlType(payload) == "hello" {
			if tok, ok := payload["token"].(string); ok {
				return tok
			}
		}
	}
	return ""
}

func performSyntheticHandshake(ctx context.Context, ra *rawactor.Actor, token string, timing Timing) error {
	hello := map[string]any{
		"t": "hello", "v": 1, "token": token,
		"agent":       map[string]any{"sdk": "ws-mixer-conformance-rawactor", "sdk_version": "0.1.0"},
		"window":      262144,
		"max_streams": 64,
	}
	if err := ra.SendControl(hello); err != nil {
		return err
	}
	_, err := ra.Expect(func(o rawactor.Observation) bool {
		return isControl(o, "welcome")
	}, timing.Grace()+5*time.Second)
	return err
}

func isControl(o rawactor.Observation, t string) bool {
	if o.Frame == nil || o.Frame.Type != wire.TypeData || o.Frame.StreamID != 0 {
		return false
	}
	var m map[string]any
	if json.Unmarshal(o.Frame.Payload, &m) != nil {
		return false
	}
	return m["t"] == t
}

// wireFloorPingIntervalMs is OVERVIEW.md section 2.9/2.10's welcome.ping_interval
// floor: the one every real client validates on a *received* welcome,
// unconditionally in some SDKs (JS's control.ts) and bypassably in others
// (Go's allow_subfloor_timing) -- so a runner-side scale-down of a
// client-role fixture's own scripted welcome must never cross it, unlike a
// SERVER-role SDK's own outgoing values (pingIntervalMs/pingTimeoutMs
// above), which nobody here validates.
const wireFloorPingIntervalMs = 5000

// peekWelcomePingInterval scans a role:"client" fixture for its one "recv
// welcome" step's ping_interval, or 0 if it has none.
func peekWelcomePingInterval(f *fixture.Fixture) int64 {
	for _, st := range f.Steps {
		if st.Recv != nil && fixture.ControlType(st.Recv) == "welcome" {
			if v, ok := st.Recv["ping_interval"].(float64); ok {
				return int64(v)
			}
		}
	}
	return 0
}

// driveRecv: "a recv step (any payload): raw actor sends those bytes"
// (docs/CONFORMANCE.md section 3.3, last row).
// driveRecv sends a fixture's "recv" step payload to the SDK-under-test via
// the raw actor. scaleWelcomeTiming is true only for watchdogFixtures: those
// fixtures now write their welcome's ping_interval/ping_timeout as plain,
// unscaled protocol time (30000/90000, matching what a real server would
// send), so this raw actor -- which otherwise echoes a "recv welcome" step's
// payload onto the wire completely verbatim -- must scale those two fields
// itself before sending (by welcomeScale, RunFixture's floor-clamped ratio,
// not the raw --time-scale), or the client-under-test's real keepalive
// timers would run for the full unscaled duration and the fixture's own
// (also correspondingly scaled) wait_ms would never catch up to them.
func driveRecv(ra *rawactor.Actor, payload map[string]any, lastStreamID *uint32, scaleWelcomeTiming bool, welcomeScale float64) error {
	if fixture.IsFrame(payload) {
		f, err := frameFromPayload(payload)
		if err != nil {
			return err
		}
		if f.StreamID != 0 {
			*lastStreamID = f.StreamID
		}
		return ra.SendFrame(f)
	}
	if scaleWelcomeTiming && fixture.ControlType(payload) == "welcome" {
		payload = scaleWelcomePayloadTiming(payload, welcomeScale)
	}
	return ra.SendControl(payload)
}

// scaleWelcomePayloadTiming returns a shallow copy of a welcome payload with
// ping_interval/ping_timeout scaled by welcomeScale, leaving every other
// field untouched. welcomeScale is already floor-clamped by the caller
// (RunFixture's welcomeTimeScale), so no further flooring happens here.
func scaleWelcomePayloadTiming(payload map[string]any, welcomeScale float64) map[string]any {
	out := make(map[string]any, len(payload))
	for k, v := range payload {
		out[k] = v
	}
	if v, ok := payload["ping_interval"].(float64); ok {
		out["ping_interval"] = int64(v * welcomeScale)
	}
	if v, ok := payload["ping_timeout"].(float64); ok {
		out["ping_timeout"] = int64(v * welcomeScale)
	}
	return out
}

func frameFromPayload(payload map[string]any) (*wire.Frame, error) {
	typ := fixture.FrameType(payload)
	sid := fixture.StreamID(payload)
	f := &wire.Frame{StreamID: sid}
	switch typ {
	case "OPEN":
		f.Type = wire.TypeOpen
	case "DATA":
		f.Type = wire.TypeData
		p, err := payloadBytes(payload)
		if err != nil {
			return nil, err
		}
		f.Payload = p
	case "WINDOW":
		inc, _ := payload["increment"].(float64)
		decoded, _ := wire.Decode(wire.Window(sid, uint32(inc)))
		f.Type = wire.TypeWindow
		f.Payload = decoded.Payload
	case "CLOSE":
		f.Type = wire.TypeClose
	case "RESET":
		f.Type = wire.TypeReset
		code, _ := payload["code"].(float64)
		msg, _ := payload["message"].(string)
		encoded := wire.Reset(sid, uint32(code), msg)
		decoded, _ := wire.Decode(encoded)
		f.Payload = decoded.Payload
	default:
		return nil, fmt.Errorf("unknown frame type %q", typ)
	}
	return f, nil
}

func payloadBytes(payload map[string]any) ([]byte, error) {
	if hexStr, ok := payload["payload_hex"].(string); ok {
		return hexDecode(hexStr)
	}
	if n, ok := payload["payload_length"].(float64); ok {
		return make([]byte, int(n)), nil
	}
	return nil, nil
}

func hexDecode(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("odd-length hex string %q", s)
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(out); i++ {
		hi, err := hexNibble(s[2*i])
		if err != nil {
			return nil, err
		}
		lo, err := hexNibble(s[2*i+1])
		if err != nil {
			return nil, err
		}
		out[i] = hi<<4 | lo
	}
	return out, nil
}

func hexNibble(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, nil
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, nil
	default:
		return 0, fmt.Errorf("invalid hex digit %q", c)
	}
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
