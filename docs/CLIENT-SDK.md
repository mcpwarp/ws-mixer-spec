# Client SDK requirements

Status: **normative**, language-independent by construction. These are behavioural requirements for
**every** `ws-mixer.v1` client SDK — JS, Python, a Go client, or any future one. They are stated once here
so a new SDK has a checklist independent of any one language's implementation.

Wire-level behaviour (framing, streams, flow control, control messages, error codes) is defined in
[`WIRE.md`](./WIRE.md); this document only adds the client-side behavioural obligations that sit above the
wire and are not implied by it alone.

| Requirement | Rule |
|---|---|
| Reconnect / backoff | Exactly the state machine in [WIRE.md §2.9](./WIRE.md#29-sequences) "Reconnect" — full-jitter backoff, attempt reset only on `welcome`, drain/`4012` handling, fatal-close handling. Not restated here; implement that table. |
| Token provider | `token` MAY be a static string or a callback returning a fresh token (sync or `Promise`). The callback MUST be invoked on **every** dial, not cached across reconnects. |
| 401 on upgrade | The SDK calls the token provider **once more** and retries the dial **immediately** (no backoff). A **second** HTTP 401 is fatal. |
| Provider failure | A token provider that throws or rejects is **fatal**: no retry, the thrown/rejected error is surfaced verbatim to the caller. |
| Disconnect reason shape | Every disconnect (recoverable or fatal) is reported as one object carrying: `phase` (`"dial"` \| `"handshake"` \| `"connected"`), `wsCode` when a WS close occurred, `errorCode`/name when a ws-mixer error preceded it, `httpStatus` when the upgrade itself failed, `fatal: boolean`, and a human-readable `message`. |
| Fatal set | Unchanged from [WIRE.md §2.9](./WIRE.md#29-sequences): close `4010`, `4011` (only after the one refresh-retry above has already failed), HTTP `403`, HTTP `404`, missing subprotocol echo. |
| Handler delivery | Stream, `app`, and `drain` handlers are delivered in wire order from **one** delivery loop per connection, and never concurrently with each other or with themselves — a slow handler must not reorder or overlap deliveries. |
| Stats / counters | The "ignore and count" counters (unknown frame types, stale frames, duplicate pongs, refused opens, protocol violations, bytes in/out) MUST be exposed via some accessor on the client (each SDK documents its own shape — e.g. the JS SDK's `client.stats()`). |

Each language's own implementation notes (WebSocket library choice, exposed types, usage examples) live in
that SDK's own repo — see [`REPOS.md`](./REPOS.md) for the map.
