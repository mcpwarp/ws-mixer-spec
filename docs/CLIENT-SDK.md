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
| Disconnect reason shape | Every disconnect (recoverable or fatal) is reported as one object carrying: `phase` (`"dial"` \| `"handshake"` \| `"connected"`), `wsCode` when a WS close occurred, `errorCode`/name when a ws-mixer error preceded it, `httpStatus` when the upgrade itself failed, `fatal: boolean`, a human-readable `message`, and `closeReason`. |
| `wsCode` | When derived from a ws-mixer `error` this side is closing on (its own caller's code or a peer's), it is the semantic `4000 + code` — even for a `code > 999`, where [WIRE.md §2.8](./WIRE.md#28-error-codes) requires the wire itself to carry the clamped `4002` instead. When observed from a bare close frame (no preceding `error` this side generated), it is exactly what was on the wire. |
| `closeReason` | The reason field of the close frame **received from the peer** — never the SDK's own outgoing reason. It is the bytes as received (at most 123 UTF-8 bytes) — the SDK MUST NOT re-truncate or normalise them. If the underlying library surfaces a reason that is not valid UTF-8, the SDK SHOULD surface a lossy (U+FFFD) decoding rather than drop the field, and MUST NOT treat the bad encoding itself as fatal. SHOULD rather than MUST because some libraries reject an invalid-UTF-8 close frame outright before the SDK ever sees the bytes; that disconnect is then reported as whatever the library surfaces — typically `wsCode: 1007`, or `1006` (abnormal closure) when the library tears the connection down without completing the close handshake — and no `closeReason`. Absent or empty whenever no reason was received from the peer — including when this side initiated the close (a peer's echo carries no information and RFC 6455 doesn't require it to copy the reason), and when the SDK closed on `error` without reading the close frame that followed it, as [WIRE.md §2.7](./WIRE.md#27-control-channel-stream-0) allows ("logs, surfaces and closes"); the human-readable text is then in `message` instead. Consumers SHOULD prefer `closeReason` and fall back to `message`. |
| Handshake-phase close | Both a close frame observed between the 101 upgrade and `welcome`, and the client's own 10 s welcome timeout ([WIRE.md §2.9](./WIRE.md#29-sequences)), are reported with `phase: "handshake"` — not as a dial failure. A received close carries the peer's `wsCode`, and its `closeReason` when the close frame itself was read (see the `closeReason` row). The welcome timeout carries the locally generated `wsCode: 4001` and no `closeReason` (nothing was received from the peer). |
| Application close | The SDK MUST expose a connection-close API taking a ws-mixer error code and message, so the application can close with `APPLICATION_CLOSE` (`0x0e` / 4014). The SDK performs [WIRE.md §2.8](./WIRE.md#28-error-codes)'s three steps: `error{code:14, message}` on stream 0, WS close `4014` with the message truncated to 123 UTF-8 bytes on a character boundary, then close the socket. |
| Fatal set | Unchanged from [WIRE.md §2.9](./WIRE.md#29-sequences): close `4010`, `4011` (only after the one refresh-retry above has already failed), HTTP `403`, HTTP `404`, missing subprotocol echo. Also fatal: a token-provider failure (see above), and — for an SDK that offers a bounded retry count — reconnect attempts exhausted. |
| Handler delivery | Stream, `app`, and `drain` handlers are delivered in wire order from **one** delivery loop per connection, and never concurrently with each other or with themselves — a slow handler must not reorder or overlap deliveries. |
| Stats / counters | The "ignore and count" counters (unknown frame types, stale frames, duplicate pongs, refused opens, protocol violations, bytes in/out) MUST be exposed via some accessor on the client (each SDK documents its own shape — e.g. the JS SDK's `client.stats()`). |

Each language's own implementation notes (WebSocket library choice, exposed types, usage examples) live in
that SDK's own repo — see [`REPOS.md`](./REPOS.md) for the map.
