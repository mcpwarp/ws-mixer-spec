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
