// Command fakeadapter is a minimal, hand-written stand-in for a real SDK
// adapter (docs/CONFORMANCE.md section 1), used only by adapter_test.go to
// exercise the driver (Spawn/Send/WaitFor/SendAndAck/Kill) without needing a
// real SDK built. It implements just enough of the protocol to be useful:
// ready, ack, a canned "connected" on `connect`, an app echo on `send_app`,
// and a data echo on `write`.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
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
		case "shutdown":
			emit(map[string]any{"event": "ack", "seq": seq})
			return
		default:
			emit(map[string]any{"event": "error", "seq": seq, "message": "unsupported command " + name})
		}
	}
}
