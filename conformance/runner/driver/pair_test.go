package driver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mcpwarp/ws-mixer-spec/conformance/runner/adapter"
)

// TestScenarioParsesAwaitNone checks the await_none/within_ms step shape
// (docs/CONFORMANCE.md section 3.2, added for D-2026-09-20-07's conformance
// hook) round-trips through Scenario's json tags the same way Cmd/Await
// already do, including the default-window case where within_ms is omitted.
func TestScenarioParsesAwaitNone(t *testing.T) {
	raw := []byte(`{
		"description": "test",
		"connect": {"reconnect": {"enabled": true}},
		"steps": [
			{"actor": "client", "cmd": {"cmd": "close", "code": 14, "message": "bye"}},
			{"actor": "client", "await": {"event": "disconnected", "ws_code": 4014}},
			{"actor": "client", "await_none": {"event": "reconnected"}, "within_ms": 1500},
			{"actor": "server", "await_none": {"event": "connected"}}
		]
	}`)
	var s Scenario
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(s.Steps) != 4 {
		t.Fatalf("len(Steps) = %d, want 4", len(s.Steps))
	}
	if s.Steps[0].Cmd == nil || s.Steps[0].Await != nil || s.Steps[0].AwaitNone != nil {
		t.Errorf("step 0 should be cmd-only, got %+v", s.Steps[0])
	}
	if s.Steps[1].Await == nil {
		t.Errorf("step 1 should carry an await")
	}
	step2 := s.Steps[2]
	if step2.AwaitNone == nil || step2.AwaitNone["event"] != "reconnected" {
		t.Errorf("step 2 await_none = %+v, want event:reconnected", step2.AwaitNone)
	}
	if step2.WithinMs == nil || *step2.WithinMs != 1500 {
		t.Errorf("step 2 within_ms = %v, want *1500", step2.WithinMs)
	}
	step3 := s.Steps[3]
	if step3.AwaitNone == nil || step3.AwaitNone["event"] != "connected" {
		t.Errorf("step 3 await_none = %+v, want event:connected", step3.AwaitNone)
	}
	// within_ms is a *int precisely so this case (the JSON field omitted
	// entirely) is distinguishable from an explicit "within_ms": 0 -- both
	// would collapse to the zero value of a plain int, but only "absent"
	// should fall back to defaultAwaitNoneMs; an explicit 0 is a
	// validateScenario error (see TestValidateScenarioRejectsAmbiguousStep).
	if step3.WithinMs != nil {
		t.Errorf("step 3 within_ms = %v, want nil (absent -> RunPair falls back to defaultAwaitNoneMs=%d)", *step3.WithinMs, defaultAwaitNoneMs)
	}
}

// TestEventMatchesForAwaitNone exercises eventMatches directly against the
// shapes an await_none predicate is built from in RunPair, independent of
// spawning any adapter process.
func TestEventMatchesForAwaitNone(t *testing.T) {
	reconnected := adapter.Event{Event: "reconnected", Raw: map[string]any{"event": "reconnected", "session": "s2"}}
	data := adapter.Event{Event: "data", Raw: map[string]any{"event": "data", "id": float64(1)}}
	connected := adapter.Event{Event: "connected", Raw: map[string]any{"event": "connected"}}

	if !eventMatches(reconnected, map[string]any{"event": "reconnected"}) {
		t.Error("reconnected event should match an await_none{event:reconnected} predicate")
	}
	if eventMatches(data, map[string]any{"event": "reconnected"}) {
		t.Error("an unrelated event must not match")
	}
	if !eventMatches(connected, map[string]any{"event": "connected"}) {
		t.Error("connected event should match an await_none{event:connected} predicate")
	}
	// The "reconnected" also satisfies "connected" widening (see eventMatches'
	// own comment) applies here too, since await_none reuses the same
	// matcher -- a server-role await_none{event:"connected"} is meant to
	// catch this on the off chance an adapter conflates the two.
	if !eventMatches(reconnected, map[string]any{"event": "connected"}) {
		t.Error("eventMatches should still widen connected<-reconnected for await_none predicates built the same way await ones are")
	}
	// The widening is one-directional (Opus re-review item 1): wanting
	// "reconnected" must NOT be satisfied by a plain "connected" event, or
	// application_close_client.json's client-side
	// await_none{event:"reconnected"} could never fail against an SDK that
	// silently dialed a brand-new (not reconnect-loop) connection and
	// reported it as "connected" instead of "reconnected".
	if eventMatches(connected, map[string]any{"event": "reconnected"}) {
		t.Error("eventMatches must not widen the other way: wanting reconnected must not match a plain connected event")
	}
}

// TestValidateScenarioRejectsAmbiguousStep checks each of the shapes
// RunPair's first-match if-chain would otherwise silently mishandle (Opus
// re-review item 3): a step must carry exactly one of cmd/await/await_none,
// and within_ms is only meaningful -- and only bounded -- alongside
// await_none. Every existing scenario has exactly one of the three per step
// already (checked by grep before adding this validation), so none of this
// tightens anything that was previously load-bearing.
func TestValidateScenarioRejectsAmbiguousStep(t *testing.T) {
	cases := []struct {
		name string
		step string
	}{
		{"cmd and await_none both set (the false-PASS case)", `{"actor":"client","cmd":{"cmd":"close","code":14},"await_none":{"event":"reconnected"}}`},
		{"await and await_none both set", `{"actor":"client","await":{"event":"connected"},"await_none":{"event":"reconnected"}}`},
		{"cmd and await both set", `{"actor":"client","cmd":{"cmd":"close","code":14},"await":{"event":"connected"}}`},
		{"neither cmd, await, nor await_none set", `{"actor":"client"}`},
		{"within_ms without await_none", `{"actor":"client","await":{"event":"connected"},"within_ms":500}`},
		{"within_ms:0 without await_none (cmd)", `{"actor":"client","cmd":{"cmd":"close","code":14},"within_ms":0}`},
		{"within_ms:0 alongside await_none (Review 3 item 2's blocker case: an explicit 0 must not silently become the 1500ms default)", `{"actor":"client","await_none":{"event":"reconnected"},"within_ms":0}`},
		{"within_ms negative", `{"actor":"client","await_none":{"event":"reconnected"},"within_ms":-1}`},
		{"within_ms exceeds the runner's per-step timeout", `{"actor":"client","await_none":{"event":"reconnected"},"within_ms":30000}`},
		{"within_ms absurdly large (Review 3 item 3's blocker case: must be rejected, not silently overflow past the ceiling check)", `{"actor":"client","await_none":{"event":"reconnected"},"within_ms":10000000000000}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s Scenario
			raw := `{"description":"test","steps":[` + tc.step + `]}`
			if err := json.Unmarshal([]byte(raw), &s); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if err := validateScenario(&s); err == nil {
				t.Errorf("expected validateScenario to reject %q, got nil", tc.step)
			}
		})
	}
}

// TestValidateScenarioAcceptsWellFormedSteps is the positive-case complement
// of the above: exactly-one-of-three steps, with and without a legal
// within_ms, must pass.
func TestValidateScenarioAcceptsWellFormedSteps(t *testing.T) {
	raw := `{"description":"test","steps":[
		{"actor":"client","cmd":{"cmd":"close","code":14}},
		{"actor":"client","await":{"event":"connected"}},
		{"actor":"client","await_none":{"event":"reconnected"}},
		{"actor":"client","await_none":{"event":"reconnected"},"within_ms":1500}
	]}`
	var s Scenario
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := validateScenario(&s); err != nil {
		t.Errorf("expected well-formed steps to pass validation, got %v", err)
	}
}

// TestLoadScenariosRealDir loads every conformance/scenarios/*.json this
// repo actually ships (relative to this package's own file, so it works
// regardless of the caller's working directory) and checks the count and
// step-driving.json exclusion -- the "make sure everything still loads"
// check the Opus re-review asked for, which a malformed step (e.g. the
// application_close_client.json blocker itself, had it been a *parse*
// failure rather than a runtime one) would have caught.
func TestLoadScenariosRealDir(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("..", "..", "scenarios"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	wantJSON := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			wantJSON++
		}
	}

	scenarios, err := LoadScenarios(dir)
	if err != nil {
		t.Fatalf("LoadScenarios(%s): %v", dir, err)
	}
	// step-driving.json is a sidecar, not a scenario -- excluded from the
	// returned list but still present on disk, so the counts differ by one.
	if wantJSON != len(scenarios)+1 {
		t.Errorf("found %d *.json files (excluding step-driving.json, %d scenarios), LoadScenarios returned %d",
			wantJSON, wantJSON-1, len(scenarios))
	}
	if len(scenarios) != 10 {
		t.Errorf("len(scenarios) = %d, want 10 (docs/CONFORMANCE.md section 3.2's scenario count)", len(scenarios))
	}
	names := map[string]bool{}
	for _, s := range scenarios {
		names[s.Name] = true
		if len(s.Steps) == 0 {
			t.Errorf("scenario %s has no steps", s.Name)
		}
	}
	for _, want := range []string{"application_close", "application_close_client", "drain_reconnect"} {
		if !names[want] {
			t.Errorf("expected scenario %q to be loaded, got %v", want, names)
		}
	}
	if names["step-driving"] {
		t.Error("step-driving.json should be excluded, not loaded as a scenario")
	}
}
