// Command runner is the ws-mixer conformance runner
// (docs/CONFORMANCE.md): it drives every spec/fixtures/sequences/*.json
// against the SDK matching the fixture's role, with the runner's own raw
// wire actor playing the opposite role, and drives conformance/scenarios/*.json
// pair scenarios between two real SDK adapters.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mcpwarp/ws-mixer/conformance/runner/adapter"
	"github.com/mcpwarp/ws-mixer/conformance/runner/driver"
	"github.com/mcpwarp/ws-mixer/conformance/runner/fixture"
	"github.com/mcpwarp/ws-mixer/conformance/runner/report"
)

// sdkDef is one entry of the matrix: an SDK name, the roles it supports, and
// how to build/locate its adapter binary.
type sdkDef struct {
	name  string
	roles map[string]bool // "server" and/or "client"
	build func(repoRoot, workDir string) (string, error)
}

func main() {
	var (
		timeScale       = flag.Float64("time-scale", 1.0/50, "multiplies every fixture wait_ms and internal timer (docs/CONFORMANCE.md section 3.4; see conformance/README.md for why this runner's default is more conservative than the doc's suggested 1/250)")
		sdkFilter       = flag.String("sdk", "", "comma-separated SDK names to run (default: all discovered)")
		fixtureGlob     = flag.String("fixture", "*", "glob filter on fixture/scenario name")
		mode            = flag.String("mode", "all", "fixture | pair | all")
		reportPath      = flag.String("report", "", "write JUnit XML to this path")
		adapterOverride = flag.String("adapter", "", "name=path, overrides adapter discovery for one SDK (repeatable via comma: go=path1,js=path2)")
		verbose         = flag.Bool("v", false, "print failure detail immediately")
		strict          = flag.Bool("strict", true, "raw actor fails on any observed frame the current step doesn't expect, instead of skipping it (docs/CONFORMANCE.md section 2)")
	)
	flag.Parse()

	repoRoot, err := findRepoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "conformance runner:", err)
		os.Exit(2)
	}

	workDir, err := os.MkdirTemp("", "ws-mixer-conformance-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	defer os.RemoveAll(workDir)

	overrides := parseAdapterOverrides(*adapterOverride)

	sdks, err := discoverSDKs(repoRoot)
	if err != nil {
		fmt.Fprintln(os.Stderr, "conformance runner: discovering adapters:", err)
		os.Exit(2)
	}
	// --adapter names an SDK the discovery pass above didn't find a
	// directory for (e.g. a prebuilt binary handed in directly): add it as
	// its own sdkDef rather than silently ignoring the override.
	for name := range overrides {
		found := false
		for _, s := range sdks {
			if s.name == name {
				found = true
				break
			}
		}
		if !found {
			sdks = append(sdks, sdkDef{name: name, roles: map[string]bool{"server": true, "client": true}})
		}
	}
	if *sdkFilter != "" {
		want := map[string]bool{}
		for _, s := range strings.Split(*sdkFilter, ",") {
			want[strings.TrimSpace(s)] = true
		}
		var filtered []sdkDef
		for name := range want {
			found := false
			for _, s := range sdks {
				if s.name == name {
					filtered = append(filtered, s)
					found = true
					break
				}
			}
			if !found {
				// Named on --sdk but no adapter directory and no --adapter
				// override: report it as a SKIP row per SDK/fixture, not a
				// hard error (docs/CONFORMANCE.md section 3.5: "A missing or
				// non-executable adapter is a listed SKIP ... never a hard
				// failure" -- e.g. `--sdk python` before python/ exists).
				filtered = append(filtered, sdkDef{name: name, roles: map[string]bool{"server": true, "client": true}})
			}
		}
		sdks = filtered
	}

	// Resolve (or record missing) each SDK's adapter binary once, up front.
	type resolved struct {
		def  sdkDef
		path string
		err  error
	}
	var binaries []resolved
	for _, s := range sdks {
		if p, ok := overrides[s.name]; ok {
			binaries = append(binaries, resolved{def: s, path: p})
			continue
		}
		if s.build == nil {
			binaries = append(binaries, resolved{def: s, err: fmt.Errorf("no adapter directory at conformance/adapters/%s and no --adapter override", s.name)})
			continue
		}
		p, err := s.build(repoRoot, workDir)
		if err != nil && s.name == "go" {
			// go is the reference implementation (the only server SDK), and
			// a build failure here is a toolchain/config problem -- e.g. GO
			// pointing at a go binary that doesn't exist -- not a "missing
			// adapter" (docs/CONFORMANCE.md section 3.5's SKIP rule is for
			// an SDK that was never expected to be present, like python
			// today). Silently downgrading this to a SKIP on every go-* key
			// would quietly zero out most of the matrix and still exit 0.
			fmt.Fprintf(os.Stderr, "conformance runner: go adapter failed to build: %v\n", err)
			os.Exit(1)
		}
		binaries = append(binaries, resolved{def: s, path: p, err: err})
	}

	// Probe each successfully built/located adapter once for the roles it
	// actually supports (ready.roles, docs/CONFORMANCE.md section 1.2),
	// replacing the placeholder {server:true, client:true} guess from
	// discovery/--sdk/--adapter above. This keeps the fixture/pair-mode role
	// filtering below meaningful without hardcoding per-SDK role tables in
	// this file.
	for i := range binaries {
		if binaries[i].err != nil {
			continue
		}
		if roles, err := probeRoles(binaries[i].def.name, binaries[i].path); err == nil {
			binaries[i].def.roles = roles
		}
	}

	timing := driver.Timing{Scale: *timeScale, Strict: *strict}
	sd, err := driver.LoadStepDriving(filepath.Join(repoRoot, "conformance", "scenarios", "step-driving.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "loading step-driving.json:", err)
		os.Exit(2)
	}

	var results []driver.Result
	ctx := context.Background()

	if *mode == "fixture" || *mode == "all" {
		fixtures, err := fixture.LoadDir(filepath.Join(repoRoot, "spec", "fixtures", "sequences"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "loading fixtures:", err)
			os.Exit(2)
		}
		for _, f := range fixtures {
			if !globMatch(*fixtureGlob, f.Name) {
				continue
			}
			for _, b := range binaries {
				if !b.def.roles[f.Role] {
					results = append(results, driver.Result{
						Mode: "fixture", SDK: b.def.name + "-" + f.Role, Name: f.Name,
						Status: "SKIP", Message: fmt.Sprintf("%s SDK does not support role %q", b.def.name, f.Role),
					})
					continue
				}
				r := driver.Result{Mode: "fixture", SDK: b.def.name + "-" + f.Role, Name: f.Name}
				if b.err != nil {
					r.Status = "SKIP"
					r.Message = fmt.Sprintf("no adapter for %s: %v", b.def.name, b.err)
					results = append(results, r)
					continue
				}
				results = append(results, runOneFixture(ctx, f, b.def.name, b.path, timing, sd, *verbose))
			}
		}
	}

	if *mode == "pair" || *mode == "all" {
		scenarios, err := driver.LoadScenarios(filepath.Join(repoRoot, "conformance", "scenarios"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "loading scenarios:", err)
			os.Exit(2)
		}
		for _, s := range scenarios {
			if !globMatch(*fixtureGlob, s.Name) {
				continue
			}
			for _, srv := range binaries {
				if !srv.def.roles["server"] {
					continue
				}
				for _, cli := range binaries {
					if !cli.def.roles["client"] {
						continue
					}
					label := srv.def.name + "->" + cli.def.name
					if srv.err != nil || cli.err != nil {
						missing := srv.def.name
						if cli.err != nil {
							missing = cli.def.name
						}
						results = append(results, driver.Result{Mode: "pair", SDK: label, Name: s.Name, Status: "SKIP", Message: "no adapter for " + missing})
						continue
					}
					results = append(results, runOnePair(ctx, s, srv.def.name, srv.path, cli.def.name, cli.path, timing, label, *verbose))
				}
			}
		}
	}

	if len(results) == 0 {
		fmt.Fprintln(os.Stderr, "conformance runner: no fixtures or scenarios selected -- check --sdk/--fixture/--mode filters")
		os.Exit(2)
	}

	report.PrintTable(os.Stdout, results)

	// The COUNTS.json floor is a guard against regressing the full matrix; a
	// filtered run (--sdk, --fixture, or a non-"all" --mode, which narrows
	// which scenarios/fixtures can appear at all) only ever produces a
	// subset of that matrix's results, so checking it here would either
	// false-fail on a deliberately narrow run or silently pass a floor it
	// never actually exercised. Only check it on a genuinely unfiltered run.
	if *sdkFilter == "" && *fixtureGlob == "*" && *mode == "all" {
		countsPath := filepath.Join(repoRoot, "conformance", "COUNTS.json")
		if err := checkCounts(countsPath, results); err != nil {
			fmt.Fprintln(os.Stderr, "conformance runner:", err)
			os.Exit(1)
		}
	}

	if *reportPath != "" {
		p := *reportPath
		if !filepath.IsAbs(p) {
			// A relative --report is resolved against the repo root, not
			// this process's cwd (conformance/runner/, when launched via
			// `go run .` from the Makefile) -- so `--report
			// build/conformance.xml` lands where a human typing that path
			// from the repo root would expect it.
			p = filepath.Join(repoRoot, p)
		}
		if err := report.WriteJUnit(p, results); err != nil {
			fmt.Fprintln(os.Stderr, "writing report:", err)
			os.Exit(2)
		}
	}

	for _, r := range results {
		if r.Status == "FAIL" {
			os.Exit(1)
		}
	}
}

// discoverSDKs enumerates conformance/adapters/*/ (one subdirectory per
// SDK, docs/CONFORMANCE.md section 5) instead of a hardcoded {go, js} list,
// so a new adapter directory (e.g. conformance/adapters/python/) is picked
// up with no runner change -- exactly what section 6's Python checklist
// promises. Roles start as an optimistic {server, client} guess; main()
// replaces it with the adapter's actual self-reported roles once it is
// built (probeRoles).
func discoverSDKs(repoRoot string) ([]sdkDef, error) {
	dir := filepath.Join(repoRoot, "conformance", "adapters")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var sdks []sdkDef
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		def := sdkDef{name: name, roles: map[string]bool{"server": true, "client": true}}
		switch name {
		case "go":
			def.build = buildGoAdapter
		case "js":
			def.build = buildJSAdapter
		default:
			def.build = buildGenericAdapter(name)
		}
		sdks = append(sdks, def)
	}
	sort.Slice(sdks, func(i, j int) bool { return sdks[i].name < sdks[j].name })
	return sdks, nil
}

// probeRoles spawns the adapter just long enough to read its `ready` event
// (docs/CONFORMANCE.md section 1.2: "roles is [\"server\"], [\"client\"] or
// both") and reports what it actually supports, rather than trusting a
// static table in this file.
func probeRoles(name, path string) (map[string]bool, error) {
	a, err := adapter.Spawn(name, path)
	if err != nil {
		return nil, err
	}
	defer a.Kill()
	ready, err := a.WaitForTimeout(func(e adapter.Event) bool { return e.Event == "ready" }, 10*time.Second)
	if err != nil {
		return nil, err
	}
	roles := map[string]bool{}
	if list, ok := ready.Raw["roles"].([]any); ok {
		for _, r := range list {
			if s, ok := r.(string); ok {
				roles[s] = true
			}
		}
	}
	return roles, nil
}

func runOneFixture(ctx context.Context, f *fixture.Fixture, sdkName, path string, timing driver.Timing, sd *driver.StepDriving, verbose bool) driver.Result {
	a, err := adapter.Spawn(sdkName, path)
	if err != nil {
		return driver.Result{Mode: "fixture", SDK: sdkName + "-" + f.Role, Name: f.Name, Status: "SKIP", Message: "spawn failed: " + err.Error()}
	}
	defer a.Kill()

	ready, err := a.WaitForTimeout(func(e adapter.Event) bool { return e.Event == "ready" }, 10*time.Second)
	if err != nil {
		return driver.Result{Mode: "fixture", SDK: sdkName + "-" + f.Role, Name: f.Name, Status: "SKIP", Message: "adapter never emitted ready: " + err.Error()}
	}
	if !rolesInclude(ready, f.Role) {
		return driver.Result{Mode: "fixture", SDK: sdkName + "-" + f.Role, Name: f.Name, Status: "SKIP", Message: fmt.Sprintf("adapter does not support role %q", f.Role)}
	}

	fctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	res := driver.RunFixture(fctx, f, a, timing, sd)
	res.SDK = sdkName + "-" + f.Role
	if verbose && res.Status == "FAIL" {
		fmt.Fprintln(os.Stderr, "--- FAIL detail:", res.Name, "---")
		fmt.Fprintln(os.Stderr, res.Detail)
	}
	return res
}

func runOnePair(ctx context.Context, s *driver.Scenario, srvName, srvPath, cliName, cliPath string, timing driver.Timing, label string, verbose bool) driver.Result {
	srv, err := adapter.Spawn(srvName, srvPath)
	if err != nil {
		return driver.Result{Mode: "pair", SDK: label, Name: s.Name, Status: "SKIP", Message: "server spawn failed: " + err.Error()}
	}
	defer srv.Kill()
	cli, err := adapter.Spawn(cliName, cliPath)
	if err != nil {
		return driver.Result{Mode: "pair", SDK: label, Name: s.Name, Status: "SKIP", Message: "client spawn failed: " + err.Error()}
	}
	defer cli.Kill()

	if _, err := srv.WaitForTimeout(func(e adapter.Event) bool { return e.Event == "ready" }, 10*time.Second); err != nil {
		return driver.Result{Mode: "pair", SDK: label, Name: s.Name, Status: "SKIP", Message: "server never emitted ready"}
	}
	if _, err := cli.WaitForTimeout(func(e adapter.Event) bool { return e.Event == "ready" }, 10*time.Second); err != nil {
		return driver.Result{Mode: "pair", SDK: label, Name: s.Name, Status: "SKIP", Message: "client never emitted ready"}
	}

	pctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	res := driver.RunPair(pctx, s, srv, cli, timing, label)
	if verbose && res.Status == "FAIL" {
		fmt.Fprintln(os.Stderr, "--- FAIL detail:", res.Name, "---")
		fmt.Fprintln(os.Stderr, res.Detail)
	}
	return res
}

// checkCounts loads conformance/COUNTS.json -- a checked-in floor on the
// number of PASSing (SDK, fixture-or-scenario) cells per SDK key, the same
// trick as spec/fixtures/COUNTS.json (docs/CONFORMANCE.md section 5) -- and
// fails if this run's actual count for any key present in the file dropped
// below its floor. A key this run never produced any results for (e.g. an
// SDK filtered out by --sdk) is not checked -- the floor only guards
// against a *regression* in a run that actually exercised that key.
func checkCounts(path string, results []driver.Result) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s does not exist -- run the full matrix once and check in the observed pass counts as the floor", path)
		}
		return fmt.Errorf("reading %s: %w", path, err)
	}
	var doc struct {
		Floors map[string]int `json:"floors"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	floors := doc.Floors

	actual := map[string]int{}
	total := map[string]int{}
	skipped := map[string]int{}
	seen := map[string]bool{}
	for _, r := range results {
		seen[r.SDK] = true
		total[r.SDK]++
		switch r.Status {
		case "PASS":
			actual[r.SDK]++
		case "SKIP":
			skipped[r.SDK]++
		}
	}

	var violations, allSkipped []string
	for key, floor := range floors {
		if !seen[key] {
			continue
		}
		// A key every one of whose rows is SKIP (adapter missing/unbuildable
		// for this run, e.g. `--sdk python` before an adapter exists) is not
		// a regression to guard against -- it never ran anything, so it
		// can't have dropped below a floor it never exercised
		// (docs/CONFORMANCE.md section 3.5: "never a hard failure").
		if skipped[key] == total[key] {
			allSkipped = append(allSkipped, fmt.Sprintf("%s: %d/%d rows skipped", key, skipped[key], total[key]))
			continue
		}
		if actual[key] < floor {
			violations = append(violations, fmt.Sprintf("%s: %d passed, floor is %d", key, actual[key], floor))
		}
	}
	if len(allSkipped) > 0 {
		sort.Strings(allSkipped)
		fmt.Fprintf(os.Stderr, "conformance runner: floor not checked for fully-skipped keys:\n  %s\n", strings.Join(allSkipped, "\n  "))
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		return fmt.Errorf("pass count below COUNTS.json floor:\n  %s", strings.Join(violations, "\n  "))
	}
	return nil
}

func rolesInclude(ready adapter.Event, role string) bool {
	roles, _ := ready.Raw["roles"].([]any)
	for _, r := range roles {
		if s, _ := r.(string); s == role {
			return true
		}
	}
	return false
}

func parseAdapterOverrides(s string) map[string]string {
	out := map[string]string{}
	if s == "" {
		return out
	}
	for _, pair := range strings.Split(s, ",") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) == 2 {
			out[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	return out
}

func globMatch(pattern, name string) bool {
	ok, err := filepath.Match(pattern, name)
	return err == nil && ok
}

func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "spec", "fixtures", "sequences")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find repo root (looked for spec/fixtures/sequences upward from %s)", dir)
		}
		dir = parent
	}
}
