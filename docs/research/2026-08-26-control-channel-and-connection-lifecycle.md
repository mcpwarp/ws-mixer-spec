# ws-mixer: control channel and connection lifecycle

Date: 2026-08-26 · Pass 3. Builds on
[pass 1: framing and encoding](./2026-08-25-framing-and-encoding.md) (8-byte header, one WS message = one frame, stream 0 = control, 64 KiB cap, odd server-opened ids, `Sec-WebSocket-Protocol: ws-mixer.v1`) and
[pass 2: flow control and stream lifecycle](./2026-08-25-flow-control-and-stream-lifecycle.md) (per-stream credit window, error-code table, `4000 + code` close codes).

Anatoly's rulings carried in from pass 2: credit violation is connection-fatal; defaults are window 256 KiB and `max_streams` 64, both advertised in `hello` and tunable; metrics from day one; late frames on dead streams are discarded with a debug log, frames for never-opened ids kill the connection.

---

## Summary

### Control message set (stream 0, one JSON object per DATA frame)

| `t` | Direction | When | Required fields | Optional fields |
|---|---|---|---|---|
| `hello` | C→S | first frame, always | `t`, `v`, `token`, `agent` | `window`, `max_streams`, `meta` |
| `welcome` | S→C | reply to `hello` | `t`, `v`, `session`, `window`, `max_streams`, `ping_interval`, `ping_timeout` | `server`, `meta` |
| `ping` | both | every `ping_interval` | `t`, `id` | `ts` |
| `pong` | both | immediately on `ping` | `t`, `id` | `ts` |
| `drain` | S→C | rollout / id exhaustion / idle evict | `t`, `reason`, `last_stream_id` | `deadline_ms`, `message`, `retry_after_ms` |
| `error` | both | connection-fatal, last frame before close | `t`, `code`, `message` | `stream_id`, `last_stream_id` |

Six types. No `register`, no `goaway`, no `settings` — arguments in §1.

```json
{"t":"hello","v":1,"token":"eyJhbGciOi…","agent":{"sdk":"ws-mixer-js","sdk_version":"0.3.1","runtime":"node/22.4.0","os":"darwin/arm64"},"window":262144,"max_streams":64,"meta":{"services":[{"id":"anki","name":"Anki MCP"}]}}
{"t":"welcome","v":1,"session":"01J8Z2K9QF3M4N5P6R7S8T9V0W","window":262144,"max_streams":64,"ping_interval":30000,"ping_timeout":90000,"server":{"name":"mcpwarp-edge","version":"1.4.2","node":"edge-7c9f-bkl2p"},"meta":{"public_url":"https://anki-3f9k.mcpwarp.io"}}
{"t":"ping","id":42,"ts":1756137600123}
{"t":"pong","id":42,"ts":1756137600123}
{"t":"drain","reason":"rollout","last_stream_id":1287,"deadline_ms":25000,"message":"edge-7c9f-bkl2p is shutting down; reconnect now"}
{"t":"error","code":3,"message":"stream 41: received 20480 DATA bytes with 8192 credit remaining","stream_id":41,"last_stream_id":1287}
```

### Handshake

```
client                          Cloudflare                        server
  |  GET /tunnel  Upgrade: websocket                                |
  |  Sec-WebSocket-Protocol: ws-mixer.v1                            |
  |  Authorization: Bearer <token>          ------------------->    |
  |                                          101 Switching Protocols
  |  <---------------------------------- Sec-WebSocket-Protocol: ws-mixer.v1
  |                                                                 |
  |  DATA(stream 0) {"t":"hello",…}         ------------------->    |  start 10 s hello timer
  |  <---------------------------- DATA(stream 0) {"t":"welcome",…} |
  |                                                                 |
  |  ==== streams may now be opened by the server (OPEN id=1) ====  |
  |  <-------------------------------------- OPEN(1) DATA(1) …      |
  |  ping/pong on stream 0 every 30 s, both directions               |
```

Rules: nothing but `hello` may be sent by the client before `welcome`; nothing at all may be sent by the server before `welcome`; any violation is connection `PROTOCOL_ERROR` (4001). Auth is checked at HTTP upgrade **and** re-checked against `hello.token`; failure → `error{code:0x0b}` + close `4011`.

### Keepalive numbers

| Knob | Value | Why |
|---|---|---|
| Mechanism | **in-band `ping`/`pong` on stream 0** (WS-native ping/pong optional, additive) | uniform across Go/Node/Python/browser; observable; measures app-loop liveness, not just TCP |
| `ping_interval` | **30 000 ms**, server-assigned in `welcome` | 3× margin under the ~100 s idle cut Cloudflare **observably** applies but has never documented (§3.1); matches the AnkiMCP production data point and yamux's `KeepAliveInterval` |
| `ping_timeout` | **90 000 ms** (3 missed) | one lost ping must not kill a healthy tunnel |
| Who pings | **both directions, independently** | each side needs its own liveness verdict; the message is symmetric |
| On timeout | `error{code:0x0d KEEPALIVE_TIMEOUT}` + close `4013`, then reconnect after one immediate attempt | pass-2 code table |
| WS-native ping | client-side libraries MAY leave their own ping on; it is not part of the protocol contract | browsers cannot send them; Python `websockets` sends them by default and that is fine |

### Drain

```
SIGTERM ─► preStop sleep(5s, let endpoints drop) ─► drain{reason:"rollout", last_stream_id:N, deadline_ms:25000}
        ─► server opens no new streams
        ─► in-flight streams run to completion, or until deadline
        ─► at deadline: RESET(CANCEL) every survivor
        ─► error{code:0x0c GOING_AWAY} + WS close 4012 "draining"
        ─► client reconnects IMMEDIATELY, zero backoff, jitter 0–2 s
terminationGracePeriodSeconds: 45   (5 preStop + 25 drain + 15 slack)
```

### Backoff

| Parameter | Value |
|---|---|
| Policy | full jitter: `sleep = random(0, min(cap, base * 2^attempt))` |
| `base` | 1 000 ms |
| `cap` | 60 000 ms |
| Reset | on a successful `welcome` (not on TCP connect) |
| Drain (`4012`) | **no backoff** — reconnect after `random(0, 2000) ms` of jitter only |
| Keepalive timeout (`4013`) | one immediate retry, then normal backoff |
| `ENHANCE_YOUR_CALM` (`4009`) | start at `cap`; honour `retry_after_ms` if present |
| Fatal — never reconnect | `4010 UNSUPPORTED`, `4011 UNAUTHORIZED`, HTTP 401/403 on upgrade, subprotocol not echoed |

### WS close codes (complete)

| Code | Name | Sent by | Client action |
|---|---|---|---|
| 1000 | normal (`NO_ERROR`) | both | normal backoff |
| 1001 | going away (raw WS layer, no `error` seen) | both | treat as `4012` |
| 1006 | abnormal (no close frame) | — | normal backoff |
| 1009 | message too big (WS library rejected before us) | both | backoff + loud error |
| 4001 | `PROTOCOL_ERROR` | both | backoff + **loud developer error** |
| 4002 | `INTERNAL_ERROR` | both | backoff |
| 4003 | `FLOW_CONTROL_ERROR` | both | backoff + loud developer error |
| 4004 | `FRAME_SIZE_ERROR` | both | backoff + loud developer error |
| 4009 | `ENHANCE_YOUR_CALM` | both | long backoff |
| 4010 | `UNSUPPORTED` | server | **stop**, surface to developer |
| 4011 | `UNAUTHORIZED` | server | **stop**, surface to user |
| 4012 | `GOING_AWAY` (drain complete) | server | **reconnect immediately** |
| 4013 | `KEEPALIVE_TIMEOUT` | both | immediate retry once, then backoff |
| 4014 | `REPLACED` (same identity reconnected) | server | **stop** — another connection owns this identity |

`4000 + error_code` from the pass-2 table, plus `4014` claiming code `0x0e`. All inside RFC 6455 §7.4.2's private-use range 4000–4999, so no collision with 1000–2999 (protocol) or 3000–3999 (libraries/frameworks). Reason string ≤ **123 bytes of UTF-8** (§5.5 caps the whole control payload at 125; 2 bytes are the code).

---

## 1. The control message set

### 1.0 Envelope rules

- One JSON **object** per stream-0 DATA frame. Never an array, never a bare scalar, never two objects in one frame.
- UTF-8, no BOM. `t` is the discriminator and MUST be the first field on send (readers must not depend on order, but it makes `tcpdump` legible).
- Payload ≤ 16 KiB (pass 2 §6). Over → connection `ENHANCE_YOUR_CALM` (4009).
- Malformed JSON, non-object, missing `t`, or `t` not a string → connection `PROTOCOL_ERROR` (4001) with a message naming the offending field.
- **Unknown fields inside a known `t` are ignored** (forward compatibility). **Unknown `t` values are a connection `PROTOCOL_ERROR`** — with one escape hatch, §1.7.
- Timestamps (`ts`) are integer milliseconds since the Unix epoch. Durations are integer milliseconds and always carry a `_ms` suffix. Byte counts are integers with no suffix. No floats anywhere in the control channel — floats are where JSON parsers disagree.

### 1.1 `hello` (client → server)

| Field | Type | Req | Notes |
|---|---|:--:|---|
| `t` | `"hello"` | ✔ | |
| `v` | integer | ✔ | must be `1`; must match the negotiated subprotocol. Mismatch → `UNSUPPORTED` (4010) |
| `token` | string | ✔ | bearer credential; 1–4096 chars. See §2.2 for why it is here *and* in the header |
| `agent` | object | ✔ | `{sdk, sdk_version, runtime, os}` — `sdk` and `sdk_version` required, rest optional. Free-form strings ≤ 128 chars |
| `window` | integer | | this peer's **receive** window in bytes, `16384 … 2^31-1`. Default 262144 |
| `max_streams` | integer | | streams this peer will accept concurrently, `1 … 100000`. Default 64 |
| `meta` | object | | **opaque to ws-mixer**; handed to the application verbatim. This is where mcpwarp puts its service registration |

`agent` is required, not optional, and this is deliberate: when a third-party SDK misbehaves the server's log line must say *which SDK and which version*. It costs the implementer one hard-coded literal and it is the single highest-value diagnostic field in the protocol.

### 1.2 `welcome` (server → client)

| Field | Type | Req | Notes |
|---|---|:--:|---|
| `t` | `"welcome"` | ✔ | |
| `v` | integer | ✔ | the version the server accepted; `1` |
| `session` | string | ✔ | server-generated connection id, ≤ 64 chars. Appears in every server log line and in `X-Mcpwarp-Session` on proxied requests. ULID recommended |
| `window` | integer | ✔ | the server's own receive window |
| `max_streams` | integer | ✔ | the **effective** cap — `min(client's request, server policy)`. The client MUST use this number, not the one it asked for |
| `ping_interval` | integer ms | ✔ | how often each side sends `ping`. The server dictates it; the client obeys |
| `ping_timeout` | integer ms | ✔ | how long without a `pong` before the connection is dead. MUST be ≥ 2 × `ping_interval` |
| `server` | object | | `{name, version, node}` — the mirror of `agent`, for the client's logs |
| `meta` | object | | opaque; mcpwarp puts assigned public URLs here |

**Why `welcome` and not `hello_ack`.** Nothing; it is a coin flip. `welcome` reads better in logs and avoids the "did you mean the ack of the ack" confusion when someone later adds a third handshake message. Pick one and never rename it.

**Why the server dictates limits.** `max_streams` and `ping_interval` are the two knobs where a misconfigured client can hurt the server, so the server gets the last word. `window` is per-direction (pass 2 §1.3) so each side simply states its own; there is nothing to negotiate.

### 1.3 `ping` / `pong`

| Field | Type | Req | Notes |
|---|---|:--:|---|
| `t` | `"ping"` \| `"pong"` | ✔ | |
| `id` | integer | ✔ | `0 … 2^53-1`, monotonically increasing **per sender**. Two independent sequences, one per direction |
| `ts` | integer ms | | sender's clock at send time. Echoed **verbatim** in the `pong` |

Rules:
- A receiver MUST reply to `ping` with `pong` carrying the **same** `id` and the **same** `ts`, ahead of any queued DATA (pass 2 §1.4 rule 3: control jumps the queue).
- A `pong` whose `id` was never sent → connection `PROTOCOL_ERROR`. A duplicate `pong` → ignore + count (a retransmitting proxy is not a protocol violation worth killing a tunnel over).
- RTT = `now - ts` on the pinger's own clock. Echoing `ts` rather than keeping a local table means the pinger needs no state beyond "highest id sent, highest id ponged" — which matters for a from-scratch Python implementation. Clock skew is irrelevant because only the pinger ever subtracts.
- `pong` is never sent unsolicited. (RFC 6455 §5.5.3 allows unsolicited WS pongs as a one-way heartbeat; we do not, because our `pong` carries a required `id`.)

This is HTTP/2 PING (RFC 9113 §6.7 — opaque payload echoed with the ACK flag) and yamux's Ping (a 32-bit ping id echoed back), restated in JSON. The one deviation is that we use an explicit `pong` type rather than an ACK flag on `ping`, for the same reason pass 1 chose separate frame types over flags: one name, one meaning.

### 1.4 `drain` (server → client)

| Field | Type | Req | Notes |
|---|---|:--:|---|
| `t` | `"drain"` | ✔ | |
| `reason` | enum string | ✔ | `rollout` \| `overload` \| `id_exhausted` \| `replaced` \| `maintenance` \| `client_requested` |
| `last_stream_id` | integer | ✔ | the highest stream id the server has opened or will open. Everything above it was definitely never processed |
| `deadline_ms` | integer | | how long in-flight streams have before they are reset. Absent = "immediately" |
| `retry_after_ms` | integer | | hint for when to reconnect. Absent = reconnect immediately with jitter |
| `message` | string | | human text for the client's log, ≤ 256 chars |

- After sending `drain` the server MUST NOT send `OPEN`. It MAY keep sending DATA/WINDOW/CLOSE/RESET on already-open streams.
- After receiving `drain` the client SHOULD begin reconnecting *in parallel* (§5.1) so the new connection is warm before the old one dies.
- `reason` is a closed enum. An unknown value is **not** fatal — treat it as `maintenance` and count it. (This is the one place where a new value must not break old clients: rollout reasons are exactly the kind of thing that gets extended.)
- The client MAY send `drain` with `reason: "client_requested"` for a graceful shutdown of its own (§4.4). It is the only message the client is allowed to send in the server→client column, and only with that reason.

### 1.5 `error` (both directions)

| Field | Type | Req | Notes |
|---|---|:--:|---|
| `t` | `"error"` | ✔ | |
| `code` | integer | ✔ | from the pass-2 error-code table |
| `message` | string | ✔ | human-readable, ≤ 1024 chars, **never** just the code name. Say what was received and what was expected |
| `stream_id` | integer | | the stream that triggered it, if the error is attributable |
| `last_stream_id` | integer | | as in `drain`; lets the peer retry unprocessed streams elsewhere |

`error` is always the **last** message the sender puts on the wire, immediately followed by a WS close with `4000 + code`. A receiver that gets `error` MUST NOT reply with another `error` (loop prevention, RFC 9113 §5.4.2); it logs, surfaces, and closes.

The `message` field is the whole reason this design carries JSON rather than a 4-byte code. Pass 2 already made the case; the rule for implementers is: **`message` must contain at least one number that came off the wire.** `"flow control error"` is useless; `"stream 41: DATA of 20480 bytes with 8192 credit remaining"` ends the debugging session.

### 1.6 What is *not* in the set, and why

| Rejected | Why |
|---|---|
| `register` | See §1.6.1 — it goes in `hello.meta` |
| `goaway` | `drain` already carries `last_stream_id`, and `error` carries it too. A third message with the same field is redundancy, not clarity |
| `settings` (mid-connection) | Pass 2 fixed windows for the life of the connection. Nothing is left to renegotiate |
| `close`/`bye` | The WS close frame is the close. A JSON one would be a second way to spell the same event |
| `log`/`notice` | Application concern. If mcpwarp wants server→client notices it opens a stream, or uses `meta` |

#### 1.6.1 `register`: drop it, use `hello.meta`

The tunnel-architecture pass says mcpwarp needs "one long-lived token authenticating the **agent connection**; each exposed MCP server registers in-band with its own logical ID (`RegisterService{id,name}`), validated against the agent's token". So registration is real — the question is whether ws-mixer owns it.

It should not, for three reasons:

1. **ws-mixer is HTTP-agnostic and app-agnostic by construction** (pass 1 §2 kept OPEN payload-free for exactly this reason). A `register` message would put mcpwarp's service model into a byte-stream multiplexer, and the second consumer of ws-mixer would have to ignore it.
2. **Registration must complete before the first stream arrives.** If it is a separate message, the spec now needs a *third* handshake state ("welcomed but not registered") and a rule for what happens if a stream arrives in it. Putting it in `hello.meta` makes the atomicity free: the server has the registration before it sends `welcome`, and `welcome.meta` is the natural place to return assigned URLs.
3. **The failure mode is already covered.** A rejected registration is `error{code: UNAUTHORIZED|UNSUPPORTED}` + close, which the client already handles as fatal.

The cost is that `meta` is schema-less to ws-mixer, so a malformed registration is caught by mcpwarp's own validator rather than the wire validator. That is fine and in fact correct — but it means **mcpwarp must ship its own JSON Schema for `hello.meta`** (§7.4), or Anatoly's "wtf my code doesn't work" bug simply moves one layer up.

Concretely, for mcpwarp:

```json
{"t":"hello","v":1,"token":"…","agent":{"sdk":"ws-mixer-py","sdk_version":"0.2.0"},
 "meta":{"mcpwarp":{"v":1,"services":[{"id":"anki","name":"Anki MCP","transport":"streamable-http"}]}}}
```

If dynamic registration (adding a service without reconnecting) is ever needed, it is a new control message added behind a capability — which is exactly what §1.7 exists for. Deferring it costs nothing today.

### 1.7 Unknown `t`: error, with a negotiated escape hatch

The strict rule and forward compatibility genuinely conflict. The resolution both HTTP/2 and this design already use is **negotiate, then be strict**:

- v1 defines exactly six `t` values. Anything else → connection `PROTOCOL_ERROR` (4001), message `unknown control message type "xyz"`.
- `hello` and `welcome` MAY carry `capabilities`: an array of short lowercase strings. A peer MUST NOT send a message type outside the v1 six unless the *other* peer listed the capability that defines it. So a v1.3 server talking to a v1.0 client never sends the new type, and the strict rule never fires in practice.
- `capabilities` itself is optional and absent means "none", so a v1.0 implementation that has never heard of it is correct.

| Alternative | Verdict |
|---|---|
| Ignore unknown `t` silently (HTTP/2's rule for unknown *frame types*, RFC 9113 §5.5) | Rejected. Against the "no silent drops" requirement, and it hides the most common real cause: a version-skewed SDK talking to the wrong endpoint. The h2 rule exists for *intermediaries* forwarding frames they do not understand — we have no such role |
| Error always, no capabilities | Rejected. Makes every additive change a v2 subprotocol bump |
| Ignore-and-count with a warning log | Tempting middle ground. Rejected because "warning in a log nobody reads" is how the wtf-bugs happen |

Same rule applies one level down: an unknown value in a **closed enum** (`drain.reason`) is tolerated; an unknown *message type* is not. The distinction is that a tolerated enum value degrades to a known default, whereas an unknown message type means an action was requested and silently not taken.


---

## 2. Handshake

### 2.1 The upgrade request

```http
GET /v1/tunnel HTTP/1.1
Host: edge.mcpwarp.io
Upgrade: websocket
Connection: Upgrade
Sec-WebSocket-Version: 13
Sec-WebSocket-Key: <base64>
Sec-WebSocket-Protocol: ws-mixer.v1
Authorization: Bearer <token>
User-Agent: ws-mixer-py/0.2.0 (python/3.12 linux)
```

| Element | Requirement |
|---|---|
| `Sec-WebSocket-Protocol` | client MUST offer at least `ws-mixer.v1`; may offer several, most-preferred first |
| Server response | MUST echo exactly one offered token. **If the response omits the header, the client MUST treat the connection as failed and close** — an echoed-nothing upgrade means a proxy or a wrong endpoint answered, and it is the single cheapest way to catch "I pointed my SDK at the dashboard URL" |
| No overlap | server fails the upgrade with **HTTP 400** and a JSON body, not a 101-then-close |
| `Authorization` | `Bearer <token>`, same token as `hello.token` |
| Path | server-chosen and versioned (`/v1/tunnel`); the subprotocol is the real version gate, the path is for routing |

### 2.2 Auth: header *and* `hello`, not one or the other

| Option | Pros | Cons |
|---|---|---|
| Header only | rejected before the 101 → cheap, standard HTTP 401, works with any HTTP-layer auth middleware, never appears in a WS frame | browsers cannot set headers on `new WebSocket()`; the token lands in access logs of every intermediary that logs headers |
| `hello` only | works from a browser; token never in a header | requires accepting the socket first (resource cost per unauthenticated connection); no standard 401; auth failure has to be reported as a WS close |
| **Both (recommended)** | fast rejection at the edge, and the in-band copy is what the session is actually bound to | one extra field |

**Recommendation: require the `Authorization` header, and require `hello.token` to be present and to match.** Rationale:

- mcpwarp's clients are Node, Python and Go CLIs/SDKs — all can set headers (`ws` `headers:` option, python-`websockets` `additional_headers=`, coder/websocket `HTTPHeader`). The browser limitation is real but no browser client is planned, and if one ever appears it can send the token only in `hello` (a negotiated relaxation, not a redesign).
- Cloudflare passes arbitrary request headers through a proxied WebSocket upgrade, so the header survives the edge.
- **Do not use `Sec-WebSocket-Protocol` to smuggle the token.** It is the well-known browser workaround (`new WebSocket(url, ["ws-mixer.v1", token])`) and it is bad here: it collides with our version negotiation, puts the credential in a header that proxies log and cache, and requires the server to echo one of the offered values — meaning the server echoes the token back. Explicitly forbidden.
- The `hello.token` copy costs nothing and buys two things: the session is bound to a credential the *application* validated (not just the ingress), and a future rotation/refresh message has an obvious place to live.
- Query-string tokens (`?token=…`) are forbidden for the usual reason: they land in every access log and referrer.

### 2.3 State machine before `welcome`

Server side:

| Event | Action |
|---|---|
| Upgrade with no acceptable subprotocol | HTTP 400, no 101 |
| Upgrade with bad/absent `Authorization` | **HTTP 401**, no 101. (Not a 101-then-4011 — an HTTP status is what every client library, curl and load balancer already understands) |
| 101 sent, then any frame that is not DATA on stream 0 | `error{PROTOCOL_ERROR}` + close 4001 |
| 101 sent, then valid JSON with `t != "hello"` | `error{PROTOCOL_ERROR}` + close 4001 |
| `hello.v` not supported | `error{UNSUPPORTED}` + close 4010, message lists supported versions |
| `hello.token` absent, malformed, or ≠ header token | `error{UNAUTHORIZED}` + close 4011 |
| Token valid at upgrade but rejected by the application (e.g. bad `meta` registration) | `error{UNAUTHORIZED}` or `{UNSUPPORTED}` + matching close |
| **No `hello` within 10 s of the 101** | `error{PROTOCOL_ERROR}`, message `hello timeout`, close 4001 |
| Second `hello` on the same connection | `error{PROTOCOL_ERROR}` + close 4001 |

Client side:

| Event | Action |
|---|---|
| 101 without an echoed subprotocol | close 1002, fatal, do not retry |
| Any frame before `welcome` other than DATA-on-0 | `error{PROTOCOL_ERROR}` + close 4001 |
| First control message is not `welcome` | `error{PROTOCOL_ERROR}` + close 4001 |
| **No `welcome` within 10 s** | close 4001, retry with backoff (this one is *not* fatal — it is usually a slow or wedged edge, not a bug) |
| `welcome.max_streams` > what the client offered | accept the *smaller* of the two; do not error. (`welcome` is authoritative, but a client is entitled to its own resource ceiling) |
| `welcome.ping_timeout` < 2 × `ping_interval` | `error{PROTOCOL_ERROR}` + close 4001 — a server that asks for a 1-missed-ping death is misconfigured and will flap |

**Why 10 s for the hello timeout**: it must exceed the worst-case cold start of a Python SDK plus a Cloudflare RTT, and be far below the observed ~100 s idle cut so the timer never races with the proxy. yamux uses 75 s for its analogous `StreamOpenTimeout`, which is far too generous for a first-frame timer that exists to reap half-open sockets.

**One-round-trip cost.** The client cannot open a stream anyway (only the server opens streams), so waiting for `welcome` costs the client nothing. The server must wait one RTT before its first `OPEN` — ~50–150 ms through Cloudflare, once per connection lifetime. Not worth optimising with an optimistic-open scheme.


---

## 3. Keepalive

### 3.1 Correction to pass 1

Pass 1 stated "Cloudflare: 100 s idle timeout on Free/Pro, non-configurable". **That number is not documented by Cloudflare and the plan split is fabricated.** The current [WebSockets page](https://developers.cloudflare.com/network/websockets/) (updated 2026-08-14) says only:

> "Cloudflare will close a WebSocket connection when no data is transmitted in either direction for a period of time. Enterprise customers can contact their account team to configure a custom idle timeout. To keep long-lived connections alive during periods of inactivity, implement a client-side heartbeat (ping/pong) mechanism."

No value, ever — the archived support KB never gave one either. The 100 s figure is real *as an empirical observation*, reported independently by [Teleport](https://github.com/gravitational/teleport/discussions/11762) ("terminate … after 100 seconds of inactivity"), [Buildbot](https://github.com/buildbot/buildbot/issues/4078) ("resetting websockets with 100 seconds of inactivity", fixed with a 60 s client keepalive) and [MQTTnet](https://github.com/dotnet/MQTTnet/issues/1691). There is also a community thread reporting drops at **20 s** through Cloudflare Tunnel, which is why 60 s is too close to the edge.

**Treat 100 s as a working assumption, not a contract.** Design margin accordingly.

Two related corrections, both making the picture *simpler*:

| Myth | Reality |
|---|---|
| "Cloudflare's 100 s proxy read timeout kills WebSockets" | Proxy Read Timeout is **125 s**, Enterprise-configurable up to 6000 s, and it governs *waiting for an HTTP response* — it fires pre-101. Once upgraded, WS lifetime is a separate idle timer. Raising `proxy_read_timeout` is **not** the lever ([connection limits](https://developers.cloudflare.com/fundamentals/reference/connection-limits/), [error 524](https://developers.cloudflare.com/support/troubleshooting/http-status-codes/cloudflare-5xx-errors/error-524/)) |
| "cloudflared's `keepAliveTimeout: 90s` drops the WebSocket" | It maps to Go's `http.Transport.IdleConnTimeout`, which only reaps **pooled, unused** connections. A hijacked WebSocket is in-use and exempt ([ingress/origin_service.go](https://github.com/cloudflare/cloudflared/blob/master/ingress/origin_service.go), [origin parameters](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/configure-tunnels/origin-parameters/)) |

And a useful non-problem: the cloudflared↔edge QUIC link pings every 1 s against a 5 s idle timeout ([quic/constants.go](https://github.com/cloudflare/cloudflared/blob/master/quic/constants.go): `MaxIdleTimeout = 5s`, `MaxIdlePingPeriod = 1s`), so that hop is self-keepaliving and can be ruled out of any WS-drop investigation.

Finally, from the same CF page: **"When Cloudflare releases new code to its global network, we may restart servers, which terminates WebSockets connections."** No heartbeat prevents this. Reconnect is not an error path, it is the normal operating mode — which is the whole justification for §5.

### 3.2 In-band `ping` on stream 0, not RFC 6455 ping

| | WS-native ping/pong (RFC 6455 §5.5.2/5.5.3) | **In-band `ping` on stream 0** |
|---|---|---|
| Sendable from a browser | **No** — the WHATWG `WebSocket` interface has no ping method ([spec](https://html.spec.whatwg.org/multipage/web-sockets.html#the-websocket-interface); [whatwg/html#4353](https://github.com/whatwg/html/issues/4353) closed as declined) | yes |
| Observable by app code | Node `ws`: yes (`'ping'`/`'pong'` events). coder/websocket: yes (`OnPingReceived`/`OnPongReceived`). gorilla: yes (`SetPongHandler`). **Python `websockets`: no callback API at all** | always |
| Answered automatically | yes, by every library — which is the problem: **it proves the library is alive, not that the read loop and application are** | the SDK's own dispatcher answers it, so a wedged dispatcher fails the check |
| Consistent defaults | **no** — Python `websockets` pings every **20 s** with a **20 s** timeout by default and fails the connection with **close 1011** ([`asyncio/connection.py`](https://github.com/python-websockets/websockets/blob/main/src/websockets/asyncio/connection.py), [keepalive docs](https://websockets.readthedocs.io/en/stable/topics/keepalive.html)); Go coder/websocket and gorilla and Node `ws` do **nothing** unless you write the loop | one number in `welcome`, obeyed by all |
| RTT measurement | Python exposes `connection.latency`; Go/Node do not | uniform, from the echoed `ts` |
| Counts as CF activity | almost certainly (post-101 the proxy sees an opaque byte stream, and CF's own advice is "implement a heartbeat (ping/pong)") — but **not documented** | certainly; it is a data frame |

**Decision: the protocol's keepalive is the in-band `ping`/`pong` on stream 0. WS-native ping is out of scope and neither required nor forbidden.**

The deciding arguments:

1. **Uniformity.** The library table above is a mess. A spec that says "use your library's ping" produces a Go peer that never pings talking to a Python peer that kills the connection at 20 s. A spec that says "send this JSON every N ms" produces identical behaviour in three languages, and the SDK author can read the whole rule in one sentence.
2. **It tests the right thing.** A WS pong is emitted by the library's frame layer. Our `ping` is answered by the SDK's own control dispatcher, so it fails if the read loop is blocked, the writer is deadlocked, or the event loop is starved — which are the realistic failure modes for a single-threaded Python SDK on a user's laptop. This is the same reason HTTP/2 has an application-visible PING (RFC 9113 §6.7) despite TCP keepalives existing.
3. **Certainty against the proxy.** CF's control-frame transparency is inference, not documentation. A data frame is unambiguous.
4. **Browser-compatible by construction**, at zero cost, should a browser client ever matter.

Cost: ~50 bytes on the wire every 30 s, and one JSON parse. Irrelevant.

**What SDKs should do with their library's own ping:**

| SDK | Recommendation |
|---|---|
| Python `websockets` | **`ping_interval=20, ping_timeout=None`.** Keep the pings (free extra proxy activity, and `connection.latency` for metrics) but disable the library's own death timer so there is exactly **one** liveness authority — ours. Leaving `ping_timeout=20` gives you a second, stricter, invisible watchdog that will close with 1011 and produce a bug report you cannot correlate with anything |
| Node `ws` | leave `ws.ping()` unused; do not implement the README `isAlive` pattern. One mechanism |
| Go coder/websocket | no ping loop. Note `Ping(ctx)` requires a concurrent reader and any context expiry **closes the whole connection** rather than failing one op — do not wire it to a per-op timeout |
| Go gorilla | no ping loop. If used at all, `WriteControl` is the concurrency-safe writer |
| Browser (hypothetical) | nothing to do; in-band ping already works |

### 3.3 Numbers

| Knob | Value | Reasoning |
|---|---|---|
| `ping_interval` | **30 000 ms** | ≥3× margin under the observed 100 s cut. AnkiMCP runs a binary WS through the same Cloudflare setup with a 30 s heartbeat in production — a direct, load-bearing data point. Also yamux's `KeepAliveInterval` default |
| `ping_timeout` | **90 000 ms** | 3 missed pings. Two would be 60 s, which on a congested home uplink with a 16 KiB write queue ahead of the pong is a realistic false positive. Three costs an extra 30 s of detection latency and removes the flap |
| Direction | **both, independently** | Each side maintains its own `last_pong_at`. The server needs to reap dead tunnels to free routing-table entries; the client needs to notice a black-holed connection to start reconnecting. Neither can be derived from the other |
| Adjustable | server sets both in `welcome`; client MUST obey; server MUST NOT change them mid-connection | one authority, no renegotiation (consistent with pass 2's fixed windows) |
| Floor | server MUST NOT set `ping_interval` < 5 000 ms; client MUST reject a smaller value with `PROTOCOL_ERROR` | a hostile/misconfigured server must not be able to make a client spin |
| Alignment | jitter the first ping by `random(0, ping_interval)` | prevents a thundering herd of pings after a mass reconnect |

**On timeout**: `error{code: 0x0d KEEPALIVE_TIMEOUT}` + close `4013`. The client then reconnects with **one immediate attempt** (the most common cause is a CF server restart, and the new connection will land instantly) and normal backoff thereafter.

**The dead-peer race.** A peer that has sent `drain` and is finishing streams still owes pongs. Keepalive stays armed until the socket closes. Conversely, do not treat "no `pong`" as fatal while your own writer is blocked on backpressure — measure the timeout from `last_pong_received`, not from `last_ping_sent`, so a slow link degrades into latency rather than a disconnect loop.

---

## 4. Graceful drain

### 4.1 Prior art

| System | Mechanism | Verdict |
|---|---|---|
| **HTTP/2** ([RFC 9113 §6.8](https://www.rfc-editor.org/rfc/rfc9113.html#section-6.8)) | GOAWAY with `Last-Stream-ID` + error code + debug data. Two-GOAWAY dance: first `2^31-1`/NO_ERROR, wait ≥1 RTT, then a real last-stream-id | The model. `Last-Stream-ID` is the load-bearing part |
| **muxado** ([session.go](https://github.com/inconshreveable/muxado/blob/master/session.go)) | GoAway carries `lastId`; receiver closes every locally-opened stream `> lastId` with a distinguishable `remoteGoneAway` error | The receiver side in ~10 lines. Copy it |
| **yamux** ([session.go](https://github.com/hashicorp/yamux/blob/master/session.go)) | GoAway with a 3-value code and **no last-stream-id**. Sets `remoteGoAway`; new opens fail with `ErrRemoteGoAway`; existing streams keep running; conn is not closed | Too weak — the receiver learns "stop opening" but cannot tell which in-flight opens were processed, so nothing is safely retryable |
| **frp** ([client/service.go](https://github.com/fatedier/frp/blob/dev/client/service.go)) | `GracefulClose(d)` = close proxies, `time.Sleep(d)`, hard close. Wired only for KCP/QUIC, with `d = 500 ms` | The anti-pattern: a fixed sleep, no round trip, no per-stream accounting |
| **chisel** | `CHISEL_SHUTDOWN_GRACE` 5 s HTTP drain; no tunnel-level drain signal | Nothing to copy |
| **ngrok** | Documented only as prose; the server can "explicitly instruct" the agent to stop reconnecting ([docs](https://ngrok.com/docs/agent/)) | The *stop-reconnecting* signal is a good idea — we have it as the 4010/4011 fatal codes |
| **inlets** | Nothing documented. Pushes keepalive/timeouts onto the ingress ([uplink install](https://docs.inlets.dev/uplink/installation/): nginx `keepalive-timeout: 350`, `proxy-read-timeout: 3600`) | Cautionary example |

### 4.2 We need only one `drain`, not two

RFC 9113's two-GOAWAY dance exists because in HTTP/2 **both** peers open streams, so a server sending GOAWAY cannot know which client-initiated streams are already in flight toward it — hence "send `2^31-1` first, wait one RTT, then the real number".

In ws-mixer **only the server opens streams** (pass 1). The server therefore knows its own `highest_opened` exactly, with no race, and `drain.last_stream_id` is exact on the first send. **One `drain`, no round-trip dance.** This is a real simplification that falls straight out of the unidirectional-open decision, and it is worth stating in the spec so nobody re-imports the h2 complexity later.

The HTTP/2 rule that *does* carry over verbatim:

> "Endpoints MUST NOT increase the value they send in the last stream identifier, since the peers might already have retried unprocessed requests on another connection."

And the receiver rule, from muxado:

> On `drain`, close every stream with `id > last_stream_id` with a distinguishable *retry-safe* error. Everything at or below `last_stream_id` was or might have been processed and is **not** safe to replay blindly.

(For ws-mixer this set is normally empty, since the client never opens streams — but a server that sends `drain` and then, buggily, an `OPEN` above `last_stream_id` must be caught. Make that a `PROTOCOL_ERROR`.)

### 4.3 The rollout sequence

```
t=0     SIGTERM lands (or the pod is marked Terminating)
        ── preStop hook starts FIRST, before SIGTERM: `sleep 5`
        ── endpoints/EndpointSlice removal happens CONCURRENTLY and asynchronously,
           so new upgrades may still arrive during this window — keep accepting them
           and answer each with drain{reason:"rollout", deadline_ms:0} immediately
t=5s    SIGTERM reaches the process
        ── stop accepting new WS upgrades (listener closed / readiness false)
        ── for each live connection: send drain{reason:"rollout",
                                              last_stream_id: highest_opened,
                                              deadline_ms: 25000}
        ── stop sending OPEN on every connection
t=5..30 in-flight streams run to completion. Each connection whose stream table
        empties is closed early with error{GOING_AWAY} + WS close 4012
t=30s   deadline: RESET(CANCEL) every surviving stream, then error{GOING_AWAY}
        + WS close 4012 "draining", reason ≤ 123 bytes
t=45s   terminationGracePeriodSeconds — SIGKILL if anything is left
```

`terminationGracePeriodSeconds: 45`. **Note that preStop time counts against the grace period** ([Pod Lifecycle](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/): "if the preStop hook takes 15 seconds to complete and the grace period is 30 seconds, only 15 seconds remain"), so the budget is `preStop(5) + drain(25) + slack(15)`. The k8s default of 30 s is **not** enough and must be set explicitly.

Two k8s facts that shape this and are commonly missed:

1. **preStop runs before SIGTERM, not in parallel.** "PreStop hooks are not executed asynchronously from the signal to stop the Container; the hook must complete its execution before the TERM signal can be sent" ([lifecycle hooks](https://kubernetes.io/docs/concepts/containers/container-lifecycle-hooks/)).
2. **"The Pod is removed from endpoint slices … concurrently and asynchronously with the shutdown signal being sent to containers."** So the server must **keep accepting** during the preStop window and drain each new connection immediately, rather than closing the listener at SIGTERM. A closed listener during that window produces connection-refused errors at the ingress, which is exactly the user-visible failure the drain exists to prevent.

**Long SSE streams caught mid-drain.** An MCP SSE response may legitimately last hours (pass 2 §7 removed all stream idle timers for this reason), so "wait for in-flight streams" cannot mean "wait". At the deadline they get `RESET(CANCEL)` — `CANCEL`, not `REFUSED_STREAM`, because work *was* performed and the response was partially delivered; the layer above must not blindly replay. The HTTP layer surfaces this to the public client as a truncated SSE stream, which is a condition every SSE client already handles by reconnecting with `Last-Event-ID`. That is the correct place for the retry, and it is why `deadline_ms` is a policy knob rather than a protocol constant.

**Why the client should reconnect *before* the old connection dies.** On receiving `drain` the client SHOULD start a new connection immediately, in parallel, and move new work to it — the old socket keeps serving its in-flight streams until the deadline. This makes a rollout invisible: there is no window in which the agent has zero connections. It requires the server to tolerate two connections from one identity briefly, which is §5.4.

**Other `drain` reasons** reuse the same machinery: `id_exhausted` (pass 1 §4 — 2^30 ids consumed), `overload` (shed a tunnel; pair with `retry_after_ms` so it lands on a different pod), `replaced` (§5.4), `maintenance`.

### 4.4 Client-initiated graceful close

The client shuts down (user hits Ctrl-C, the SDK's `close()` is called):

1. Client sends `drain{reason: "client_requested", last_stream_id: <highest id it has seen>, deadline_ms: <grace>}`.
2. Server stops opening new streams on that connection and immediately deregisters the agent from routing, so public traffic stops arriving.
3. Client finishes in-flight streams, or resets them with `CANCEL` at its own deadline.
4. Client sends `error{code: 0x00 NO_ERROR, message: "client shutting down"}` and closes with **1000**.
5. Server does not treat this as a failure and does not alert.

An SDK's `close()` should default to a **5 s** grace and expose it. `close(force=true)` skips straight to step 4. A client that just drops the socket is not a protocol violation — the server reaps it on keepalive timeout — but it means up to `ping_timeout` (90 s) of public requests routed into a black hole, so the SDK must make the graceful path the default and wire it to SIGINT/SIGTERM.

---

## 5. Reconnect and backoff

### 5.1 Client policy

| Trigger | Policy |
|---|---|
| `drain` received | start a new connection **immediately and in parallel**, before the old one closes. No backoff, jitter `random(0, 2000) ms` only |
| Close `4012 GOING_AWAY` without a preceding `drain` (server died hard, CF restarted a node) | reconnect immediately with the same 0–2 s jitter |
| Close `4013 KEEPALIVE_TIMEOUT` | one immediate attempt, then normal backoff |
| Close `1006` / TCP reset / DNS failure | normal backoff |
| Close `4001/4003/4004` (protocol/flow/frame errors) | normal backoff **and** a loud developer-facing error — these mean an SDK bug, and a silent retry loop hides it |
| Close `4009 ENHANCE_YOUR_CALM` | start at `cap`; honour `drain.retry_after_ms` if it was sent |
| Close `4010 UNSUPPORTED`, `4011 UNAUTHORIZED`, `4014 REPLACED` | **fatal — never reconnect.** Surface and exit |
| HTTP 401/403/404 on the upgrade | fatal |
| HTTP 5xx / 502 / 429 on the upgrade | normal backoff (429: honour `Retry-After`) |
| 101 without an echoed `Sec-WebSocket-Protocol` | fatal (wrong endpoint) |

### 5.2 The backoff numbers

```
delay = random(0, min(cap, base * 2^attempt))       # AWS "full jitter"
base = 1000 ms      cap = 60000 ms      attempt resets to 0 on `welcome`
```

| Parameter | Value | Source / reason |
|---|---|---|
| Algorithm | **full jitter** | [AWS, Exponential Backoff and Jitter](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/): `sleep = random(0, min(cap, base * 2**attempt))`. Beats unjittered exponential substantially; one line to implement, unlike decorrelated jitter which is stateful |
| `base` | 1 000 ms | gRPC's `INITIAL_BACKOFF` ([connection-backoff.md](https://github.com/grpc/grpc/blob/master/doc/connection-backoff.md)) |
| Multiplier | 2 (implicit in `2^attempt`) | gRPC uses 1.6 for gentler growth. With full jitter the difference is small and `2^attempt` is one shift instead of a float multiply — pick the one an implementer cannot get wrong |
| `cap` | **60 000 ms** | gRPC caps at 120 s. Halved deliberately: a down tunnel means the user's MCP server is unreachable from Claude, and a 2-minute worst-case reconnect on a developer tool is user-visible dead time |
| Reset | on **`welcome`**, never on TCP connect or on the 101 | gRPC: "We choose to reset the Backoff when the SETTINGS frame is received, at that time point, we know for sure that this connection was accepted by the server." Resetting on the upgrade gives a hot loop against a server that accepts and immediately closes |
| Connect timeout | 10 s per attempt (ngrok's `connect_timeout`, chisel's `CHISEL_WS_TIMEOUT` is 45 s) | |
| Max attempts | unlimited by default; SDK exposes a cap | chisel `--max-retry-count` defaults to `-1` |

**Optional two-tier refinement**, worth adding once the basics work: frp's `FastBackoffManager` does 3 retries at 200 ms with **heavy** jitter (0.5) inside a 1-minute window before falling into exponential ([client/service.go](https://github.com/fatedier/frp/blob/dev/client/service.go)). It covers the common "server bounced, it is back in 2 seconds" case without waiting a full second. Recommended as a v1.1 refinement, not v1 — the drain path already covers planned restarts, and this only helps unplanned ones.

**What not to copy**: chisel uses factor 2 with **jitter off** ([jpillora/backoff](https://github.com/jpillora/backoff) defaults `Jitter: false`), and rathole uses a flat `retry_interval = 1` second with no backoff at all. Both are thundering-herd generators at fleet scale — and a Cloudflare node restart drops many tunnels at once, which is exactly the herd scenario.

### 5.3 In-flight streams are lost. Say it out loud.

**There is no stream resumption in v1.** On reconnect:

- Every stream on the old connection is gone. Stream ids restart from 1 on the new connection; there is no continuity of ids, buffers, or credit.
- The client's handler for each stream sees an error (`connection closed`), not an EOF. Handlers must be able to tell "the response ended" from "the tunnel died", because those need different behaviour at the layer above.
- `drain.last_stream_id` / `error.last_stream_id` tell the peer which streams were *definitely* never processed. In practice that set is empty for the client (it never opens streams), so the field is there for symmetry and for the server-opens-more-than-it-said bug check.
- **Retry is the application layer's job.** The public HTTP request that was riding a lost stream gets a 502 (or a truncated SSE stream); the MCP client retries as it would against any HTTP server. ws-mixer does not, and must not, replay bytes it has already delivered.

Justifying the omission: resumption requires the sender to buffer everything it has sent until acknowledged, plus a resumable session id, plus a re-sync handshake — that is QUIC's connection migration or an MQTT-style session, and it roughly doubles the size of the spec. The whole design premise is "reconnect is normal and cheap" (Cloudflare restarts nodes with no warning; see §3.1), so the right investment is making reconnect fast and observable, not making it invisible.

### 5.4 Connection replacement: same identity connects twice

This is the failure mode ngrok has a documented error for — [ERR_NGROK_334](https://ngrok.com/docs/errors/err_ngrok_334), "already online", raised when a reconnect races the reaping of the old session. It is guaranteed to happen here, because §4.3 *tells* clients to open the new connection before the old one dies.

Options:

| Policy | Behaviour | Verdict |
|---|---|---|
| Reject the new connection | second connection gets `error{UNAUTHORIZED-ish}` + close | **Wrong.** Turns every drain and every half-open socket into an outage lasting until the old connection's keepalive expires — up to 90 s |
| Reject the old, accept the new (**recommended**) | server sends the old connection `drain{reason:"replaced", deadline_ms: 10000}`, routes all *new* requests to the new connection immediately, lets the old one finish its in-flight streams, then closes it with `4014 REPLACED` | Last-writer-wins. Matches how a user actually behaves (restart the CLI) and makes half-open sockets self-healing |
| Accept both, load-balance | keep a pool per identity | Right answer eventually (frp does this with `group`s, ngrok with endpoint pools) but it needs a routing-layer decision that is out of scope for pass 3 |

**Recommendation: last-writer-wins with a drain of the predecessor**, and the pool left as a documented future direction. The client MUST NOT reconnect on `4014` — if it does, two SDK instances fighting over one identity produce an infinite mutual-eviction loop. That is the entire reason `4014` is on the fatal list; the error message must name the condition plainly: `"this tunnel identity was claimed by a newer connection (session 01J8Z…); is another instance running?"`

Server-side bookkeeping: the routing entry is keyed by identity and holds `(session, connected_at)`. A new connection wins only if its `connected_at` is later; a `drain{replaced}` is sent to the loser; the loser's eventual close must **not** delete a routing entry that now points at a different session (the classic reaper race — compare the session id before deleting).

---

## 6. WS close codes — the complete table

| WS code | ws-mixer code | Name | Who sends | Reason string example | Client action |
|---:|---:|---|---|---|---|
| 1000 | 0x00 | `NO_ERROR` | both | `client shutting down` | normal backoff |
| 1001 | — | WS "going away" | both | — | treat as `4012` |
| 1002 | — | WS protocol error (library-generated) | both | — | fatal if the subprotocol was not echoed, else backoff |
| 1006 | — | abnormal, no close frame seen | — | — | normal backoff |
| 1009 | 0x04 | message too big — the WS library rejected it before ws-mixer saw it | both | — | backoff + loud developer error |
| 1011 | — | internal error. **Python `websockets` emits this on its own `ping_timeout`** — see §3.2 | both | `keepalive ping timeout` | backoff; if you see this, an SDK left `ping_timeout` on |
| 4001 | 0x01 | `PROTOCOL_ERROR` | both | `unknown control message type "regsiter"` | backoff + **loud developer error** |
| 4002 | 0x02 | `INTERNAL_ERROR` | both | `internal error` | backoff |
| 4003 | 0x03 | `FLOW_CONTROL_ERROR` | both | `stream 41: 20480 bytes, 8192 credit` | backoff + loud developer error |
| 4004 | 0x04 | `FRAME_SIZE_ERROR` | both | `frame 98304 B exceeds 65544 B` | backoff + loud developer error |
| 4009 | 0x09 | `ENHANCE_YOUR_CALM` | both | `control message rate exceeded` | long backoff, start at `cap` |
| 4010 | 0x0a | `UNSUPPORTED` | server | `version 2 not supported; this server speaks v1` | **fatal** — surface to the developer |
| 4011 | 0x0b | `UNAUTHORIZED` | server | `token expired at 2026-08-26T09:12:00Z` | **fatal** — surface to the user |
| 4012 | 0x0c | `GOING_AWAY` | server | `draining: rollout` | **reconnect immediately**, 0–2 s jitter |
| 4013 | 0x0d | `KEEPALIVE_TIMEOUT` | both | `no pong for 92s` | one immediate retry, then backoff |
| 4014 | 0x0e | `REPLACED` | server | `identity claimed by session 01J8Z…` | **fatal** — do not reconnect |

Rules:

- The mechanical rule stays **`ws_close = 4000 + error_code`** (pass 2 §5.3). `4014` claims a new ws-mixer code `0x0e REPLACED`, extending pass 2's table by one; everything else is unchanged.
- Codes 4005–4008 are unassigned because pass-2 codes `0x05 STREAM_CLOSED`, `0x06 REFUSED_STREAM`, `0x07 CANCEL`, `0x08 STREAM_LIMIT` are **stream-level only** and never appear on a close frame. Leave the gap — do not renumber to make the table dense.
- **Range check**: [RFC 6455 §7.4.2](https://www.rfc-editor.org/rfc/rfc6455#section-7.4) — 0–999 unusable, 1000–2999 "reserved for definition by this protocol", 3000–3999 "reserved for use by libraries, frameworks, and applications" (IANA-registered), 4000–4999 "reserved for private use and thus can't be registered". Every ws-mixer code is in 4000–4999. No collision with the 1000-series, and deliberately not in 3000–3999 (that range is where WS *libraries* put their own codes, e.g. framework-level auth failures — staying out of it means a 4xxx code always came from ws-mixer).
- **Reason string ≤ 123 bytes of UTF-8.** [§5.5](https://www.rfc-editor.org/rfc/rfc6455#section-5.5): "All control frames MUST have a payload length of 125 bytes or less"; the close code eats 2. Truncation MUST be on a UTF-8 character boundary — a split multi-byte sequence makes some libraries drop the close frame entirely. The full diagnostic lives in the stream-0 `error` message; the close reason is a hint for the case where the `error` was lost in the race.
- **A client MUST handle a close with no `error` message preceding it.** Close frames can be lost, `1006` carries nothing, and an intermediary may close on its own. The close code alone must be enough to pick a reconnect policy — which is why the policy column above is keyed on the close code, not on the `error` payload.

---

## 7. Validation and conformance

The stated fear is "wtf my code doesn't work" bugs from unvalidated JSON. The answer has three parts: a normative schema, a language-neutral fixture corpus, and a validation *strategy* that produces good error messages (a schema alone does not).

### 7.1 `spec/control.schema.json` — draft 2020-12

Layout, following the [MCP schema](https://github.com/modelcontextprotocol/modelcontextprotocol/tree/main/schema)'s shape (which is the closest analogue and is 2020-12 with 145 `$defs`, `const`-pinned discriminators, and flat `anyOf` unions):

```
spec/
  control.schema.json        normative, hand-written, versioned with the subprotocol
  fixtures/
    hello.json
    welcome.json
    ping.json
    pong.json
    drain.json
    error.json
    envelope.json            unknown t, non-object, missing t, wrong types
```

```jsonc
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://mcpwarp.io/schema/ws-mixer/v1/control.schema.json",
  "title": "ws-mixer v1 control messages",
  "$defs": {
    "Hello": {
      "type": "object",
      "required": ["t", "v", "token", "agent"],
      "properties": {
        "t":           {"const": "hello"},
        "v":           {"type": "integer", "const": 1},
        "token":       {"type": "string", "minLength": 1, "maxLength": 4096},
        "agent":       {"$ref": "#/$defs/Agent"},
        "window":      {"type": "integer", "minimum": 16384, "maximum": 2147483647},
        "max_streams": {"type": "integer", "minimum": 1, "maximum": 100000},
        "capabilities":{"type": "array", "items": {"type": "string", "pattern": "^[a-z0-9_.-]{1,64}$"}},
        "meta":        {"type": "object"}
      },
      "additionalProperties": false
    },
    "Ping": {
      "type": "object",
      "required": ["t", "id"],
      "properties": {
        "t":  {"const": "ping"},
        "id": {"type": "integer", "minimum": 0, "maximum": 9007199254740991},
        "ts": {"type": "integer", "minimum": 0}
      },
      "additionalProperties": false
    }
    // … Welcome, Pong, Drain, Error
  },
  "anyOf": [
    {"$ref": "#/$defs/Hello"},   {"$ref": "#/$defs/Welcome"},
    {"$ref": "#/$defs/Ping"},    {"$ref": "#/$defs/Pong"},
    {"$ref": "#/$defs/Drain"},   {"$ref": "#/$defs/Error"}
  ]
}
```

Choices, each with a reason:

| Choice | Why |
|---|---|
| `anyOf`, not `oneOf` | With unique `t` consts the two are equivalent in outcome, `anyOf` short-circuits, and it is what MCP shipped (23 `anyOf` vs 2 `oneOf` in their schema) |
| `additionalProperties: false` on **leaves only** | This contradicts §1.0's "ignore unknown fields" — deliberately. See §7.3 |
| **No `unevaluatedProperties`** | It is the correct keyword for base+extension composition, but it is exactly what disqualifies `fastjsonschema`, `qri-io/jsonschema` and the default `ajv` import. Our messages are flat, so we never need it. Do not add it later without checking every validator |
| **No `discriminator`** | Not a JSON Schema keyword at all — it is an OpenAPI 3.1 annotation, and Ajv's `discriminator: true` is a non-standard extension. Keeping it out keeps the schema portable |
| Integers pinned with `minimum`/`maximum` | `9007199254740991` on `ping.id` because JS `Number` is the binding constraint. Documenting the range in the schema is cheaper than a paragraph nobody reads |
| Regex on capability strings | Catches the "I put a whole sentence in there" case at the wire |

### 7.2 Validation strategy: dispatch on `t`, then validate one schema

**Do not validate against the top-level union.** A bad `ping` reports `must match exactly one schema in anyOf` plus six sub-error groups — which is exactly the unhelpful error the whole exercise is meant to eliminate.

```
parse JSON                       → fail: PROTOCOL_ERROR "control frame is not valid JSON: <parser message>"
is object?                       → fail: PROTOCOL_ERROR "control frame must be a JSON object, got array"
has string "t"?                  → fail: PROTOCOL_ERROR "control frame missing \"t\""
t in the six known types?        → fail: PROTOCOL_ERROR "unknown control message type \"regsiter\""
validate against $defs/<T>       → fail: PROTOCOL_ERROR "hello: /window: 8192 is less than minimum 16384"
semantic checks (below)          → fail: the specific code
```

The union stays in the schema as the formal spec for external tooling and codegen; the runtime never uses it. Each library's mitigation for union errors (Ajv `discriminator`, python-jsonschema `best_match()`, santhosh-tekuri `detailed` output) then becomes unnecessary rather than load-bearing.

**Semantic checks a schema cannot express**, which must be hand-written in every SDK:

| Check | Error |
|---|---|
| `welcome.ping_timeout ≥ 2 × ping_interval` | `PROTOCOL_ERROR` |
| `welcome.max_streams ≤ hello.max_streams` (informational; take the min) | — |
| `pong.id` was actually sent | `PROTOCOL_ERROR` |
| `hello.token` matches the `Authorization` header | `UNAUTHORIZED` |
| `drain.last_stream_id ≤ highest_opened` | `PROTOCOL_ERROR` |
| message arrives in the wrong connection state (`hello` twice, `welcome` first) | `PROTOCOL_ERROR` |

This list is short on purpose. If it grows past a dozen, something belongs in the schema instead.

### 7.3 The one contradiction, resolved

§1.0 says unknown *fields* are ignored for forward compatibility. §7.1 closes every leaf with `additionalProperties: false`. Both are right, in different places:

- **The schema is the conformance oracle**, used in tests and by SDK *authors*. There, `additionalProperties: false` is what catches `max_stream` vs `max_streams` — the single most common wtf-bug — at the point where it is cheap to fix.
- **The runtime is lenient about unknown fields**, because a v1.2 peer must be able to add one without breaking a v1.0 peer.

So: SDKs validate with a **relaxed** variant at runtime (`additionalProperties` unset) and the **strict** variant in their test suite. Generate the relaxed one from the strict one by stripping that keyword — one line of build script, never two hand-maintained files. An SDK MAY offer a `strict: true` debug mode that uses the strict schema at runtime; recommend enabling it in SDK examples and integration tests.

### 7.4 Fixtures

JSON-Schema-Test-Suite layout, one file per message type:

```jsonc
// spec/fixtures/hello.json
[
  {
    "description": "minimal valid hello",
    "tests": [
      {"description": "required fields only",
       "data": {"t":"hello","v":1,"token":"t","agent":{"sdk":"ws-mixer-py","sdk_version":"0.1.0"}},
       "valid": true},
      {"description": "window below minimum",
       "data": {"t":"hello","v":1,"token":"t","agent":{"sdk":"x","sdk_version":"1"},"window":8192},
       "valid": false,
       "errors": [{"instanceLocation": "/window", "keyword": "minimum"}]},
      {"description": "typo in max_streams",
       "data": {"t":"hello","v":1,"token":"t","agent":{"sdk":"x","sdk_version":"1"},"max_stream":64},
       "valid": false, "strict_only": true,
       "errors": [{"instanceLocation": "", "keyword": "additionalProperties"}]},
      {"description": "v2 rejected",
       "data": {"t":"hello","v":2,"token":"t","agent":{"sdk":"x","sdk_version":"1"}},
       "valid": false,
       "errors": [{"instanceLocation": "/v", "keyword": "const"}]}
    ]
  }
]
```

Rules that keep this from rotting:

- **Assert on `valid` always; assert on `instanceLocation` + `keyword` only where it matters. Never assert on error *text*** — the three libraries word everything differently, and a text assertion is a test that fails on a dependency bump.
- `strict_only: true` marks cases that only fail under the strict schema (§7.3).
- Every message type must have ≥1 valid and ≥3 invalid fixtures, and CI fails if a type has zero or if the total count drops. This is the specific discipline [cloudevents/conformance](https://github.com/cloudevents/conformance) lacked — it was archived in 2026 for inactivity because no SDK's CI depended on it.
- Beyond schema fixtures, add a **behavioural** corpus: `spec/fixtures/sequences/` with named connection transcripts (`hello_timeout`, `two_hellos`, `frame_before_welcome`, `drain_with_inflight`, `pong_for_unsent_id`) as arrays of `{from, frame}` plus the expected terminal `{error_code, close_code}`. The schema catches malformed messages; only the sequences catch a wrong state machine, which is where the harder bugs live.

### 7.5 Per-SDK toolchain

| Language | Validator | Notes |
|---|---|---|
| Go | [`santhosh-tekuri/jsonschema/v6`](https://github.com/santhosh-tekuri/jsonschema) | full 2020-12, passes the official suite, emits standard `detailed` output with `instanceLocation`/`keywordLocation` — the fixture assertions map directly. **Test-time only**; hand-write the runtime structs |
| Python | [`jsonschema.Draft202012Validator`](https://python-jsonschema.readthedocs.io/en/stable/validate/) | 2020-12 first-class. `iter_errors()` + `ValidationError.json_path`. **Not `fastjsonschema`** — it stops at draft-07 |
| TypeScript | [`ajv`](https://ajv.js.org/json-schema.html), imported as `import Ajv2020 from "ajv/dist/2020"` | the bare `from "ajv"` import is **draft-07 and fails open** on 2020-12 keywords. Add an ESLint `no-restricted-imports` rule banning it. `@cfworker/json-schema` is the edge-runtime fallback |

**Runtime validation: hand-written, not schema-driven.** The recommendation is that each SDK's *hot path* uses hand-written checks (a switch on `t`, then explicit field checks in the message's constructor) and the *test suite* runs the schema against the fixtures. Reasons:

1. Six message types with ~6 fields each is 150 lines of hand-written validation. A schema library is a dependency in every SDK for something that small — against "implementable by a Python dev in an afternoon".
2. Hand-written checks produce better messages, because they know the protocol (`"welcome.ping_timeout (30000) must be at least 2x ping_interval (30000)"` is not expressible in JSON Schema at all).
3. The schema-vs-fixtures test proves the hand-written checks agree with the normative artifact. That is where the schema earns its keep — as the cross-SDK arbiter, not as a runtime dependency.

Escape hatch: an SDK that *wants* schema-driven runtime validation is free to do it; the relaxed schema is shipped in the package. Just do not make it mandatory.

**Codegen**: TS ([`json-schema-to-typescript`](https://github.com/bcherny/json-schema-to-typescript)) and Python ([`datamodel-code-generator`](https://github.com/koxudaxi/datamodel-code-generator)) are mature enough to generate types from the schema. Go is the weak leg — [`omissis/go-jsonschema`](https://github.com/omissis/go-jsonschema) lists `oneOf`/`anyOf`/`allOf` as not implemented, which kills it for a tagged union. Hand-write the Go structs.

---

## 8. Observability hooks (for the Go API sketch)

Pass-2 ruling: metrics from day one. ws-mixer should expose **events** (a callback/channel interface, so the embedder chooses its metrics library) and ship a thin Prometheus adapter. Nothing in this list requires a dependency inside the core.

### 8.1 Events

| Event | Payload |
|---|---|
| `ConnectionOpened` | session, peer sdk+version, negotiated window/max_streams/ping_interval |
| `ConnectionClosed` | session, ws close code, ws-mixer error code, human message, duration, streams opened, bytes in/out |
| `HandshakeFailed` | stage (`upgrade`\|`auth`\|`hello_timeout`\|`version`), remote addr, reason |
| `StreamOpened` / `StreamClosed` | stream id, duration, bytes in/out, close kind (`normal`\|`reset`) |
| `StreamReset` | stream id, error code, direction (sent/received), message |
| `PingRTT` | session, id, rtt |
| `KeepaliveTimeout` | session, last pong age |
| `DrainStarted` / `DrainCompleted` | session, reason, deadline, streams in flight at start, streams cancelled at deadline |
| `ProtocolViolation` | session, error code, message, frame type, stream id — the "loud developer error" hook |
| `StaleFrameDiscarded` | session, stream id, frame type (the pass-2 debug-log counter) |

### 8.2 Metrics

| Metric | Type | Labels |
|---|---|---|
| `wsmixer_connections_active` | gauge | `role` |
| `wsmixer_connections_total` | counter | `role`, `sdk` |
| `wsmixer_connections_closed_total` | counter | `close_code`, `error_code` |
| `wsmixer_handshake_failures_total` | counter | `stage` |
| `wsmixer_streams_active` | gauge | |
| `wsmixer_streams_total` | counter | |
| `wsmixer_stream_duration_seconds` | histogram | `outcome` |
| `wsmixer_stream_resets_total` | counter | `code`, `direction` |
| `wsmixer_bytes_total` | counter | `direction` |
| `wsmixer_send_window_blocked_seconds` | histogram | — how long writers sat at window 0. **The single most useful number for tuning `initial_window`** |
| `wsmixer_recv_window_bytes` | gauge/histogram | sampled remaining credit |
| `wsmixer_window_updates_total` | counter | |
| `wsmixer_ping_rtt_seconds` | histogram | — doubles as the tunnel's latency SLI |
| `wsmixer_keepalive_timeouts_total` | counter | |
| `wsmixer_drains_total` | counter | `reason` |
| `wsmixer_streams_cancelled_on_drain_total` | counter | — if this is non-zero in normal rollouts, `deadline_ms` is too short |
| `wsmixer_control_messages_total` | counter | `type`, `direction` |
| `wsmixer_protocol_violations_total` | counter | `code`, `sdk` — **alert on this by `sdk`; it finds the broken SDK build** |
| `wsmixer_stale_frames_total` | counter | — pass-2 said "see whether the counter ever moves"; this is that counter |
| `wsmixer_socket_buffered_bytes` | gauge | — the Node/browser `bufferedAmount` watchdog from pass 2 §1.4 |

### 8.3 Tracing / correlation

- `session` (from `welcome`) and `stream_id` are the two correlation keys. Every log line from either SDK must carry both.
- The HTTP layer above should inject them as `X-Mcpwarp-Session` / `X-Mcpwarp-Stream` on proxied requests so an edge log line and a customer-machine log line can be joined.
- No distributed-tracing propagation inside ws-mixer itself: it moves opaque bytes and has no notion of a request. Trace headers ride in the DATA payload, which is where the HTTP layer already puts headers.


---

## 9. What the Python dev must implement

On top of pass 2's §8 list (read loop, write task, per-stream state, credit accounting, 5×5 state table, `highest_opened`).

**Connect**
1. `websockets.connect(url, subprotocols=["ws-mixer.v1"], additional_headers={"Authorization": f"Bearer {token}"}, compression=None, max_size=65544, ping_interval=20, ping_timeout=None)`.
2. Assert `ws.subprotocol == "ws-mixer.v1"`; if not, raise a fatal error and do **not** retry.
3. Send `hello`. Await the first stream-0 message with a **10 s** timeout.
4. Validate it is `welcome`; check `ping_timeout >= 2 * ping_interval` and `ping_interval >= 5000`; store `session`, `window`, `max_streams` (take the min with your own), `ping_interval`, `ping_timeout`. Reset the backoff counter **here** and nowhere else.

**Control dispatcher** — one function, a `match` on `t`:
5. `ping` → enqueue `pong` with the same `id` and `ts`, at the **head** of the control queue.
6. `pong` → if `id` was never sent, connection `PROTOCOL_ERROR`; else record `last_pong_at = now`, `rtt = now - ts`, emit the metric.
7. `drain` → stop expecting new `OPEN`s; start the reconnect task **now**, in parallel; let in-flight streams finish; arm a `deadline_ms` timer that `RESET(CANCEL)`s survivors.
8. `error` → log code + message loudly, mark the connection dead, close. **Never** reply with another `error`.
9. `hello`/`welcome` after the handshake → connection `PROTOCOL_ERROR`.
10. Unknown `t` → connection `PROTOCOL_ERROR` with the offending string in the message (unless the type is covered by a negotiated capability).

**Timers** — three `asyncio.Task`s, that is all:
11. Ping task: first sleep `random(0, ping_interval)`, then every `ping_interval` send `ping{id: next, ts: now_ms}`.
12. Watchdog: every second, `if now - last_pong_at > ping_timeout:` send `error{KEEPALIVE_TIMEOUT}`, close `4013`.
13. Handshake timer: cancelled once `welcome` lands.

**Close and reconnect**
14. Map the WS close code through the §6 table into one of `{fatal, immediate, backoff, long_backoff}`. Fatal set: `4010, 4011, 4014`, plus HTTP 401/403/404 on the upgrade and a missing subprotocol echo.
15. Backoff: `delay = random(0, min(60_000, 1000 * 2**attempt))`; `attempt += 1` per failure; reset only on `welcome`.
16. On the client's own shutdown (SIGINT/SIGTERM): send `drain{reason:"client_requested"}`, wait up to 5 s, send `error{NO_ERROR}`, close `1000`.

**Validation**
17. Parse → object check → `t` check → per-type field validation, in that order, each with its own message. Every message names the field and the value.
18. In tests: run `spec/fixtures/*.json` against `jsonschema.Draft202012Validator` and against your hand-written validator, and assert they agree.

Budget: **~150–200 lines** on top of pass 2's 250–350. The single biggest risk of overrun is the timer bookkeeping; keeping it to three tasks and two timestamps (`last_pong_at`, `highest_ping_id`) is what keeps it small.

---

## Open questions for Anatoly

1. **`register` in `hello.meta`, or a first-class `register` message?** I argue `meta` (§1.6.1). The cost is that ws-mixer's validator does not see the registration, so mcpwarp must ship its own schema for `meta.mcpwarp`. If you would rather have one schema covering everything, that is an argument for making ws-mixer less generic — a defensible choice, since mcpwarp is the only consumer today.
2. **Does mcpwarp ever need to add or remove an exposed service without reconnecting?** If yes, `hello.meta` is not enough and we need a `register`/`unregister` pair behind a capability. If a reconnect is acceptable (the CLI restarts anyway), this never comes up.
3. **`ping_timeout` 90 s (3 missed) or 60 s (2 missed)?** 90 s means up to a minute and a half of public requests black-holed into a dead tunnel. 60 s halves that but will produce false positives on congested home uplinks. My lean is 90 s **plus** the server treating "no traffic at all" separately from "no pong". Do you have data on the worst-case laptop-uplink stall?
4. **Backoff cap 60 s or gRPC's 120 s?** 60 s is a product decision (dead time on a developer tool) rather than a protocol one.
5. **Connection replacement: last-writer-wins (recommended), or a connection pool per identity?** The pool is where this ends up if a user runs the CLI on a laptop and a desktop simultaneously and expects both to serve. Is that a supported scenario for v1?
6. **Empirical Cloudflare test.** Pass 1's open question 7 is still open and now more urgent: the 100 s figure is *not* documented and one community report says 20 s through Cloudflare Tunnel. A 30-minute test through a proxied hostname (idle a WS with no traffic; idle it with only RFC 6455 pings; idle it with only stream-0 pings) would turn three assumptions into three facts. **This is the highest-value unblocked experiment in the whole design.**
7. **Should `drain` be allowed to carry a replacement endpoint** (`{"reconnect_to": "https://edge-2.mcpwarp.io/v1/tunnel"}`)? It makes pod-aware rebalancing trivial and costs one optional field. It also creates an open-redirect-shaped trust question. Deferring is safe.
8. **Symmetric keepalive, or rathole's asymmetric model** (server pings, client only times out)? Symmetric is ~10 more lines per SDK and gives both sides a verdict; asymmetric is simpler to specify. I lean symmetric but it is genuinely a toss-up.

---

## Sources

**Cloudflare**
[WebSockets](https://developers.cloudflare.com/network/websockets/) (the idle-timeout paragraph, no number) ·
[Connection limits](https://developers.cloudflare.com/fundamentals/reference/connection-limits/) (Proxy Read Timeout 125 s, client keep-alive 400 s) ·
[Error 524](https://developers.cloudflare.com/support/troubleshooting/http-status-codes/cloudflare-5xx-errors/error-524/) ·
[Tunnel origin parameters](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/configure-tunnels/origin-parameters/) ·
[cloudflared quic/constants.go](https://github.com/cloudflare/cloudflared/blob/master/quic/constants.go) · [ingress/origin_service.go](https://github.com/cloudflare/cloudflared/blob/master/ingress/origin_service.go) ·
empirical 100 s reports: [Teleport #11762](https://github.com/gravitational/teleport/discussions/11762) · [Buildbot #4078](https://github.com/buildbot/buildbot/issues/4078) · [MQTTnet #1691](https://github.com/dotnet/MQTTnet/issues/1691)

**RFCs**
[RFC 9113 §6.7 PING](https://www.rfc-editor.org/rfc/rfc9113.html#section-6.7) · [§6.8 GOAWAY](https://www.rfc-editor.org/rfc/rfc9113.html#section-6.8) ·
[RFC 6455 §5.5 control frames](https://www.rfc-editor.org/rfc/rfc6455#section-5.5) · [§7.4 close codes](https://www.rfc-editor.org/rfc/rfc6455#section-7.4) · [§5.5.3 Pong](https://datatracker.ietf.org/doc/html/rfc6455#section-5.5.3)

**Muxers and tunnels**
[yamux spec.md](https://github.com/hashicorp/yamux/blob/master/spec.md) · [mux.go defaults](https://github.com/hashicorp/yamux/blob/master/mux.go) (KeepAliveInterval 30 s, ConnectionWriteTimeout 10 s) · [session.go GoAway](https://github.com/hashicorp/yamux/blob/master/session.go) ·
[muxado heartbeat.go](https://github.com/inconshreveable/muxado/blob/master/heartbeat.go) (10 s interval / 15 s tolerance, heartbeat as a dedicated stream) · [session.go GoAway lastId](https://github.com/inconshreveable/muxado/blob/master/session.go) ·
[ngrok agent docs](https://ngrok.com/docs/agent/) · [agent config v3](https://ngrok.com/docs/agent/config/v3/) (heartbeat 10 s / tolerance 15 s) · [ERR_NGROK_334](https://ngrok.com/docs/errors/err_ngrok_334) ·
[chisel README](https://github.com/jpillora/chisel/blob/master/README.md) (`--keepalive` 25 s, retry 1 s→5 m) · [jpillora/backoff](https://github.com/jpillora/backoff) (jitter off by default) ·
[frp client config](https://github.com/fatedier/frp/blob/dev/pkg/config/v1/client.go) (heartbeat 30 s/90 s, disabled when tcpMux is on) · [client/service.go FastBackoff](https://github.com/fatedier/frp/blob/dev/client/service.go) ·
[rathole README](https://github.com/rapiz1/rathole/blob/main/README.md) (server heartbeat 30 s, client timeout 40 s, flat 1 s retry) ·
[inlets uplink install](https://docs.inlets.dev/uplink/installation/) (no protocol-level numbers; pushes timeouts onto nginx)

**Backoff**
[AWS: Exponential Backoff and Jitter](https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/) ·
[gRPC connection-backoff.md](https://github.com/grpc/grpc/blob/master/doc/connection-backoff.md) (1 s / 1.6 / ±0.2 / 120 s; reset on SETTINGS)

**Kubernetes**
[Pod Lifecycle — termination](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/) · [Container lifecycle hooks](https://kubernetes.io/docs/concepts/containers/container-lifecycle-hooks/)

**WebSocket libraries**
[python websockets keepalive](https://websockets.readthedocs.io/en/stable/topics/keepalive.html) · [asyncio/connection.py](https://github.com/python-websockets/websockets/blob/main/src/websockets/asyncio/connection.py) (ping_interval 20, ping_timeout 20, close 1011 on timeout, `connection.latency`) ·
[coder/websocket conn.go Ping](https://github.com/coder/websocket/blob/master/conn.go) · [read.go CloseRead](https://github.com/coder/websocket/blob/master/read.go) ·
[gorilla/websocket conn.go](https://github.com/gorilla/websocket/blob/main/conn.go) · [examples/chat/client.go](https://github.com/gorilla/websocket/blob/main/examples/chat/client.go) (pongWait 60 s, pingPeriod 54 s) ·
[ws docs](https://github.com/websockets/ws/blob/master/doc/ws.md) · [ws: detecting broken connections](https://github.com/websockets/ws#how-to-detect-and-close-broken-connections) ·
[WHATWG WebSocket interface](https://html.spec.whatwg.org/multipage/web-sockets.html#the-websocket-interface) · [whatwg/html#4353 — ping API declined](https://github.com/whatwg/html/issues/4353)

**Schema and conformance**
[JSON Schema 2020-12: unevaluatedProperties](https://json-schema.org/understanding-json-schema/reference/object#unevaluatedproperties) ·
[JSON-Schema-Test-Suite](https://github.com/json-schema-org/JSON-Schema-Test-Suite) ·
[MCP schema](https://github.com/modelcontextprotocol/modelcontextprotocol/tree/main/schema) (2020-12, `anyOf` + `const` tags, no fixtures) ·
[cloudevents/conformance](https://github.com/cloudevents/conformance) (archived 2026-02 — the cautionary tale) ·
[santhosh-tekuri/jsonschema v6](https://github.com/santhosh-tekuri/jsonschema) · [python-jsonschema](https://python-jsonschema.readthedocs.io/en/stable/validate/) · [Ajv 2020-12](https://ajv.js.org/json-schema.html) · [@cfworker/json-schema](https://github.com/cfworker/cfworker/tree/main/packages/json-schema) ·
[json-schema-to-typescript](https://github.com/bcherny/json-schema-to-typescript) · [datamodel-code-generator](https://github.com/koxudaxi/datamodel-code-generator) · [omissis/go-jsonschema](https://github.com/omissis/go-jsonschema) (no `anyOf`/`oneOf`)
