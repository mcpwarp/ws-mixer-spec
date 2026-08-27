package driver

import "time"

// Timing is the set of time-scaled durations for one run
// (docs/CONFORMANCE.md section 3.4). Every duration is floored at 300ms so a
// loaded CI box does not turn a few-ms window into a flake.
type Timing struct {
	Scale float64 // e.g. 1.0/250
	// Strict controls the raw actor's --strict mode (default true, set by
	// main.go): any observed frame a fixture step doesn't expect fails the
	// step, instead of being silently skipped over. See rawactor.SetStrict.
	Strict bool
}

// floor is the minimum any scaled duration is allowed to reach.
// docs/CONFORMANCE.md section 3.4 suggests 20ms; this runner uses a more
// conservative 300ms instead, learned the hard way (see
// conformance/README.md's "timing floor" note): unlike an in-process fake
// transport, a scaled hello_timeout_ms races real OS process scheduling on
// both the adapter-under-test's and the runner's own goroutines to actually
// perform a TCP+WebSocket round trip and send bytes -- 20-40ms flaked
// intermittently (an adapter's own hello timer firing before the runner's
// raw actor goroutine got scheduled to send welcome), so this floor buys
// real headroom at the cost of a somewhat slower test run.
const floor = 300 * time.Millisecond

// Scale multiplies ms by t.Scale and floors at 300ms.
func (t Timing) Scale_(ms int64) time.Duration {
	d := time.Duration(float64(ms)*t.Scale) * time.Millisecond
	if d < floor {
		return floor
	}
	return d
}

// ScaleMs is Scale_ returning milliseconds, for building set_options/command payloads.
func (t Timing) ScaleMs(ms int64) int64 { return t.Scale_(ms).Milliseconds() }

// FloorMs is the timing floor in milliseconds, passed to the adapter as
// set_options.floor_ms (coordination contract with conformance/adapters/**:
// the adapter applies the same floor to its own scaled timers, so a scaled
// value that would otherwise flake below floor is clamped identically on
// both sides).
func (t Timing) FloorMs() int64 { return floor.Milliseconds() }

// Grace is the bounded wait for an `expect` assertion (docs/CONFORMANCE.md
// section 4's suggested "5 x time-scaled ~40ms", adapted to this runner's own
// 300ms floor): 5x the 300ms floor (1500ms) by default, but never less than
// that, and more if a 1s baseline scaled by t.Scale comes out higher.
func (t Timing) Grace() time.Duration {
	g := 5 * floor
	scaled := t.Scale_(1000) // a generous 1s baseline, scaled down
	if scaled > g {
		return scaled
	}
	return g
}
