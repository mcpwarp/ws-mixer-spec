// Package wire is the conformance runner's own, independent mux-frame codec
// (docs/CONFORMANCE.md section 2): a hand-rolled binary.BigEndian codec that
// deliberately does NOT import go/wsmixer, so a Go SDK codec bug is not
// invisible to the Go SDK's own conformance run. It has no opinion on wire
// validity beyond "is there a legible 8-byte header" -- the raw actor's whole
// point is to send/observe bytes without judging them.
package wire

import "encoding/binary"

// Frame types, per OVERVIEW.md section 2.2.
const (
	TypeOpen   = 0x00
	TypeData   = 0x01
	TypeWindow = 0x02
	TypeClose  = 0x03
	TypeReset  = 0x04
)

const HeaderSize = 8

// Frame is one decoded mux frame: 8-byte header plus payload.
type Frame struct {
	Type     uint8
	Flags    uint8
	StreamID uint32
	Payload  []byte
}

// Decode parses one WebSocket message into a Frame. It performs no
// wire-validity checks beyond "long enough to have a header" -- the raw
// actor logs whatever bytes arrive, illegal or not.
func Decode(msg []byte) (*Frame, bool) {
	if len(msg) < HeaderSize {
		return nil, false
	}
	return &Frame{
		Type:     msg[0],
		Flags:    msg[1],
		StreamID: binary.BigEndian.Uint32(msg[4:8]),
		Payload:  append([]byte(nil), msg[8:]...),
	}, true
}

// Encode serializes f to its wire form. Flags and the reserved bytes are
// always written as f.Flags/0 -- the raw actor can deliberately set Flags
// nonzero for the "close_nonzero_flags"-shaped tests.
func Encode(f *Frame) []byte {
	out := make([]byte, HeaderSize+len(f.Payload))
	out[0] = f.Type
	out[1] = f.Flags
	out[2] = 0
	out[3] = 0
	binary.BigEndian.PutUint32(out[4:8], f.StreamID)
	copy(out[8:], f.Payload)
	return out
}

func Open(streamID uint32) []byte { return Encode(&Frame{Type: TypeOpen, StreamID: streamID}) }

func Data(streamID uint32, payload []byte) []byte {
	return Encode(&Frame{Type: TypeData, StreamID: streamID, Payload: payload})
}

func Window(streamID uint32, increment uint32) []byte {
	p := make([]byte, 4)
	binary.BigEndian.PutUint32(p, increment)
	return Encode(&Frame{Type: TypeWindow, StreamID: streamID, Payload: p})
}

func Close(streamID uint32) []byte { return Encode(&Frame{Type: TypeClose, StreamID: streamID}) }

func Reset(streamID uint32, code uint32, message string) []byte {
	p := make([]byte, 4+len(message))
	binary.BigEndian.PutUint32(p[:4], code)
	copy(p[4:], message)
	return Encode(&Frame{Type: TypeReset, StreamID: streamID, Payload: p})
}

// TypeName renders a frame type's wire name, matching spec/fixtures/frames'
// `decoded.type` strings, or "" for a type outside the known five.
func TypeName(t uint8) string {
	switch t {
	case TypeOpen:
		return "OPEN"
	case TypeData:
		return "DATA"
	case TypeWindow:
		return "WINDOW"
	case TypeClose:
		return "CLOSE"
	case TypeReset:
		return "RESET"
	default:
		return ""
	}
}

// WindowIncrement returns a WINDOW frame's 4-byte increment. Caller must
// ensure len(f.Payload) >= 4.
func WindowIncrement(f *Frame) uint32 { return binary.BigEndian.Uint32(f.Payload) }

// ResetCode returns a RESET frame's 4-byte error code. Caller must ensure
// len(f.Payload) >= 4.
func ResetCode(f *Frame) uint32 { return binary.BigEndian.Uint32(f.Payload[:4]) }

// ResetMessage returns a RESET frame's trailing message bytes as a string,
// verbatim -- no UTF-8 sanitizing (that is SDK-level policy, not this
// codec's job).
func ResetMessage(f *Frame) string {
	if len(f.Payload) <= 4 {
		return ""
	}
	return string(f.Payload[4:])
}
