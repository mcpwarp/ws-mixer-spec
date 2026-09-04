package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mcpwarp/ws-mixer-spec/conformance/runner/adapter"
	"github.com/mcpwarp/ws-mixer-spec/conformance/runner/codes"
	"github.com/mcpwarp/ws-mixer-spec/conformance/runner/fixture"
	"github.com/mcpwarp/ws-mixer-spec/conformance/runner/rawactor"
	"github.com/mcpwarp/ws-mixer-spec/conformance/runner/wire"
)

// ObservedError carries the raw actor's own view of an error{} control frame
// already consumed by an earlier `send` step of type "error" -- several
// fixtures (credit_violation, ...) script that observe explicitly before a
// later `expect error_code` step, so checkExpect must reuse this rather than
// waiting for a second error{} frame that will never arrive (rawactor.Expect
// only ever matches a given observation once).
type ObservedError struct {
	Have    bool
	Code    uint32
	Name    string
	Message string
	At      time.Time
}

// decodeControl decodes a stream-0 DATA observation's JSON payload, or nil
// if obs isn't a frame (a WS close, or a raw decode failure).
func decodeControl(o rawactor.Observation) map[string]any {
	if o.Frame == nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(o.Frame.Payload, &m) != nil {
		return nil
	}
	return m
}

// defaultTimeout bounds a command's round trip (spawn a process, write JSON,
// get an ack, observe the resulting bytes on the wire) -- real wall-clock
// overhead unrelated to --time-scale, so it stays a generous constant
// instead of being scaled down with the rest of the fixture's timers.
const defaultTimeout = 20 * time.Second

// driveSend classifies and drives one `send` step per docs/CONFORMANCE.md
// section 3.3's table, then confirms the raw actor observed the resulting
// bytes on the wire.
func driveSend(
	ctx context.Context,
	ad *adapter.Adapter,
	ra *rawactor.Actor,
	role string,
	payload map[string]any,
	stepIndex int,
	sd *StepDriving,
	fixtureName string,
	timing Timing,
	lastStreamID *uint32,
	lastResetName *string,
	lastError *ObservedError,
) error {
	if fixture.IsFrame(payload) {
		return driveSendFrame(ad, ra, payload, stepIndex, sd, fixtureName, timing, lastStreamID, lastResetName)
	}
	return driveSendControl(ad, ra, payload, lastError, timing)
}

func driveSendFrame(
	ad *adapter.Adapter,
	ra *rawactor.Actor,
	payload map[string]any,
	stepIndex int,
	sd *StepDriving,
	fixtureName string,
	timing Timing,
	lastStreamID *uint32,
	lastResetName *string,
) error {
	typ := fixture.FrameType(payload)
	sid := fixture.StreamID(payload)

	switch typ {
	case "OPEN":
		seq, err := ad.SendCmd("open_stream", nil)
		if err != nil {
			return err
		}
		ev, err := ad.WaitForTimeout(func(e adapter.Event) bool {
			return (e.Event == "stream_opened" || e.Event == "error") && e.Seq == seq
		}, defaultTimeout)
		if err != nil {
			return fmt.Errorf("open_stream: %w", err)
		}
		if ev.Event == "error" {
			if b, ok := ev.Raw["unsupported"].(bool); (ok && b) || ev.Raw["error"] == "unsupported" {
				return fmt.Errorf("open_stream: %w: %s", adapter.ErrUnsupported, ev.String("message"))
			}
			return fmt.Errorf("open_stream failed: %s", ev.String("message"))
		}
		gotID, _ := ev.Int("id")
		if sid != 0 && uint32(gotID) != sid {
			return fmt.Errorf("open_stream allocated id %d, fixture expects %d", gotID, sid)
		}
		*lastStreamID = uint32(gotID)
		_, err = ra.Expect(func(o rawactor.Observation) bool {
			return o.Frame != nil && o.Frame.Type == wire.TypeOpen && o.Frame.StreamID == uint32(gotID)
		}, defaultTimeout)
		return err

	case "DATA":
		data, err := payloadBytes(payload)
		if err != nil {
			return err
		}
		if _, err := ad.SendAndAck("write", map[string]any{"id": sid, "data_b64": b64(data)}, defaultTimeout); err != nil {
			return err
		}
		*lastStreamID = sid
		_, err = ra.Expect(func(o rawactor.Observation) bool {
			if o.Frame == nil || o.Frame.Type != wire.TypeData || o.Frame.StreamID != sid {
				return false
			}
			if hexStr, ok := payload["payload_hex"].(string); ok {
				want, _ := hexDecode(hexStr)
				return bytesEqual(o.Frame.Payload, want)
			}
			if n, ok := payload["payload_length"].(float64); ok {
				return len(o.Frame.Payload) == int(n)
			}
			return true
		}, defaultTimeout)
		return err

	case "CLOSE":
		if _, err := ad.SendAndAck("close_write", map[string]any{"id": sid}, defaultTimeout); err != nil {
			return err
		}
		*lastStreamID = sid
		_, err := ra.Expect(func(o rawactor.Observation) bool {
			return o.Frame != nil && o.Frame.Type == wire.TypeClose && o.Frame.StreamID == sid
		}, defaultTimeout)
		return err

	case "RESET":
		code, _ := payload["code"].(float64)
		msg, _ := payload["message"].(string)
		override := sd.Override(fixtureName, stepIndex)
		autonomous := override == "auto" ||
			(override == "" && (uint32(code) == 0x05 || uint32(code) == 0x08))
		if !autonomous {
			if _, err := ad.SendAndAck("reset", map[string]any{"id": sid, "code": int(code), "message": msg}, defaultTimeout); err != nil {
				return err
			}
		}
		*lastStreamID = sid
		obs, err := ra.Expect(func(o rawactor.Observation) bool {
			return o.Frame != nil && o.Frame.Type == wire.TypeReset && o.Frame.StreamID == sid && wire.ResetCode(o.Frame) == uint32(code)
		}, defaultTimeout)
		if err != nil {
			return err
		}
		// stream_reset_code (docs/CONFORMANCE.md section 4) is asserted from
		// this observed wire frame's own code, never the fixture's -- the
		// match predicate above already proved the two agree, so this is
		// just naming the value the raw actor actually saw.
		*lastResetName = codes.Name(wire.ResetCode(obs.Frame))
		return nil

	case "WINDOW":
		// No command issues WINDOW directly (docs/CONFORMANCE.md section 1.1's
		// 12 commands has none) -- it is always a side effect of the SDK's own
		// flow-control bookkeeping, so this is always observe-only.
		*lastStreamID = sid
		obs, err := ra.Expect(func(o rawactor.Observation) bool {
			return o.Frame != nil && o.Frame.Type == wire.TypeWindow && o.Frame.StreamID == sid
		}, defaultTimeout)
		if err != nil {
			return err
		}
		if inc, ok := payload["increment"].(float64); ok {
			got := wire.WindowIncrement(obs.Frame)
			if got != uint32(inc) {
				return fmt.Errorf("WINDOW increment = %d, want %d", got, uint32(inc))
			}
		}
		return nil

	default:
		return fmt.Errorf("unknown frame type %q in send step", typ)
	}
}

// diagnoseTimeout enriches a bare "timed out waiting for a matching
// observation" error with the adapter's own recent events -- an observe-only
// step that never sees its expected frame is otherwise indistinguishable
// from "the SDK is just slow" and "the SDK silently accepted something it
// should have rejected" (e.g. a disabled credit check never emitting the
// error{} a violation should have produced): the adapter's event log tells
// which one it was.
func diagnoseTimeout(ad *adapter.Adapter, what string, err error) error {
	if err == nil {
		return nil
	}
	events := ad.Events()
	n := len(events)
	tail := events
	if n > 5 {
		tail = events[n-5:]
	}
	var raw []map[string]any
	for _, e := range tail {
		raw = append(raw, e.Raw)
	}
	return fmt.Errorf("%s: %w (no protocol-level reaction observed; adapter's own last events: %+v)", what, err, raw)
}

func driveSendControl(ad *adapter.Adapter, ra *rawactor.Actor, payload map[string]any, lastError *ObservedError, timing Timing) error {
	t := fixture.ControlType(payload)
	switch t {
	case "hello", "welcome":
		// Observe only (docs/CONFORMANCE.md section 3.3): produced by the
		// handshake, never a command.
		_, err := ra.Expect(func(o rawactor.Observation) bool { return isControl(o, t) }, defaultTimeout)
		return diagnoseTimeout(ad, "waiting for "+t, err)
	case "ping", "pong":
		// Observe only: produced by the SDK's own keepalive loop. Content
		// check: `id` must round-trip (OVERVIEW.md section 2.10).
		obs, err := ra.Expect(func(o rawactor.Observation) bool { return isControl(o, t) }, defaultTimeout)
		if err != nil {
			return diagnoseTimeout(ad, "waiting for "+t, err)
		}
		if want, ok := payload["id"]; ok {
			m := decodeControl(obs)
			if got := m["id"]; got != want {
				return fmt.Errorf("%s.id = %v, want %v", t, got, want)
			}
		}
		return nil
	case "error":
		// Observe only: produced by the SDK's own fail path. Content check:
		// `code` must match the fixture, `message` must be non-empty
		// (docs/CONFORMANCE.md section 4's error_code mapping applies the
		// same way to an autonomously-emitted error{} as to one asserted via
		// expect.error_code).
		obs, err := ra.Expect(func(o rawactor.Observation) bool { return isControl(o, t) }, defaultTimeout)
		if err != nil {
			return diagnoseTimeout(ad, "waiting for error{} (a check the SDK-under-test may have skipped)", err)
		}
		m := decodeControl(obs)
		gotCodeF, _ := m["code"].(float64)
		gotName, _ := m["name"].(string)
		msg, _ := m["message"].(string)
		if wantCode, ok := payload["code"].(float64); ok && gotCodeF != wantCode {
			return fmt.Errorf("error.code = %v, want %v", gotCodeF, wantCode)
		}
		if msg == "" {
			return fmt.Errorf("error.message must be non-empty")
		}
		*lastError = ObservedError{Have: true, Code: uint32(gotCodeF), Name: gotName, Message: msg, At: obs.At}
		return nil
	case "drain":
		args := map[string]any{}
		if reason, ok := payload["reason"].(string); ok {
			args["reason"] = reason
		}
		var wantDeadline int64
		haveDeadline := false
		if dl, ok := payload["deadline_ms"].(float64); ok {
			raw := int64(dl)
			args["deadline_ms"] = raw // sent unscaled; the adapter itself scales by time_scale (set_options) before putting it on the wire
			wantDeadline = timing.ScaleMs(raw)
			haveDeadline = true
		}
		if _, err := ad.SendAndAck("drain", args, defaultTimeout); err != nil {
			return err
		}
		obs, err := ra.Expect(func(o rawactor.Observation) bool { return isControl(o, "drain") }, defaultTimeout)
		if err != nil {
			return err
		}
		m := decodeControl(obs)
		if reason, ok := args["reason"]; ok {
			if got, _ := m["reason"].(string); got != reason {
				return fmt.Errorf("drain.reason = %q, want %q", got, reason)
			}
		}
		if haveDeadline {
			gotF, _ := m["deadline_ms"].(float64)
			if int64(gotF) != wantDeadline {
				return fmt.Errorf("drain.deadline_ms = %v, want %v", gotF, wantDeadline)
			}
		}
		if want, ok := payload["last_stream_id"].(float64); ok {
			got, _ := m["last_stream_id"].(float64)
			if got != want {
				return fmt.Errorf("drain.last_stream_id = %v, want %v", got, want)
			}
		}
		return nil
	case "app":
		if _, err := ad.SendAndAck("send_app", map[string]any{"body": payload["body"]}, defaultTimeout); err != nil {
			return err
		}
		obs, err := ra.Expect(func(o rawactor.Observation) bool { return isControl(o, "app") }, defaultTimeout)
		if err != nil {
			return err
		}
		m := decodeControl(obs)
		if !jsonEqual(m["body"], payload["body"]) {
			return fmt.Errorf("app.body = %#v, want %#v", m["body"], payload["body"])
		}
		return nil
	default:
		return fmt.Errorf("unhandled control type %q in send step", t)
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
