# ws-mixer: wire framing and encoding

Date: 2026-08-25 · Scope: framing & encoding only. Flow-control algorithm and control-message contents are separate passes.

## Recommendation up front

**One WebSocket *message* = exactly one mux frame. Binary opcode (0x2) always. 8-byte binary header, no length field. Control-channel payloads (stream 0) are UTF-8 JSON.**

```
 offset  size  field       notes
 ------  ----  ----------  -----------------------------------------------
 0       1     type        0x00 OPEN 0x01 DATA 0x02 WINDOW 0x03 CLOSE 0x04 RESET
 1       1     flags       none defined in v1; senders MUST write 0
 2       2     reserved    MUST be 0 on send; receiver MUST ignore
 4       4     stream_id   uint32 big-endian; high bit MUST be 0; 0 = control
 8       ..    payload     WS message length minus 8
```

- `struct.unpack('>BBHI', hdr)` / `DataView.getUint32(4)` / `binary.BigEndian.Uint32(b[4:8])`. No bit-packing, no varints, no alignment traps.
- **No length field.** WebSocket already delimits messages and RFC 6455 §5.4 guarantees *message* boundaries survive intermediaries (only *frame* boundaries may be re-cut). Payload length is `len(message) - 8`.
- **No per-frame version byte.** Version is pinned by the WebSocket subprotocol string (`Sec-WebSocket-Protocol: ws-mixer.v1`) and confirmed in `hello` on stream 0.
- **Max frame payload: 65_536 bytes (hard cap).** Senders SHOULD chunk DATA at **16 KiB** by default.
- **Stream IDs: 31-bit, server-opened only, odd numbers, strictly increasing.** Even space permanently reserved. Exhaustion → server sends `drain` on stream 0 and closes; client reconnects.
- **Control frames = DATA frames on stream 0 with a single JSON object as payload.** No sixth frame type.
- **permessage-deflate: explicitly disabled on both ends.**

Total: DATA overhead is 8 bytes per frame (0.05 % at 16 KiB) plus the WebSocket frame header (2–4 bytes server→client, 6–8 bytes client→server with masking).

---

## 1. Encoding choice

| Option | Verdict |
|---|---|
| **Binary header + opaque payload** (recommended) | 8 bytes/frame, zero deps in Py/JS/Go, trivially parseable. |
| JSON everywhere | Binary payloads need base64 (+33 % on every byte of user traffic) and a JSON parse per chunk. Rejected. |
| Protobuf everywhere | Adds a codegen toolchain + runtime dep in *every* SDK language — directly against "implementable in an afternoon". Rejected. |
| **Hybrid: binary header, JSON for control bodies** (recommended) | Control traffic is a handful of messages per connection, so encoding cost is irrelevant there; JSON is in every stdlib, self-describing, and readable in a hexdump/devtools. msgpack/CBOR/protobuf would buy bytes we don't spend. |

Other calls:

- **Binary opcode, never text.** Stream payloads are opaque bytes; a text frame would force UTF-8 validation and, in browsers, string decoding. Keeping stream-0 JSON also on the binary opcode means the receive loop has exactly one code path. (Cost: `JSON.parse(new TextDecoder().decode(payload))` instead of `JSON.parse(msg)` — one line.)
- **One WS message = one mux frame** is safe. RFC 6455 §5.4: an intermediary "might coalesce and/or split frames … senders and receivers must not depend on the presence of specific frame boundaries" — that is about *fragments within a message*; the message boundary itself is preserved, and every WS library reassembles fragments before handing you a message. Cloudflare "passes frames transparently" ([websocket.org CF guide](https://websocket.org/guides/infrastructure/cloudflare/)).
  - Bonus: because a frame never exceeds 64 KiB, no SDK needs a *streaming* message reader. That matters most for Python, where `websockets` hands you whole messages and streaming reads are awkward.
  - Cost: no batching of many small frames into one WS message. Acceptable; revisit only with measurements (see open questions).
- **Cloudflare**: 100 s idle timeout on Free/Pro, non-configurable ([CF docs](https://developers.cloudflare.com/network/websockets/), [websocket.org](https://websocket.org/guides/infrastructure/cloudflare/)) → an app-level keepalive on stream 0 every 30–60 s is mandatory. Browsers cannot send WS-native ping frames from JS, so the keepalive must be an application message, not a WS ping. No documented message-size cap for *proxied* (non-Workers) WebSockets; the 32 MiB figure people quote is the **Workers** limit ([CF changelog 2025-10-31](https://developers.cloudflare.com/changelog/post/2025-10-31-increased-websocket-message-size-limit/)) and does not apply to plain proxying. Our 64 KiB cap sits far below anything anyone has reported.
- **permessage-deflate: disable explicitly** (`compression=None` in python-websockets, `perMessageDeflate: false` in `ws`, `CompressionDisabled` in coder/websocket).
  - Cloudflare reportedly does not negotiate it at all ([workerd#4091](https://github.com/cloudflare/workerd/issues/4091)) — so we would pay the cost only on direct connections.
  - python-websockets enables it by default at ~**64 KiB of zlib state per connection** ([docs](https://websockets.readthedocs.io/en/stable/topics/compression.html)); on a server holding tens of thousands of tunnels that is gigabytes for nothing.
  - Payload compression, if wanted, belongs to the HTTP layer riding *above* ws-mixer, where it can be content-aware. Negotiation is per-connection so this has zero wire impact.

## 2. Frame layout — comparison

| | HTTP/2 (RFC 9113 §4.1) | yamux | muxado | **ws-mixer (proposed)** |
|---|---|---|---|---|
| Header size | 9 B | 12 B | 8 B | **8 B** |
| Length field | 24-bit | 32-bit | 24-bit | **none** (WS delimits) |
| Version | none (in ALPN/preface) | 8-bit per frame | none | **none** (subprotocol + hello) |
| Type | 8-bit | 8-bit | 4-bit (packed with flags) | **8-bit** |
| Flags | 8-bit | 16-bit | 4-bit | **8-bit** |
| Stream ID | 31-bit (+1 reserved bit) | 32-bit | 31-bit | **31-bit (+1 reserved)** |
| Byte order | big-endian | big-endian | big-endian | **big-endian** |
| Open a stream | HEADERS on a new ID | DATA/WINDOW + SYN flag | DATA + SYN flag | **OPEN frame type** |
| Half-close | END_STREAM flag | FIN flag | FIN flag | **CLOSE frame type** |
| Abort | RST_STREAM frame | RST flag | RST frame | **RESET frame type** |
| Session-level | stream 0 (SETTINGS/PING/GOAWAY frames) | stream 0 (Ping/GoAway types) | GOAWAY frame | **stream 0, JSON in DATA** |

Sources: [RFC 9113 §4](https://www.rfc-editor.org/rfc/rfc9113.html#section-4), [yamux spec.md](https://github.com/hashicorp/yamux/blob/master/spec.md), [muxado frame/common.go](https://github.com/inconshreveable/muxado/blob/master/frame/common.go).

Notes on the deltas:

- **Byte order**: big-endian, unanimously. Not because it's faster — because every reference implementation, every hexdump convention, and `struct('>...')` / `DataView` defaults line up with it.
- **muxado packs type+flags into one byte**; we don't. Saving one byte on a 16 KiB frame is not worth the shift-and-mask in three languages.
- **Separate OPEN/CLOSE/RESET types instead of SYN/FIN/RST flags** (already decided). This trades one extra 8-byte WS message per stream lifecycle event for a spec where each frame type has exactly one meaning. At a few hundred bytes-to-megabytes per stream, the extra ~16 bytes is noise, and it removes the class of bug where an implementer forgets that a zero-length DATA+FIN is a legal thing to receive.
- **Reserved bytes 2–3**: cheap growth room (an error code in the header, a priority, a channel tag) and they make hexdumps align on 8-byte columns. See open question 1 if you'd rather have a 6-byte header.
- **`RESET` payload** should carry a 4-byte error code (big-endian), mirroring yamux GoAway codes / HTTP/2 RST_STREAM. `WINDOW` payload is a 4-byte big-endian increment (muxado `wndinc`). `OPEN` payload: opaque/empty in v1 (metadata belongs in the HTTP layer above, since ws-mixer is HTTP-agnostic).

## 3. Max frame / message size

**Hard cap 64 KiB payload (65_536); recommended sending chunk 16 KiB.** A receiver MUST close the connection (WS status 1009 / protocol error) on a larger message.

Library defaults that constrain the choice:

| Library | Default inbound message limit | Consequence |
|---|---|---|
| Go [coder/websocket](https://github.com/coder/websocket/blob/master/read.go) | **32 KiB** (`defaultReadLimit = 32768`) | must call `SetReadLimit` regardless; keep the number small so it stays a one-liner |
| Go [gorilla/websocket](https://github.com/gorilla/websocket/blob/main/conn.go) | **unlimited** (`readLimit == 0` disables the check) | we MUST set a limit ourselves or we have a memory-exhaustion DoS |
| Node [`ws`](https://github.com/websockets/ws/blob/master/doc/ws.md) | `maxPayload` **100 MiB** | fine; tighten it anyway |
| Python [`websockets`](https://websockets.readthedocs.io/en/stable/reference/asyncio/client.html) | `max_size` **1 MiB**, `max_queue` 16 | **64 KiB fits under the default** — a Python implementer who never touches `max_size` still interoperates |
| Browser `WebSocket` | no author-facing limit | fine |

Why not larger:

- **Head-of-line blocking.** One WS connection = one TCP stream; a frame in flight blocks every other stream's frames behind it. 64 KiB on a 1 Mbps home uplink is ~500 ms of latency injected into every concurrent stream. This is exactly why HTTP/2's `SETTINGS_MAX_FRAME_SIZE` defaults to 2^14 = 16 KiB ([RFC 9113 §4.2](https://www.rfc-editor.org/rfc/rfc9113.html#section-4.2)) — hence our recommended 16 KiB *sending* size, with 64 KiB as the headroom a peer is allowed to use.
- **Memory.** Worst case is (max frame) × (streams with a frame in flight). At 64 KiB × 1000 streams that is 64 MB per connection before flow control even engages; per-stream receive windows will bound this properly in the flow-control pass.
- **No streaming reader needed** in any SDK, as above.

Why not smaller than 64 KiB as the cap: leaves room to raise the default chunk size later (via a hello-negotiated setting) without a version bump, and matches the "buffer one whole message" model every WS library uses.

## 4. Stream ID space

- **31-bit** (`uint32`, high bit reserved and MUST be zero), like HTTP/2 and muxado. yamux uses the full 32 bits; the spare bit costs nothing and buys a flag slot.
- **Stream 0 = control**, session-scoped, never opened or closed.
- **Odd/even split is not needed today** (only the server opens streams) — **but adopt it anyway**: server-opened streams are **odd** (1, 3, 5, …), even IDs permanently reserved. Cost is one bit of ID space; benefit is that client-initiated streams (server-push, agent-initiated RPC to the edge) become a non-breaking extension instead of a v2. This is the yamux ("client odd, server even") and HTTP/2 (§5.1.1) convention.
- **Strictly increasing, never reused.** Reuse creates the classic ambiguity where a late frame from a closed stream lands on a fresh one. HTTP/2 §5.1.1 makes reuse an error; do the same.
- **Exhaustion**: 2^30 ≈ 1.07 × 10^9 odd IDs per connection. At a sustained 1000 streams/s that is ~12 days of continuous connection. Strategy, mirroring HTTP/2 (server sends GOAWAY, client reconnects): when the next ID would exceed the space, the server sends the already-planned **`drain`** control message on stream 0 and closes after a grace period; the client reconnects. **No wrap-around** — wrapping is where the reuse bug lives, and we already own a graceful-drain path for rollouts, so exhaustion is free to handle.

## 5. Versioning

**Version lives in the handshake, not in every frame.**

1. `Sec-WebSocket-Protocol: ws-mixer.v1` on the upgrade. The client offers the versions it supports; the server echoes exactly one. A mismatch fails the HTTP upgrade with a clear status *before* a single frame is exchanged — far better diagnostics than a version byte rejected on frame 1.
2. `hello` on stream 0 confirms the version and carries a **feature/settings map** (max frame size, initial window, optional extensions).

Rationale for dropping the per-frame version byte: yamux carries one and has never incremented it; HTTP/2 carries none (version is ALPN `h2`). A byte that is constant for the life of a connection belongs in the handshake.

Evolution rules (borrowed wholesale from HTTP/2 §4.1 / §5.5):

- **Unknown frame type → ignore and discard.** Combined with feature negotiation in `hello`, a peer never *sends* an extension type the other side didn't advertise, so ignore-and-discard is a belt-and-braces path rather than the primary mechanism.
- **Unknown flag bits → ignore.** Same reasoning. Corollary: **never define a flag that silently changes the meaning of a frame for a peer that ignores it** — semantic changes go behind a negotiated feature bit or a new frame type.
- **Reserved header bytes MUST be zero on send, ignored on receive.** (Not "MUST be zero on receive" — that would make them unusable later.)
- New frame types get new numbers; **numbers are never reused**, even for removed types.
- A genuinely incompatible change gets `ws-mixer.v2` as a new subprotocol string; a server can offer both from the same endpoint during migration.

## 6. Flags

**v1 defines no flags.** The byte exists, MUST be written as 0, and unknown bits are ignored.

- **FIN/END_STREAM as a flag on DATA is deliberately *not* in v1.** With CLOSE as its own frame type (already decided), a FIN flag would give two ways to spell the same event — the single biggest source of "afternoon" turning into "week" for an implementer. Cost of omitting it: one extra 8-byte WS message at the end of each stream direction. Flag bit `0x01` is **reserved for FIN** should measurements later justify it.
- **SYN/ACK equivalents** are unnecessary: OPEN is its own type, and the flow-control pass decides whether an explicit accept/ack is needed at all.
- Flags are the right home only for things that are (a) boolean, (b) meaningful on an *existing* frame type, and (c) safely ignorable by a peer that doesn't know them. FIN fails (c), which is another reason it is a type here rather than a flag.

## Open questions for Anatoly

1. **8-byte header with 2 reserved bytes, or 6-byte minimal** (`type, flags, stream_id`)? 6 bytes is the smallest thing that works and is marginally simpler to describe; 8 bytes aligns hexdumps and leaves growth room. I lean 8.
2. **FIN flag on DATA in v1 after all?** Saves one WS message per stream direction. Recommendation is to defer; worth a decision if the expected workload is many very short streams.
3. **Length field / frame batching.** Adding a 24-bit length would allow packing many small frames (WINDOW, CLOSE, tiny DATA) into one WS message. Real benefit is unmeasured. It also becomes mandatory if ws-mixer is ever to run over a plain byte stream (raw TCP/TLS) rather than WebSocket — is that a stated goal?
4. **Odd-only server stream IDs** — accept halving the ID space to reserve the client-initiated direction, or use all IDs and accept that client-initiated streams would need a v2?
5. **64 KiB cap with a 16 KiB default chunk**, or a flat 16 KiB hard cap (exactly HTTP/2's default)? The two-number version is more flexible but is one more thing in the spec.
6. **Control payload = JSON in DATA frames on stream 0**, or a dedicated CONTROL frame type? JSON-in-DATA keeps the type set at five and lets stream 0 be handled by the ordinary DATA path; a separate type is marginally more self-documenting on the wire.
7. **Unverified**: no Cloudflare documentation states a message-size limit for *proxied* (non-Workers) WebSockets. Worth a 30-minute empirical test through a proxied hostname (send 64 KiB, 1 MiB, 16 MiB messages) before we cite "no limit" anywhere.
8. **WS-native ping/pong in addition to the stream-0 keepalive?** Go/Node/Python clients can send them, browsers cannot. Recommendation is app-level only, for one uniform mechanism — confirm no browser-hosted client is planned to matter here.

## Sources

- [hashicorp/yamux spec.md](https://github.com/hashicorp/yamux/blob/master/spec.md)
- [inconshreveable/muxado frame/common.go](https://github.com/inconshreveable/muxado/blob/master/frame/common.go)
- [RFC 9113 §4 (framing), §4.2 (frame size), §5.1.1 (stream identifiers)](https://www.rfc-editor.org/rfc/rfc9113.html#section-4)
- [RFC 6455 §5.4 (fragmentation and intermediaries)](https://www.rfc-editor.org/rfc/rfc6455#section-5.4)
- [Cloudflare: WebSockets](https://developers.cloudflare.com/network/websockets/) · [Workers WS message size 32 MiB changelog](https://developers.cloudflare.com/changelog/post/2025-10-31-increased-websocket-message-size-limit/) · [workerd#4091 permessage-deflate](https://github.com/cloudflare/workerd/issues/4091) · [websocket.org Cloudflare guide](https://websocket.org/guides/infrastructure/cloudflare/)
- [coder/websocket read.go](https://github.com/coder/websocket/blob/master/read.go) · [gorilla/websocket conn.go](https://github.com/gorilla/websocket/blob/main/conn.go) · [ws docs](https://github.com/websockets/ws/blob/master/doc/ws.md) · [python websockets client reference](https://websockets.readthedocs.io/en/stable/reference/asyncio/client.html) · [python websockets compression](https://websockets.readthedocs.io/en/stable/topics/compression.html)
- [jpillora/chisel](https://github.com/jpillora/chisel) — contrast case: wraps the WebSocket in a `net.Conn` and runs SSH channels over it, so its frames are *not* aligned to WS messages and it needs SSH's own length-prefixed framing. Our one-message-one-frame choice is what lets us drop the length field.
