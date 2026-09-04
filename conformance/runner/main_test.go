package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mcpwarp/ws-mixer-spec/conformance/runner/driver"
)

func writeCounts(t *testing.T, floors map[string]int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "COUNTS.json")
	doc := struct {
		Floors map[string]int `json:"floors"`
	}{Floors: floors}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCheckCountsAllSkipIsNotAViolation covers docs/CONFORMANCE.md section
// 3.5's "a missing or non-executable adapter is a listed SKIP ... never a
// hard failure": a key whose every row is SKIP (adapter missing or
// unbuildable, e.g. `--sdk python` before an adapter exists) must not be
// reported as a floor violation.
func TestCheckCountsAllSkipIsNotAViolation(t *testing.T) {
	path := writeCounts(t, map[string]int{"python-client": 21})
	results := []driver.Result{
		{SDK: "python-client", Name: "happy_handshake", Status: "SKIP"},
		{SDK: "python-client", Name: "app_message_roundtrip", Status: "SKIP"},
	}
	if err := checkCounts(path, results); err != nil {
		t.Fatalf("checkCounts returned an error for an all-SKIP key: %v", err)
	}
}

// TestCheckCountsPartialSkipStillEnforcesFloor makes sure a key that ran
// for real (some PASS/FAIL rows, not every row SKIP) still gets its floor
// enforced -- SKIP-tolerance must not silently swallow a genuine regression.
func TestCheckCountsPartialSkipStillEnforcesFloor(t *testing.T) {
	path := writeCounts(t, map[string]int{"go-server": 3})
	results := []driver.Result{
		{SDK: "go-server", Name: "a", Status: "PASS"},
		{SDK: "go-server", Name: "b", Status: "SKIP"},
	}
	if err := checkCounts(path, results); err == nil {
		t.Fatal("checkCounts did not report a floor violation for a key with real (non-all-SKIP) results below floor")
	}
}

// TestCheckCountsUnseenKeyIsIgnored keeps the existing behaviour: a floor
// key this run never produced any results for at all (e.g. filtered out by
// --sdk) is not checked.
func TestCheckCountsUnseenKeyIsIgnored(t *testing.T) {
	path := writeCounts(t, map[string]int{"go-server": 100})
	if err := checkCounts(path, nil); err != nil {
		t.Fatalf("checkCounts flagged a key this run never produced results for: %v", err)
	}
}

// TestDiscoverSDKsFindsSubdirectories checks that discovery walks
// <adaptersDir>/*/ generically -- any subdirectory is a candidate SDK, with
// no per-name special casing (this spec repo itself ships no adapters; SDK
// repos point --adapters-dir at their own conformance/adapter/ tree).
func TestDiscoverSDKsFindsSubdirectories(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"go", "js", "python"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sdks, err := discoverSDKs(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, s := range sdks {
		names[s.name] = true
	}
	if !names["go"] || !names["js"] || !names["python"] {
		t.Fatalf("discoverSDKs(%q) = %v, want it to include go, js and python", dir, names)
	}
}

// TestDiscoverSDKsMissingAdaptersDirIsNotFatal covers an --adapters-dir that
// doesn't exist at all: discovery should return an empty list, not an
// error, so --sdk can still synthesize a SKIP row for whatever was named.
func TestDiscoverSDKsMissingAdaptersDirIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	sdks, err := discoverSDKs(filepath.Join(dir, "adapters"))
	if err != nil {
		t.Fatalf("discoverSDKs on a nonexistent adapters dir: %v", err)
	}
	if len(sdks) != 0 {
		t.Fatalf("discoverSDKs = %v, want empty", sdks)
	}
}
