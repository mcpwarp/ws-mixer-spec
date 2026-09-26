package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// TestEnforceCounts covers the floor gate main() applies to every run, passing
// the same --sdk/--mode/--fixture values main() would: --sdk and --mode only
// change which keys have rows, so a narrowed run is held to the floor of the
// keys it exercised and ignores the rest, while a --fixture glob skips the
// check with a note. The --sdk go / --mode pair / --mode fixture error cases
// fail if enforceCounts ever gates the check on sdkFilter or mode again.
func TestEnforceCounts(t *testing.T) {
	pass := func(sdk, name string) driver.Result { return driver.Result{SDK: sdk, Name: name, Status: "PASS"} }
	goFixtureRows := []driver.Result{pass("go-server", "a"), pass("go-client", "b"), pass("go-client", "c")}
	fixtureRows := append(append([]driver.Result{}, goFixtureRows...), pass("js-client", "b"), pass("js-client", "c"))
	pairRows := []driver.Result{pass("go->go", "s1"), pass("go->go", "s2"), pass("go->js", "s1")}
	goRows := append(append([]driver.Result{}, goFixtureRows...), pass("go->go", "s1"), pass("go->go", "s2"))
	allRows := append(append([]driver.Result{}, fixtureRows...), pairRows...)

	cases := []struct {
		name        string
		sdk         string
		mode        string
		fixtureGlob string
		floors      map[string]int
		results     []driver.Result
		wantErr     string
		wantNote    bool
	}{
		{
			name: "--sdk go, exercised key below floor", sdk: "go", mode: "all", fixtureGlob: "*",
			floors: map[string]int{"go-client": 3}, results: goRows,
			wantErr: "go-client: 2 passed, floor is 3",
		},
		{
			name: "--mode pair, exercised key below floor", sdk: "", mode: "pair", fixtureGlob: "*",
			floors: map[string]int{"go->js": 99}, results: pairRows,
			wantErr: "go->js: 1 passed, floor is 99",
		},
		{
			name: "--mode fixture, exercised key below floor", sdk: "", mode: "fixture", fixtureGlob: "*",
			floors: map[string]int{"js-client": 3}, results: fixtureRows,
			wantErr: "js-client: 2 passed, floor is 3",
		},
		{
			name: "--sdk go, unexercised keys below floor", sdk: "go", mode: "all", fixtureGlob: "*",
			floors: map[string]int{"go-client": 2, "js-server": 99, "go->js": 99}, results: goRows,
		},
		{
			name: "--mode pair, fixture keys unexercised", sdk: "", mode: "pair", fixtureGlob: "*",
			floors: map[string]int{"go->go": 2, "go-server": 99, "js-client": 99}, results: pairRows,
		},
		{
			name: "unfiltered, below floor", sdk: "", mode: "all", fixtureGlob: "*",
			floors: map[string]int{"go->go": 2, "js-client": 3}, results: allRows,
			wantErr: "js-client: 2 passed, floor is 3",
		},
		{
			name: "unfiltered, floor met", sdk: "", mode: "all", fixtureGlob: "*",
			floors: map[string]int{"go-server": 1, "go-client": 2, "js-client": 2, "go->go": 2, "go->js": 1}, results: allRows,
		},
		{
			name: "--fixture glob skips the check", sdk: "", mode: "all", fixtureGlob: "happy_*",
			floors: map[string]int{"go-client": 999, "go->go": 999}, results: allRows,
			wantNote: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeCounts(t, tc.floors)
			var stderr bytes.Buffer
			err := enforceCounts(&stderr, path, false, tc.sdk, tc.mode, tc.fixtureGlob, tc.results)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("enforceCounts = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("enforceCounts = %v, want an error containing %q", err, tc.wantErr)
			}
			const note = "COUNTS.json floor not checked"
			if got := strings.Contains(stderr.String(), note); got != tc.wantNote {
				t.Fatalf("stderr = %q, want note %q present=%v", stderr.String(), note, tc.wantNote)
			}
		})
	}
}

// TestEnforceCountsMissingFile: the defaulted path (no --counts) may be
// absent, since this repo ships no COUNTS.json, but says so on stderr; an
// explicit --counts that doesn't exist is an error, even when --fixture
// narrows the run.
func TestEnforceCountsMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "COUNTS.json")
	results := []driver.Result{{SDK: "go-client", Name: "a", Status: "PASS"}}

	var stderr bytes.Buffer
	if err := enforceCounts(&stderr, missing, true, "", "all", "*", results); err != nil {
		t.Fatalf("defaulted, missing counts file: enforceCounts = %v, want nil", err)
	}
	if want := "no COUNTS.json at " + missing + "; floor not checked"; !strings.Contains(stderr.String(), want) {
		t.Fatalf("defaulted, missing counts file: stderr = %q, want it to contain %q", stderr.String(), want)
	}
	if err := enforceCounts(io.Discard, missing, false, "", "all", "*", results); err == nil {
		t.Fatal("explicit, missing counts file: enforceCounts = nil, want an error")
	}
	if err := enforceCounts(io.Discard, missing, false, "", "all", "happy_*", results); err == nil {
		t.Fatal("explicit, missing counts file with --fixture happy_*: enforceCounts = nil, want an error")
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
