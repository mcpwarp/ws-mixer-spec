// Package codes is the runner's own copy of the OVERVIEW.md section 2.8
// error code table -- independent of go/wsmixer's errors.go, for the same
// reason wire is independent of go/wsmixer's frame.go (docs/CONFORMANCE.md
// section 2): a Go SDK bug in this table must not be invisible to the Go
// SDK's own conformance run.
package codes

var byName = map[string]uint32{
	"NO_ERROR":           0x00,
	"PROTOCOL_ERROR":     0x01,
	"INTERNAL_ERROR":     0x02,
	"FLOW_CONTROL_ERROR": 0x03,
	"FRAME_SIZE_ERROR":   0x04,
	"STREAM_CLOSED":      0x05,
	"REFUSED_STREAM":     0x06,
	"CANCEL":             0x07,
	"STREAM_LIMIT":       0x08,
	"ENHANCE_YOUR_CALM":  0x09,
	"UNSUPPORTED":        0x0a,
	"UNAUTHORIZED":       0x0b,
	"GOING_AWAY":         0x0c,
	"KEEPALIVE_TIMEOUT":  0x0d,
}

var byCode = func() map[uint32]string {
	m := make(map[uint32]string, len(byName))
	for k, v := range byName {
		m[v] = k
	}
	return m
}()

// Name renders a numeric code's wire name, or "INTERNAL_ERROR" for anything
// unrecognized (OVERVIEW.md section 2.8).
func Name(code uint32) string {
	if n, ok := byCode[code]; ok {
		return n
	}
	return "INTERNAL_ERROR"
}

// Code looks up a wire name's numeric value.
func Code(name string) (uint32, bool) {
	c, ok := byName[name]
	return c, ok
}

// CloseCode is the mechanical ws_close = 4000 + code rule, NO_ERROR -> 1000.
func CloseCode(code uint32) int {
	if code == 0 {
		return 1000
	}
	return 4000 + int(code)
}
