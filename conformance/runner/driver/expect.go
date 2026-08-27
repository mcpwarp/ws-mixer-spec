package driver

import (
	"context"
	"fmt"
	"time"

	"github.com/mcpwarp/ws-mixer/conformance/runner/adapter"
	"github.com/mcpwarp/ws-mixer/conformance/runner/codes"
	"github.com/mcpwarp/ws-mixer/conformance/runner/rawactor"
)

// negativeWindow is how long checkExpect waits before concluding a `null`
// assertion holds (docs/CONFORMANCE.md section 4: "a bounded wait, ~40ms,
// before failing" -- scaled up a bit here for real process/goroutine
// scheduling jitter around a spawned adapter rather than an in-process fake).
func negativeWindow(t Timing) time.Duration {
	g := t.Grace()
	if g < 150*time.Millisecond {
		g = 150 * time.Millisecond
	}
	return g
}

// checkExpect implements docs/CONFORMANCE.md section 4's expect -> observable
// mapping. error_code is asserted from the raw actor's own observed error{}
// control frame (numeric `code` + `name`), not from the fixture payload or
// the WS close code alone -- the close code is then checked *against* the
// error{} frame's code as an invariant (section 4: "close_code == 4000 +
// error_code"). stream_reset_code is asserted from whichever side actually
// received the RESET: the SDK adapter's own `stream_reset` event when the
// SDK-under-test is on the receiving end (client role fixtures reading a
// server-sent RESET, or vice versa), or the raw actor's directly observed
// RESET frame otherwise -- never read back out of the fixture JSON that
// produced it (fixture_driver.go threads lastResetName from exactly these
// two sources). send_window is never asserted (section 1.3).
func checkExpect(
	ctx context.Context,
	ad *adapter.Adapter,
	ra *rawactor.Actor,
	expect map[string]any,
	streamStates map[uint32]string,
	lastStreamID uint32,
	lastResetName string,
	lastError ObservedError,
	timing Timing,
) error {
	if v, ok := expect["stream_state"]; ok {
		want, _ := v.(string)
		got := streamStates[lastStreamID]
		if want == "" && got == "" {
			// no assertion possible without a stream in context; treat as pass
		} else if got != want {
			return fmt.Errorf("stream_state = %q, want %q (stream %d)", got, want, lastStreamID)
		}
	}

	if v, ok := expect["stream_reset_code"]; ok {
		want, _ := v.(string)
		if lastResetName != want {
			return fmt.Errorf("stream_reset_code = %q, want %q", lastResetName, want)
		}
	}

	if v, ok := expect["error_code"]; ok {
		if v == nil {
			// Assert absence: no disconnected event within the negative
			// window, then a liveness probe -- the connection must not just
			// be silent, it must still be answering pings (docs/CONFORMANCE.md
			// section 4: "still alive and healthy" is only true if neither
			// side saw a death).
			time.Sleep(negativeWindow(timing))
			if ev, found := ad.LastDisconnected(); found {
				return fmt.Errorf("expected no error, but adapter disconnected: %s (%s)", ev.String("message"), ev.String("error_name"))
			}
			if cinfo, closed := ra.Closed(); closed {
				return fmt.Errorf("expected no error, but raw actor observed a WS close: code=%d reason=%q", cinfo.Code, cinfo.Reason)
			}
			// ping.id must be an integer and ping needs `ts` (OVERVIEW.md
			// section 2.10's wire shape, `{"t":"ping","id":42,"ts":...}`) --
			// anything else is itself a PROTOCOL_ERROR the SDK would
			// (correctly) tear the connection down for, which would make
			// this probe self-defeating.
			// UnixMilli, not UnixNano: JSON round-trips ping.id through a
			// float64 on the wire, which loses precision above 2^53 --
			// milliseconds-since-epoch stays comfortably inside that.
			pingID := time.Now().UnixMilli()
			if err := ra.SendControl(map[string]any{"t": "ping", "id": pingID, "ts": time.Now().UnixMilli()}); err != nil {
				return fmt.Errorf("error_code: null liveness probe: sending ping: %w", err)
			}
			_, err := ra.Expect(func(o rawactor.Observation) bool {
				m := decodeControl(o)
				if m == nil || m["t"] != "pong" {
					return false
				}
				gotID, ok := m["id"].(float64)
				return ok && int64(gotID) == pingID
			}, timing.Grace())
			if err != nil {
				return fmt.Errorf("error_code: null liveness probe: no pong within %s: %w", timing.Grace(), err)
			}
		} else {
			wantName, _ := v.(string)
			wantCode, ok := codes.Code(wantName)
			if !ok {
				return fmt.Errorf("expect.error_code %q is not a known error code name", wantName)
			}
			// Primary authority: the raw actor's own observed error{} control
			// frame -- this fires even for a handshake failure the adapter's
			// OnConn/onConnect never saw (auth_failure, hello_timeout,
			// frame_before_hello, ...), unlike the adapter's `disconnected`
			// event. If an earlier `send` step of type "error" in this same
			// fixture already observed (and consumed) it, reuse that --
			// rawactor.Expect only ever matches a given wire observation
			// once, so waiting here again would hang until defaultTimeout.
			var errAt time.Time
			var gotCode uint32
			var gotName string
			if lastError.Have {
				errAt, gotCode, gotName = lastError.At, lastError.Code, lastError.Name
			} else {
				errObs, err := ra.Expect(func(o rawactor.Observation) bool { return isControl(o, "error") }, defaultTimeout)
				if err != nil {
					return fmt.Errorf("waiting for error{} frame: %w", err)
				}
				m := decodeControl(errObs)
				gotCodeF, _ := m["code"].(float64)
				gotCode = uint32(gotCodeF)
				gotName, _ = m["name"].(string)
				if msg, _ := m["message"].(string); msg == "" {
					return fmt.Errorf("error.message must be non-empty")
				}
				errAt = errObs.At
			}
			if gotCode != wantCode {
				return fmt.Errorf("error.code = %d (%s), want %d (%s)", gotCode, codes.Name(gotCode), wantCode, wantName)
			}
			if gotName != "" && gotName != wantName {
				return fmt.Errorf("error.name = %s, want %s", gotName, wantName)
			}

			// Invariant 1: error{} precedes the WS close.
			cinfo, err := waitRawClose(ra, defaultTimeout)
			if err != nil {
				return fmt.Errorf("waiting for raw actor close: %w", err)
			}
			if closeObs, found := ra.CloseObservation(); found && errAt.After(closeObs.At) {
				return fmt.Errorf("error{} frame observed at %s, after the WS close at %s (invariant: error precedes close)", errAt, closeObs.At)
			}

			// Invariant 2: close_code == 4000 + error.code (1000 for NO_ERROR).
			wantClose := codes.CloseCode(wantCode)
			if cinfo.Code != wantClose {
				return fmt.Errorf("close_code = %d, want %d (= 4000 + error.code %d, per OVERVIEW.md section 2.8)", cinfo.Code, wantClose, wantCode)
			}

			// Secondary, best-effort cross-check: if the adapter also
			// reported a disconnected event by now, its name must agree.
			if ev, found := ad.LastDisconnected(); found {
				if adapterName := ev.String("error_name"); adapterName != "" && adapterName != wantName {
					return fmt.Errorf("adapter disconnected.error_name = %s, want %s (raw actor agrees on error.code/close_code)", adapterName, wantName)
				}
			}
		}
	}

	if v, ok := expect["close_code"]; ok {
		if v == nil {
			time.Sleep(negativeWindow(timing))
			if cinfo, closed := ra.Closed(); closed {
				return fmt.Errorf("expected no close, but raw actor observed WS close code %d", cinfo.Code)
			}
		} else {
			wantF, _ := v.(float64)
			want := int(wantF)
			cinfo, err := waitRawClose(ra, defaultTimeout)
			if err != nil {
				return fmt.Errorf("waiting for raw actor close: %w", err)
			}
			if cinfo.Code != want {
				return fmt.Errorf("close_code = %d, want %d", cinfo.Code, want)
			}
		}
	}

	// send_window: deliberately not asserted (docs/CONFORMANCE.md section 1.3).
	return nil
}

func waitRawClose(ra *rawactor.Actor, timeout time.Duration) (*rawactor.CloseInfo, error) {
	deadline := time.Now().Add(timeout)
	for {
		if c, ok := ra.Closed(); ok {
			return c, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out after %s", timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
