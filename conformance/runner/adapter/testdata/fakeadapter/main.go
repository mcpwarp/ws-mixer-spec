// Command fakeadapter is a minimal, hand-written stand-in for a real SDK
// adapter (docs/CONFORMANCE.md section 1), used only by adapter_test.go to
// exercise the driver (Spawn/Send/WaitFor/SendAndAck/Kill) without needing a
// real SDK built. It implements just enough of the protocol to be useful:
// ready, ack, a canned "connected" on `connect`, an app echo on `send_app`,
// a data echo on `write`, and `emit_after` (test-only, not part of the real
// protocol) for exercising WaitAbsentTimeout's timing against a genuinely
// delayed, asynchronous event instead of one already sitting in the log.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

func emit(e map[string]any) {
	b, _ := json.Marshal(e)
	fmt.Println(string(b))
}

func main() {
	emit(map[string]any{"event": "ready", "sdk": "fake-sdk", "sdk_version": "0.0.0", "roles": []string{"client"}})

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		var cmd map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &cmd); err != nil {
			continue
		}
		seq, _ := cmd["seq"].(float64)
		name, _ := cmd["cmd"].(string)

		switch name {
		case "set_options":
			emit(map[string]any{"event": "ack", "seq": seq})
		case "connect":
			emit(map[string]any{"event": "ack", "seq": seq})
			emit(map[string]any{"event": "connected", "seq": seq, "session": "fake-session", "welcome": map[string]any{}})
		case "send_app":
			emit(map[string]any{"event": "ack", "seq": seq})
			emit(map[string]any{"event": "app", "body": cmd["body"]})
		case "write":
			emit(map[string]any{"event": "ack", "seq": seq})
			emit(map[string]any{"event": "data", "id": cmd["id"], "data_b64": cmd["data_b64"]})
		case "emit_after":
			// Test-only: ack immediately, then emit {"event": <event>} after
			// delay_ms on its own goroutine, so the event genuinely arrives
			// asynchronously rather than already being in the log by the time
			// a test calls WaitAbsentTimeout.
			emit(map[string]any{"event": "ack", "seq": seq})
			evName, _ := cmd["event"].(string)
			delayF, _ := cmd["delay_ms"].(float64)
			go func(name string, delay time.Duration) {
				time.Sleep(delay)
				emit(map[string]any{"event": name})
			}(evName, time.Duration(delayF)*time.Millisecond)
		case "shutdown":
			emit(map[string]any{"event": "ack", "seq": seq})
			return
		default:
			emit(map[string]any{"event": "error", "seq": seq, "message": "unsupported command " + name})
		}
	}
}
