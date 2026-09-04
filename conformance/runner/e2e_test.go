// End-to-end test of the conformance driver against real adapter binaries
// (docs/CONFORMANCE.md's "go test ./conformance/runner/... -run
// TestConformance" hook, section 5). The spec repo ships no SDK adapters of
// its own (they live with each SDK's repo now, per docs/CONFORMANCE.md
// section 6's "run" shim contract), so this test is driven entirely by
// optional environment variables naming already-built adapter shims:
//
//	WSMIXER_E2E_GO_ADAPTER=/path/to/go/run
//	WSMIXER_E2E_JS_ADAPTER=/path/to/js/run
//
// With neither set (the default -- e.g. a bare `go test ./...` in this
// repo) the whole test skips cleanly. An SDK repo's own CI (or a human
// pointing this at a locally built adapter) can set one or both to actually
// exercise the driver end-to-end against a real adapter.
package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mcpwarp/ws-mixer-spec/conformance/runner/adapter"
	"github.com/mcpwarp/ws-mixer-spec/conformance/runner/driver"
	"github.com/mcpwarp/ws-mixer-spec/conformance/runner/fixture"
)

func TestConformanceE2E(t *testing.T) {
	goPath := os.Getenv("WSMIXER_E2E_GO_ADAPTER")
	jsPath := os.Getenv("WSMIXER_E2E_JS_ADAPTER")
	if goPath == "" && jsPath == "" {
		t.Skip("WSMIXER_E2E_GO_ADAPTER / WSMIXER_E2E_JS_ADAPTER not set; this repo ships no adapters of its own to build (see file comment)")
	}

	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}

	sd, err := driver.LoadStepDriving(filepath.Join(repoRoot, "conformance", "scenarios", "step-driving.json"))
	if err != nil {
		t.Fatal(err)
	}
	timing := driver.Timing{Scale: 1.0 / 50}

	names := []string{"happy_handshake", "app_message_roundtrip", "credit_violation", "max_streams_exceeded"}
	fixturesDir := filepath.Join(repoRoot, "spec", "fixtures", "sequences")
	for _, name := range names {
		f, err := fixture.Load(filepath.Join(fixturesDir, name+".json"))
		if err != nil {
			t.Fatalf("loading fixture %s: %v", name, err)
		}
		if goPath != "" {
			t.Run("go/"+name, func(t *testing.T) {
				runFixtureForTest(t, f, "go", goPath, timing, sd)
			})
		}
		if jsPath != "" && f.Role == "client" {
			t.Run("js/"+name, func(t *testing.T) {
				runFixtureForTest(t, f, "js", jsPath, timing, sd)
			})
		}
	}

	if goPath != "" {
		t.Run("pair/happy_roundtrip/go-go", func(t *testing.T) {
			runPairForTest(t, repoRoot, "happy_roundtrip", "go", goPath, "go", goPath, timing)
		})
	}
	if goPath != "" && jsPath != "" {
		t.Run("pair/happy_roundtrip/go-js", func(t *testing.T) {
			runPairForTest(t, repoRoot, "happy_roundtrip", "go", goPath, "js", jsPath, timing)
		})
	}
}

func runFixtureForTest(t *testing.T, f *fixture.Fixture, sdkName, path string, timing driver.Timing, sd *driver.StepDriving) {
	t.Helper()
	a, err := adapter.Spawn(sdkName, path)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer a.Kill()

	ready, err := a.WaitForTimeout(func(e adapter.Event) bool { return e.Event == "ready" }, 10*time.Second)
	if err != nil {
		t.Fatalf("waiting for ready: %v", err)
	}
	if !rolesInclude(ready, f.Role) {
		t.Skipf("adapter %s does not support role %q", sdkName, f.Role)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res := driver.RunFixture(ctx, f, a, timing, sd)
	if res.Status != "PASS" {
		t.Fatalf("%s: %s\n%s", res.Status, res.Message, res.Detail)
	}
}

func runPairForTest(t *testing.T, repoRoot, scenarioName, srvName, srvPath, cliName, cliPath string, timing driver.Timing) {
	t.Helper()
	scenarios, err := driver.LoadScenarios(filepath.Join(repoRoot, "conformance", "scenarios"))
	if err != nil {
		t.Fatal(err)
	}
	var s *driver.Scenario
	for _, sc := range scenarios {
		if sc.Name == scenarioName {
			s = sc
		}
	}
	if s == nil {
		t.Fatalf("scenario %s not found", scenarioName)
	}

	srv, err := adapter.Spawn(srvName, srvPath)
	if err != nil {
		t.Fatalf("spawn server: %v", err)
	}
	defer srv.Kill()
	cli, err := adapter.Spawn(cliName, cliPath)
	if err != nil {
		t.Fatalf("spawn client: %v", err)
	}
	defer cli.Kill()

	if _, err := srv.WaitForTimeout(func(e adapter.Event) bool { return e.Event == "ready" }, 10*time.Second); err != nil {
		t.Fatalf("server never ready: %v", err)
	}
	if _, err := cli.WaitForTimeout(func(e adapter.Event) bool { return e.Event == "ready" }, 10*time.Second); err != nil {
		t.Fatalf("client never ready: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res := driver.RunPair(ctx, s, srv, cli, timing, srvName+"->"+cliName)
	if res.Status != "PASS" {
		t.Fatalf("%s: %s\n%s", res.Status, res.Message, res.Detail)
	}
}

// TestMain is docs/CONFORMANCE.md section 5's "thin TestMain wrapper so `go
// test ./...` from the repo root includes it" -- this is that wrapper for
// this module's own suite; nothing special beyond the default runner is
// needed today, but it documents the intended hook shape.
func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
