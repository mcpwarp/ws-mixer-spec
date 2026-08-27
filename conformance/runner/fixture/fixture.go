// Package fixture loads spec/fixtures/sequences/*.json, per spec/README.md's
// "Sequence fixtures" section.
package fixture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Step is one entry of a sequence fixture's `steps` array. Exactly one of
// Recv/Send/WaitMs/Expect is set (spec/README.md, enforced by
// spec/tools/check-fixtures.mjs, re-checked here defensively).
type Step struct {
	Recv         map[string]any `json:"recv,omitempty"`
	Send         map[string]any `json:"send,omitempty"`
	WaitMs       *int64         `json:"wait_ms,omitempty"`
	Expect       map[string]any `json:"expect,omitempty"`
	SchemaExempt bool           `json:"schema_exempt,omitempty"`
}

// Kind reports which of recv/send/wait_ms/expect this step carries.
func (s Step) Kind() string {
	switch {
	case s.Recv != nil:
		return "recv"
	case s.Send != nil:
		return "send"
	case s.WaitMs != nil:
		return "wait_ms"
	case s.Expect != nil:
		return "expect"
	default:
		return ""
	}
}

// Fixture is one spec/fixtures/sequences/*.json file.
type Fixture struct {
	Name        string // filename without .json
	Description string `json:"description"`
	Role        string `json:"role"` // "server" or "client"
	Steps       []Step `json:"steps"`
}

// IsFrame reports whether payload looks like a decoded mux frame object
// (has "type") as opposed to a control message (has "t"), per
// spec/README.md's "Sequence fixtures" disambiguation rule.
func IsFrame(payload map[string]any) bool {
	_, ok := payload["type"]
	return ok
}

// FrameType returns payload["type"] as a string, or "" if absent.
func FrameType(payload map[string]any) string {
	s, _ := payload["type"].(string)
	return s
}

// ControlType returns payload["t"] as a string, or "" if absent.
func ControlType(payload map[string]any) string {
	s, _ := payload["t"].(string)
	return s
}

// StreamID returns payload["stream_id"] as a uint32, or 0 if absent/invalid.
func StreamID(payload map[string]any) uint32 {
	switch v := payload["stream_id"].(type) {
	case float64:
		return uint32(v)
	}
	return 0
}

// Load reads and parses one sequence fixture file.
func Load(path string) (*Fixture, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f Fixture
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	f.Name = trimExt(filepath.Base(path))
	if f.Role != "server" && f.Role != "client" {
		return nil, fmt.Errorf("%s: role must be \"server\" or \"client\", got %q", path, f.Role)
	}
	for i, st := range f.Steps {
		kinds := 0
		for _, ok := range []bool{st.Recv != nil, st.Send != nil, st.WaitMs != nil, st.Expect != nil} {
			if ok {
				kinds++
			}
		}
		if kinds != 1 {
			return nil, fmt.Errorf("%s: step %d must have exactly one of recv/send/wait_ms/expect, has %d", path, i, kinds)
		}
	}
	return &f, nil
}

func trimExt(name string) string {
	ext := filepath.Ext(name)
	return name[:len(name)-len(ext)]
}

// LoadDir loads every *.json file in dir, sorted by filename for
// deterministic output.
func LoadDir(dir string) ([]*Fixture, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var out []*Fixture
	for _, n := range names {
		f, err := Load(filepath.Join(dir, n))
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}
