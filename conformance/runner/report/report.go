// Package report renders driver.Result slices as a stdout table and a
// JUnit XML file (docs/CONFORMANCE.md section 3.5).
package report

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mcpwarp/ws-mixer/conformance/runner/driver"
)

// PrintTable writes the "MODE SDK FIXTURE/SCENARIO RESULT" table plus the
// summary line to w.
func PrintTable(w io.Writer, results []driver.Result) {
	fmt.Fprintf(w, "%-8s %-12s %-30s %s\n", "MODE", "SDK", "FIXTURE/SCENARIO", "RESULT")
	pass, fail, skip := 0, 0, 0
	skippedAdapters := map[string]bool{}
	for _, r := range results {
		line := r.Status
		if r.Status == "FAIL" && r.Message != "" {
			line += "  " + r.Message
		}
		if r.Status == "SKIP" && r.Message != "" {
			line += "  " + r.Message
			if strings.Contains(r.Message, "no adapter") {
				skippedAdapters[r.SDK] = true
			}
		}
		fmt.Fprintf(w, "%-8s %-12s %-30s %s\n", r.Mode, r.SDK, r.Name, line)
		switch r.Status {
		case "PASS":
			pass++
		case "FAIL":
			fail++
		case "SKIP":
			skip++
		}
	}
	missing := ""
	if len(skippedAdapters) > 0 {
		names := make([]string, 0, len(skippedAdapters))
		for n := range skippedAdapters {
			names = append(names, n)
		}
		missing = fmt.Sprintf(" (%d adapter missing: %s)", len(names), strings.Join(names, ", "))
	}
	fmt.Fprintf(w, "\n%d passed · %d failed · %d skipped%s\n", pass, fail, skip, missing)
}

// --- JUnit XML ---------------------------------------------------------------

type junitSuites struct {
	XMLName xml.Name     `xml:"testsuites"`
	Suites  []junitSuite `xml:"testsuite"`
}

type junitSuite struct {
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Skipped  int         `xml:"skipped,attr"`
	Cases    []junitCase `xml:"testcase"`
}

type junitCase struct {
	Name      string        `xml:"name,attr"`
	Classname string        `xml:"classname,attr"`
	Failure   *junitFailure `xml:"failure,omitempty"`
	Skipped   *junitSkipped `xml:"skipped,omitempty"`
	SystemErr string        `xml:"system-err,omitempty"`
}

type junitFailure struct {
	Message string `xml:"message,attr"`
	Body    string `xml:",chardata"`
}

type junitSkipped struct {
	Message string `xml:"message,attr"`
}

// WriteJUnit writes one <testsuite> per SDK, one <testcase> per
// fixture/scenario, per docs/CONFORMANCE.md section 3.5.
func WriteJUnit(path string, results []driver.Result) error {
	bySDK := map[string][]driver.Result{}
	var order []string
	for _, r := range results {
		if _, ok := bySDK[r.SDK]; !ok {
			order = append(order, r.SDK)
		}
		bySDK[r.SDK] = append(bySDK[r.SDK], r)
	}
	doc := junitSuites{}
	for _, sdk := range order {
		rs := bySDK[sdk]
		suite := junitSuite{Name: sdk, Tests: len(rs)}
		for _, r := range rs {
			c := junitCase{Name: r.Mode + "/" + r.Name, Classname: sdk}
			switch r.Status {
			case "FAIL":
				suite.Failures++
				c.Failure = &junitFailure{Message: r.Message, Body: r.Detail}
			case "SKIP":
				suite.Skipped++
				c.Skipped = &junitSkipped{Message: r.Message}
			}
			if r.Detail != "" && r.Status != "FAIL" {
				c.SystemErr = r.Detail
			}
			suite.Cases = append(suite.Cases, c)
		}
		doc.Suites = append(doc.Suites, suite)
	}
	b, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	b = append([]byte(xml.Header), b...)
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, b, 0o644)
}
