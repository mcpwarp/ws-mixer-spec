package adapter

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// buildFakeAdapter compiles the fixture Go program under testdata/fakeadapter
// into a standalone binary in a temp dir. It is a minimal, hand-written
// adapter that speaks exactly the protocol in docs/CONFORMANCE.md section 1
// (ready, ack, a canned "connected", echoes send_app back as "app") -- enough
// to exercise Spawn/Send/WaitFor/SendAndAck/Kill without needing a real SDK.
func buildFakeAdapter(t *testing.T) string {
	t.Helper()
	goBin := os.Getenv("GO")
	if goBin == "" {
		goBin = "go"
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "fakeadapter")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	src, err := filepath.Abs(filepath.Join("testdata", "fakeadapter"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(goBin, "build", "-o", out, ".")
	cmd.Dir = src
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building fake adapter: %v\n%s", err, b)
	}
	return out
}

func TestSpawnAndProtocol(t *testing.T) {
	bin := buildFakeAdapter(t)
	a, err := Spawn("fake", bin)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Kill()

	ready, err := a.WaitForTimeout(func(e Event) bool { return e.Event == "ready" }, 5*time.Second)
	if err != nil {
		t.Fatalf("waiting for ready: %v", err)
	}
	if ready.String("sdk") != "fake-sdk" {
		t.Errorf("ready.sdk = %q, want fake-sdk", ready.String("sdk"))
	}

	if _, err := a.SendAndAck("set_options", map[string]any{"window": 262144}, 5*time.Second); err != nil {
		t.Fatalf("set_options: %v", err)
	}

	seq, err := a.SendCmd("connect", map[string]any{"url": "ws://example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	ev, err := a.WaitForTimeout(func(e Event) bool { return e.Event == "connected" && e.Seq == seq }, 5*time.Second)
	if err != nil {
		t.Fatalf("waiting for connected: %v", err)
	}
	if ev.Raw["session"] != "fake-session" {
		t.Errorf("session = %v, want fake-session", ev.Raw["session"])
	}

	// send_app roundtrip, and _b64 field decoding.
	if _, err := a.SendCmd("send_app", map[string]any{"body": map[string]any{"hello": "world"}}); err != nil {
		t.Fatal(err)
	}
	appEv, err := a.WaitForTimeout(func(e Event) bool { return e.Event == "app" }, 5*time.Second)
	if err != nil {
		t.Fatalf("waiting for app echo: %v", err)
	}
	if body, _ := appEv.Raw["body"].(map[string]any); body["hello"] != "world" {
		t.Errorf("app.body = %v, want hello:world", appEv.Raw["body"])
	}

	if _, err := a.SendCmd("write", map[string]any{"id": 1, "data_b64": base64.StdEncoding.EncodeToString([]byte("hi"))}); err != nil {
		t.Fatal(err)
	}
	dataEv, err := a.WaitForTimeout(func(e Event) bool { return e.Event == "data" }, 5*time.Second)
	if err != nil {
		t.Fatalf("waiting for data echo: %v", err)
	}
	gotB64, _ := dataEv.Raw["data_b64"].(string)
	b, err := base64.StdEncoding.DecodeString(gotB64)
	if err != nil || string(b) != "hi" {
		t.Errorf("data_b64 decode = %q err=%v, want \"hi\"", b, err)
	}

	// An unrecognized command's error{seq} must be reported as an error from SendAndAck.
	if _, err := a.SendAndAck("bogus_command", nil, 5*time.Second); err == nil {
		t.Error("expected SendAndAck to fail for an unknown command")
	}
}

// TestWaitForTimeoutExpires checks that WaitFor gives up (rather than hanging
// forever) when the predicate never matches and the process is still alive.
func TestWaitForTimeoutExpires(t *testing.T) {
	bin := buildFakeAdapter(t)
	a, err := Spawn("fake", bin)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Kill()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err = a.WaitFor(ctx, func(Event) bool { return false })
	if err == nil {
		t.Fatal("expected an error")
	}
}

// TestWaitAbsentTimeoutPasses checks the common case a pair scenario's
// await_none step (docs/CONFORMANCE.md section 3.2, D-2026-09-20-07's
// conformance hook) relies on: nothing matching ever arrives, and the call
// returns (Event{}, false) once its window elapses instead of hanging or
// misreporting a match.
func TestWaitAbsentTimeoutPasses(t *testing.T) {
	bin := buildFakeAdapter(t)
	a, err := Spawn("fake", bin)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Kill()
	if _, err := a.WaitForTimeout(func(e Event) bool { return e.Event == "ready" }, 5*time.Second); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	ev, found, err := a.WaitAbsentTimeout(func(e Event) bool { return e.Event == "reconnected" }, 200*time.Millisecond)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("expected no error (adapter stayed alive the whole window), got %v", err)
	}
	if found {
		t.Fatalf("expected no match, got %+v", ev.Raw)
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("returned after %v, want to have waited out the full window", elapsed)
	}
}

// TestWaitAbsentTimeoutCatchesAlreadyLoggedEvent checks the case the
// application_close_client.json blocker (Opus re-review) was actually about:
// a matching event that is ALREADY sitting, unconsumed, in the log before
// WaitAbsentTimeout is even called -- e.g. an initial "connected" a scenario
// never awaited -- must be found immediately (well under the window), not
// only a genuinely-delayed one. WaitAbsentTimeout scans the whole log from
// index 0, skipping only consumed events, same as WaitFor; that is
// deliberate (a redial that raced ahead of the step must still count), so
// this test pins down that a scenario step must consume anything it doesn't
// want a later await_none to trip over.
func TestWaitAbsentTimeoutCatchesAlreadyLoggedEvent(t *testing.T) {
	bin := buildFakeAdapter(t)
	a, err := Spawn("fake", bin)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Kill()
	if _, err := a.WaitForTimeout(func(e Event) bool { return e.Event == "ready" }, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	// fakeadapter emits "connected" as soon as connect() is sent -- consume
	// nothing, so it stays in the log for WaitAbsentTimeout to find.
	if _, err := a.SendCmd("connect", map[string]any{"url": "ws://example.invalid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.WaitForTimeout(func(e Event) bool { return e.Event == "connected" }, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	// Re-issue connect so a SECOND, still-unconsumed "connected" is in the
	// log for WaitAbsentTimeout to find (the first WaitFor above already
	// consumed the first one, mirroring RunPair's own setup consuming the
	// client's initial connected but not the server's).
	if _, err := a.SendCmd("connect", map[string]any{"url": "ws://example.invalid"}); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	ev, found, err := a.WaitAbsentTimeout(func(e Event) bool { return e.Event == "connected" }, 2*time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("expected no error (a match was found, not a dead adapter), got %v", err)
	}
	if !found {
		t.Fatal("expected the already-logged connected event to be found")
	}
	if ev.Event != "connected" {
		t.Errorf("matched event = %+v, want event=connected", ev.Raw)
	}
	if elapsed > time.Second {
		t.Errorf("returned after %v, want near-immediate (event was already logged, not delayed)", elapsed)
	}
}

// TestWaitAbsentTimeoutCatchesDelayedEvent checks the failure case: an event
// matching the predicate arrives asynchronously, mid-window (via
// fakeadapter's test-only emit_after), not one already sitting in the log —
// this is the case a scenario's negative assertion exists to catch (an SDK
// that keeps reconnecting after an application-close).
func TestWaitAbsentTimeoutCatchesDelayedEvent(t *testing.T) {
	bin := buildFakeAdapter(t)
	a, err := Spawn("fake", bin)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Kill()
	if _, err := a.WaitForTimeout(func(e Event) bool { return e.Event == "ready" }, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SendCmd("emit_after", map[string]any{"event": "reconnected", "delay_ms": 50}); err != nil {
		t.Fatal(err)
	}

	ev, found, err := a.WaitAbsentTimeout(func(e Event) bool { return e.Event == "reconnected" }, 2*time.Second)
	if err != nil {
		t.Fatalf("expected no error (a match was found, not a dead adapter), got %v", err)
	}
	if !found {
		t.Fatal("expected the delayed reconnected event to be observed as a match")
	}
	if ev.Event != "reconnected" {
		t.Errorf("matched event = %+v, want event=reconnected", ev.Raw)
	}
}

// TestWaitAbsentTimeoutDoesNotConsume checks that a passing (no-match)
// WaitAbsentTimeout call never marks anything consumed, so it can't swallow
// an event a later WaitFor step in the same scenario still needs — the
// property docs/CONFORMANCE.md section 3.2 requires of await_none.
func TestWaitAbsentTimeoutDoesNotConsume(t *testing.T) {
	bin := buildFakeAdapter(t)
	a, err := Spawn("fake", bin)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Kill()
	if _, err := a.WaitForTimeout(func(e Event) bool { return e.Event == "ready" }, 5*time.Second); err != nil {
		t.Fatal(err)
	}

	// An event (app, from send_app) is already logged before the
	// WaitAbsentTimeout call below runs.
	if _, err := a.SendAndAck("send_app", map[string]any{"body": map[string]any{"k": "v"}}, 5*time.Second); err != nil {
		t.Fatal(err)
	}

	// Watch for something that never comes.
	if _, found, err := a.WaitAbsentTimeout(func(e Event) bool { return e.Event == "reconnected" }, 150*time.Millisecond); found || err != nil {
		t.Fatalf("expected no match and no error, got found=%v err=%v", found, err)
	}

	// The earlier "app" event must still be there for a later WaitFor.
	if _, err := a.WaitForTimeout(func(e Event) bool { return e.Event == "app" }, time.Second); err != nil {
		t.Fatalf("app event should still be available to WaitFor after an unrelated WaitAbsentTimeout: %v", err)
	}
}

// TestWaitAbsentTimeoutFailsIfAdapterDiesDuringWindow checks the fix for a
// cross-repo review finding: an adapter that exits partway through an
// await_none window must FAIL that step, not PASS it. Before this, a dead
// process and "genuinely nothing happened" were indistinguishable --
// application_close_client.json's two await_none steps are its LAST steps,
// so an SDK that crashed right after emitting `disconnected` would have
// gotten a trivial PASS instead of the FAIL a crash deserves.
func TestWaitAbsentTimeoutFailsIfAdapterDiesDuringWindow(t *testing.T) {
	bin := buildFakeAdapter(t)
	a, err := Spawn("fake", bin)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Kill()
	if _, err := a.WaitForTimeout(func(e Event) bool { return e.Event == "ready" }, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = a.SendCmd("shutdown", nil)
	}()

	start := time.Now()
	_, found, err := a.WaitAbsentTimeout(func(e Event) bool { return e.Event == "reconnected" }, 2*time.Second)
	elapsed := time.Since(start)
	if found {
		t.Fatal("expected found=false: no matching event was ever emitted, only the adapter dying")
	}
	if err == nil {
		t.Fatal("expected an error: the adapter died mid-window, so the negative assertion was never actually verified")
	}
	if elapsed > time.Second {
		t.Errorf("returned after %v, want to detect the dead adapter promptly rather than waiting out the full 2s window", elapsed)
	}
}

// TestWaitAbsentTimeoutFailsIfAdapterAlreadyDead is the other half: the
// adapter is already gone before the await_none step even starts.
func TestWaitAbsentTimeoutFailsIfAdapterAlreadyDead(t *testing.T) {
	bin := buildFakeAdapter(t)
	a, err := Spawn("fake", bin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.WaitForTimeout(func(e Event) bool { return e.Event == "ready" }, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	a.Kill() // process is gone -- a.closed is already true before WaitAbsentTimeout is ever called

	start := time.Now()
	_, found, err := a.WaitAbsentTimeout(func(e Event) bool { return e.Event == "reconnected" }, 2*time.Second)
	elapsed := time.Since(start)
	if found {
		t.Fatal("expected found=false")
	}
	if err == nil {
		t.Fatal("expected an error: the adapter was already dead before the step started")
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("returned after %v, want near-immediate (adapter already closed)", elapsed)
	}
}

// TestKillLeavesNoProcess exercises the process-group teardown path end to
// end: after Kill, the process must have exited.
func TestKillLeavesNoProcess(t *testing.T) {
	bin := buildFakeAdapter(t)
	a, err := Spawn("fake", bin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.WaitForTimeout(func(e Event) bool { return e.Event == "ready" }, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	a.Kill()
	// cmd.ProcessState is set once Wait() (called inside Kill) returns.
	if a.cmd.ProcessState == nil {
		t.Error("expected ProcessState to be set after Kill")
	}
}
