package wire

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf8"
)

// frameFixture mirrors spec/fixtures/frames/*.json's shape (spec/README.md
// "Frame fixtures"). Only the fields this decoder can meaningfully check are
// decoded here.
type frameFixture struct {
	Description string `json:"description"`
	Hex         string `json:"hex"`
	Valid       bool   `json:"valid"`
	Decoded     *struct {
		Type       string  `json:"type"`
		Flags      uint8   `json:"flags"`
		StreamID   uint32  `json:"stream_id"`
		PayloadHex string  `json:"payload_hex"`
		PayloadLen *int    `json:"payload_length"`
		Increment  *uint32 `json:"increment"`
		Code       *uint32 `json:"code"`
		Message    *string `json:"message"`
	} `json:"decoded"`
}

func fixturesDir(t *testing.T) string {
	t.Helper()
	// conformance/runner/wire -> ../../../spec/fixtures/frames
	dir, err := filepath.Abs(filepath.Join("..", "..", "..", "spec", "fixtures", "frames"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("spec/fixtures/frames not found at %s: %v", dir, err)
	}
	return dir
}

// TestFrameFixtures decodes every spec/fixtures/frames/*.json's hex with this
// package's independent codec and checks the result against the fixture's
// `decoded` object for the fields this minimal (deliberately non-validating,
// docs/CONFORMANCE.md section 2) decoder can produce: type/flags/stream_id
// always, plus payload_hex/increment/code/message depending on frame type.
// This decoder performs no wire-validity checks (that is the whole point of
// the raw actor: it never judges bytes, only records them) beyond "is the
// header at least 8 bytes long" -- so fixtures with `valid:false` for a
// semantic reason (bad WINDOW length, high-bit stream id, oversized message,
// stream-0 payload over 16KiB) still decode "successfully" here and are
// checked against `decoded` only when the fixture happens to provide one
// (it never does for those, by construction); the only `ok=false` case this
// decoder can produce is "shorter than 8 bytes".
func TestFrameFixtures(t *testing.T) {
	dir := fixturesDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no frame fixtures found")
	}
	seenShortHeader := false
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			var fx frameFixture
			if err := json.Unmarshal(b, &fx); err != nil {
				t.Fatalf("unmarshal fixture: %v", err)
			}
			raw, err := hex.DecodeString(fx.Hex)
			if err != nil {
				t.Fatalf("fixture hex does not decode: %v", err)
			}

			f, ok := Decode(raw)
			if len(raw) < HeaderSize {
				seenShortHeader = true
				if ok {
					t.Fatalf("expected Decode to reject a %d-byte message as too short", len(raw))
				}
				return
			}
			if !ok {
				t.Fatalf("Decode rejected a message with a full 8-byte header")
			}
			if fx.Decoded == nil {
				return // structurally-valid-but-semantically-invalid case; nothing more to check
			}
			d := fx.Decoded
			if got, want := TypeName(f.Type), d.Type; got != want && want != "" {
				// UNKNOWN(0xNN) fixtures render with a literal hex nibble; our
				// TypeName returns "" for unknown types, so special-case that
				// family instead of trying to reproduce the exact string.
				if !(got == "" && len(want) >= 7 && want[:7] == "UNKNOWN") {
					t.Errorf("type = %s (0x%02x), want %s", got, f.Type, want)
				}
			}
			if f.Flags != d.Flags {
				t.Errorf("flags = %d, want %d", f.Flags, d.Flags)
			}
			if f.StreamID != d.StreamID {
				t.Errorf("stream_id = %d, want %d", f.StreamID, d.StreamID)
			}
			switch f.Type {
			case TypeWindow:
				if d.Increment != nil && len(f.Payload) >= 4 {
					if got := WindowIncrement(f); got != *d.Increment {
						t.Errorf("increment = %d, want %d", got, *d.Increment)
					}
				}
			case TypeReset:
				if len(f.Payload) >= 4 {
					if d.Code != nil {
						if got := ResetCode(f); got != *d.Code {
							t.Errorf("code = %d, want %d", got, *d.Code)
						}
					}
					// This codec deliberately does no UTF-8 sanitizing (the raw
					// actor never judges bytes) -- reset_invalid_utf8_message.json's
					// `decoded.message` is the SDK's sanitized (U+FFFD-replaced)
					// rendering, which only applies when the payload actually is
					// invalid UTF-8, so skip the comparison in that one case.
					if d.Message != nil && utf8.Valid(f.Payload[4:]) {
						if got := ResetMessage(f); got != *d.Message {
							t.Errorf("message = %q, want %q", got, *d.Message)
						}
					}
				}
			default:
				if d.PayloadLen != nil {
					if len(f.Payload) != *d.PayloadLen {
						t.Errorf("payload length = %d, want %d", len(f.Payload), *d.PayloadLen)
					}
				} else if d.PayloadHex != "" || len(f.Payload) > 0 {
					if got := hex.EncodeToString(f.Payload); got != d.PayloadHex {
						t.Errorf("payload_hex = %s, want %s", got, d.PayloadHex)
					}
				}
			}
		})
	}
	if !seenShortHeader {
		t.Error("expected at least one header-too-short fixture to exercise the ok=false path")
	}
}

// TestEncodeRoundTrip checks that every Encode{Open,Data,Window,Close,Reset}
// helper's output decodes back to the fields it was built from.
func TestEncodeRoundTrip(t *testing.T) {
	if f, ok := Decode(Open(5)); !ok || f.Type != TypeOpen || f.StreamID != 5 {
		t.Fatalf("Open round-trip: %+v ok=%v", f, ok)
	}
	if f, ok := Decode(Data(3, []byte("hello"))); !ok || f.Type != TypeData || f.StreamID != 3 || string(f.Payload) != "hello" {
		t.Fatalf("Data round-trip: %+v ok=%v", f, ok)
	}
	if f, ok := Decode(Window(7, 1024)); !ok || f.Type != TypeWindow || WindowIncrement(f) != 1024 {
		t.Fatalf("Window round-trip: %+v ok=%v", f, ok)
	}
	if f, ok := Decode(Close(9)); !ok || f.Type != TypeClose || f.StreamID != 9 {
		t.Fatalf("Close round-trip: %+v ok=%v", f, ok)
	}
	if f, ok := Decode(Reset(11, 7, "cancelled")); !ok || f.Type != TypeReset || ResetCode(f) != 7 || ResetMessage(f) != "cancelled" {
		t.Fatalf("Reset round-trip: %+v ok=%v", f, ok)
	}
}
