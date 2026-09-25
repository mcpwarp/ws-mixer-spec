package driver

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/mcpwarp/ws-mixer-spec/conformance/runner/adapter"
)

// isUnsupportedSkip checks err for adapter.ErrUnsupported (the coordination
// contract with conformance/adapters/**: an adapter replying
// {"ok":false,"error":"unsupported"} or unsupported:true to a command) and,
// if so, marks *res SKIP with a reason instead of leaving it to be reported
// as a FAIL. Returns whether it did.
func isUnsupportedSkip(res *Result, where string, err error) bool {
	if !errors.Is(err, adapter.ErrUnsupported) {
		return false
	}
	res.Status = "SKIP"
	res.Message = fmt.Sprintf("%s: %v", where, err)
	return true
}

// Scenario is one conformance/scenarios/*.json pair-mode file
// (docs/CONFORMANCE.md section 3.2).
type Scenario struct {
	Name        string
	Description string `json:"description"`
	// Connect carries extra fields merged into the client's "connect"
	// command args (e.g. {"reconnect":{"enabled":true}} for
	// drain_reconnect.json, docs/CONFORMANCE.md section 1.1) -- optional,
	// absent for every scenario that doesn't need non-default connect
	// behavior.
	Connect map[string]any `json:"connect,omitempty"`
	Steps   []struct {
		Actor string         `json:"actor"` // "server" | "client"
		Cmd   map[string]any `json:"cmd,omitempty"`
		Await map[string]any `json:"await,omitempty"`
		// AwaitNone is a negative assertion (docs/CONFORMANCE.md section 3.2):
		// the step passes if NO event matching it arrives on Actor within
		// WithinMs (default defaultAwaitNoneMs), and fails, naming the
		// offending event, if one does. Mutually exclusive with Cmd/Await.
		AwaitNone map[string]any `json:"await_none,omitempty"`
		// WithinMs is a pointer so an explicit `"within_ms": 0` in the JSON
		// (nonsensical -- a zero-width window is not a legal negative-assertion
		// wait, and it's not "absent" either) is distinguishable from the field
		// being omitted entirely: nil means absent (RunPair falls back to
		// defaultAwaitNoneMs), a non-nil *WithinMs <= 0 is a load-time error
		// (validateScenario), same as any other out-of-range value. A plain
		// `int` with `omitempty` could not tell "0" from "absent" and would
		// silently let an explicit 0 both skip validation (the "only alongside
		// await_none" rule) and fall through to the 1500ms default.
		WithinMs *int `json:"within_ms,omitempty"`
	} `json:"steps"`
}

// defaultAwaitNoneMs is the window an await_none step waits out when the
// scenario doesn't set within_ms -- long enough to catch a reconnect loop
// that fires on the SDK's own minimum backoff tick, short enough not to
// make a passing scenario slow. Not time-scaled: like connect.reconnect's
// baseMs/capMs (section 1.1), this bounds real wall-clock SDK-internal
// timing, not a wire-protocol duration --time-scale governs.
const defaultAwaitNoneMs = 1500

// LoadScenarios loads every *.json in dir except step-driving.json (which is
// fixture-mode's sidecar, not a scenario).
func LoadScenarios(dir string) ([]*Scenario, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" || e.Name() == "step-driving.json" {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	var out []*Scenario
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, err
		}
		var s Scenario
		if err := json.Unmarshal(b, &s); err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		s.Name = n[:len(n)-len(filepath.Ext(n))]
		if err := validateScenario(&s); err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		out = append(out, &s)
	}
	return out, nil
}

// validateScenario enforces the two step-shape rules docs/CONFORMANCE.md
// section 3.2 documents but the unmarshalled struct alone can't: exactly one
// of cmd/await/await_none per step (RunPair is a first-match if-chain, so a
// step carrying more than one would silently drop everything after the
// first match it hits -- e.g. an await_none combined with a cmd would never
// run its negative assertion at all, a false PASS), and within_ms only
// meaningful, and only bounded, alongside await_none. Every existing
// scenario has exactly one of cmd/await/await_none per step already (no
// scenario relies on a step carrying both cmd and await), so this tightens
// nothing that was previously load-bearing.
func validateScenario(s *Scenario) error {
	for i, st := range s.Steps {
		n := 0
		if st.Cmd != nil {
			n++
		}
		if st.Await != nil {
			n++
		}
		if st.AwaitNone != nil {
			n++
		}
		if n != 1 {
			return fmt.Errorf("step %d (actor %q): exactly one of cmd/await/await_none must be set, got %d", i, st.Actor, n)
		}
		if st.WithinMs != nil {
			if st.AwaitNone == nil {
				return fmt.Errorf("step %d (actor %q): within_ms is only valid alongside await_none", i, st.Actor)
			}
			ms := *st.WithinMs
			if ms <= 0 {
				return fmt.Errorf("step %d (actor %q): within_ms must be > 0, got %d", i, st.Actor, ms)
			}
			// Compare as plain ints, before any conversion to time.Duration:
			// an absurdly large within_ms (bigger than time.Duration's own
			// range in ms, or just bigger than int64 nanoseconds / 1e6) would
			// overflow int64 nanoseconds on the ms*time.Millisecond multiply
			// and could wrap negative, which would pass a ">" ceiling check
			// performed on the (already-overflowed) Duration and silently
			// turn the negative assertion into a near-instant no-op instead
			// of the rejection this check exists to give.
			if ceilingMs := int(defaultTimeout / time.Millisecond); ms > ceilingMs {
				return fmt.Errorf("step %d (actor %q): within_ms %dms exceeds the runner's per-step timeout (%v = %dms) -- an await_none can never legitimately need longer than one step is allowed to take", i, st.Actor, ms, defaultTimeout, ceilingMs)
			}
		}
	}
	return nil
}

// RunPair drives one scenario across two already-spawned, already-`ready`
// adapters: srv plays the server role (listen, open_stream, ...), cli plays
// the client role (connect). label is e.g. "go->js" for reporting.
func RunPair(ctx context.Context, s *Scenario, srv, cli *adapter.Adapter, timing Timing, label string) Result {
	res := Result{Mode: "pair", SDK: label, Name: s.Name}
	fail := func(format string, args ...any) Result {
		res.Status = "FAIL"
		res.Message = fmt.Sprintf(format, args...)
		res.Detail = "--- server stderr ---\n" + srv.Stderr() + "\n--- client stderr ---\n" + cli.Stderr()
		return res
	}

	// Unlike fixture mode, pair mode never needs a sub-floor ping_interval:
	// no scenario here waits out a real keepalive timeout, and OVERVIEW.md
	// section 2.9/2.10's ping_interval>=5000ms/ping_timeout>=2x floors are
	// also enforced by control_messages.go's wire-level welcome parser (not
	// just Conn.applyWelcome, which is as far as Options.AllowSubfloorTiming
	// reaches today -- see conformance/README.md's documented gap), so
	// keeping these at realistic, un-scaled values sidesteps a real client
	// SDK rejecting its peer's welcome outright. hello_timeout_ms and drain
	// deadline_ms have no such wire floor, so those stay scaled for a fast
	// test run.
	opts := map[string]any{
		"window": 262144, "max_streams": 64,
		"ping_interval_ms": 30000, "ping_timeout_ms": 90000,
		"hello_timeout_ms": timing.ScaleMs(10000), "time_scale": timing.Scale,
		"floor_ms":              timing.FloorMs(),
		"allow_subfloor_timing": true,
	}
	if _, err := srv.SendAndAck("set_options", opts, defaultTimeout); err != nil {
		if isUnsupportedSkip(&res, "server set_options", err) {
			return res
		}
		return fail("server set_options: %v", err)
	}
	if _, err := cli.SendAndAck("set_options", opts, defaultTimeout); err != nil {
		if isUnsupportedSkip(&res, "client set_options", err) {
			return res
		}
		return fail("client set_options: %v", err)
	}

	if _, err := srv.SendAndAck("listen", map[string]any{"addr": "127.0.0.1:0"}, defaultTimeout); err != nil {
		if isUnsupportedSkip(&res, "listen", err) {
			return res
		}
		return fail("listen: %v", err)
	}
	listening, err := srv.WaitForTimeout(func(e adapter.Event) bool { return e.Event == "listening" }, defaultTimeout)
	if err != nil {
		return fail("waiting for listening: %v", err)
	}
	url := listening.String("url")

	connectArgs := map[string]any{"url": url, "token": defaultToken}
	for k, v := range s.Connect {
		connectArgs[k] = v
	}
	if _, err := cli.SendAndAck("connect", connectArgs, defaultTimeout); err != nil {
		if isUnsupportedSkip(&res, "connect", err) {
			return res
		}
		return fail("connect: %v", err)
	}
	// "reconnected" (an adapter's own reconnect-after-drop event, per the
	// coordination contract with conformance/adapters/**) counts the same as
	// the initial "connected" here -- both mean "the client is now up".
	if _, err := cli.WaitForTimeout(func(e adapter.Event) bool { return e.Event == "connected" || e.Event == "reconnected" }, defaultTimeout); err != nil {
		return fail("waiting for client connected: %v", err)
	}

	for i, st := range s.Steps {
		target := srv
		if st.Actor == "client" {
			target = cli
		}
		if st.Cmd != nil {
			name, _ := st.Cmd["cmd"].(string)
			args := map[string]any{}
			for k, v := range st.Cmd {
				if k != "cmd" {
					args[k] = v
				}
			}
			if fill, ok := args["payload_fill"]; ok {
				delete(args, "payload_fill")
				b64, err := expandPayloadFill(fill)
				if err != nil {
					return fail("step %d (%s cmd %s): %v", i, st.Actor, name, err)
				}
				args["data_b64"] = b64
			}
			if _, err := target.SendAndAck(name, args, defaultTimeout); err != nil {
				if isUnsupportedSkip(&res, fmt.Sprintf("step %d (%s cmd %s)", i, st.Actor, name), err) {
					return res
				}
				return fail("step %d (%s cmd %s): %v", i, st.Actor, name, err)
			}
			continue
		}
		if st.Await != nil {
			ev, err := target.WaitForTimeout(func(e adapter.Event) bool { return eventMatches(e, st.Await) }, defaultTimeout)
			if err != nil {
				return fail("step %d (%s await %v): %v", i, st.Actor, st.Await, err)
			}
			_ = ev
			continue
		}
		if st.AwaitNone != nil {
			windowMs := defaultAwaitNoneMs
			if st.WithinMs != nil {
				windowMs = *st.WithinMs
			}
			window := time.Duration(windowMs) * time.Millisecond
			ev, found, err := target.WaitAbsentTimeout(func(e adapter.Event) bool { return eventMatches(e, st.AwaitNone) }, window)
			if err != nil {
				return fail("step %d (%s await_none %v): %v", i, st.Actor, st.AwaitNone, err)
			}
			if found {
				return fail("step %d (%s await_none %v): observed %+v within %v", i, st.Actor, st.AwaitNone, ev.Raw, window)
			}
			continue
		}
		return fail("step %d: neither cmd, await, nor await_none set", i)
	}

	res.Status = "PASS"
	return res
}

// expandPayloadFill turns a scenario's {"byte":"<hex>","count":N} directive
// (used instead of a literal data_b64 for a scenario that needs a large,
// content-insensitive payload, e.g. big_stream_flow_control.json's 1 MiB
// write -- checking in that many literal base64 bytes would bloat the repo
// for no assertion value, since these scenarios never inspect payload
// content) into the base64 string a `write` command expects.
func expandPayloadFill(v any) (string, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return "", fmt.Errorf("payload_fill must be an object")
	}
	hexByte, _ := m["byte"].(string)
	countF, _ := m["count"].(float64)
	if hexByte == "" || countF <= 0 {
		return "", fmt.Errorf("payload_fill needs a non-empty \"byte\" and positive \"count\"")
	}
	raw, err := hex.DecodeString(hexByte)
	if err != nil || len(raw) != 1 {
		return "", fmt.Errorf("payload_fill.byte must be exactly one hex-encoded byte, got %q", hexByte)
	}
	buf := make([]byte, int(countF))
	for i := range buf {
		buf[i] = raw[0]
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}

func eventMatches(e adapter.Event, want map[string]any) bool {
	for k, v := range want {
		if k == "event" {
			// "reconnected" satisfies an await on "connected" too (see the
			// comment above the initial-connect wait in RunPair) -- a
			// scenario written before drain_reconnect existed still means
			// "the client is up" either way.
			if e.Event != v && !(v == "connected" && e.Event == "reconnected") {
				return false
			}
			continue
		}
		if k == "code" {
			// Both adapters emit a numeric "code" alongside a friendly
			// string "name" (codes.ErrorCode.String() / codeNameOf) on
			// stream_reset/error events -- an await's "code" is written as
			// that string name (matching spec/fixtures/*'s convention),
			// checked against "name", not the numeric field.
			if s, ok := v.(string); ok {
				name, _ := e.Raw["name"].(string)
				if name != s {
					return false
				}
				continue
			}
		}
		got, ok := e.Raw[k]
		if !ok {
			return false
		}
		if !jsonEqual(got, v) {
			return false
		}
	}
	return true
}

func jsonEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}
