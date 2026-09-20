# Decision log

Status: **canonical**. This file holds every `ws-mixer.v1` **protocol** decision. Every other repo's
`docs/DECISIONS.md` records only that repo's own implementation decisions and links back here for anything
protocol-level — see [`REPOS.md`](./REPOS.md).

Each entry has a stable id (`D-YYYY-MM-DD-NN`) so other repos can cite it and so a moved or split entry
stays findable. Ids are never renumbered or reused, even when an entry moves out (see `D-2026-08-26-05`
below).

Rationale for recording decisions here once, rather than duplicating this log into every repo: a duplicated
log has no single writer and silently diverges the moment one repo amends a decision. A pointer cannot.

## 2026-08-26

Project owner's approvals.

- **D-2026-08-26-01** — 8-byte frame header, no length field: payload length is `len(ws_message) - 8`.
- **D-2026-08-26-02** — JSON control messages on stream 0, seven message types in v1.
- **D-2026-08-26-03** — 256 KiB per-stream window (default); violations are connection-fatal, not
  stream-scoped.
- **D-2026-08-26-04** — Only the server opens streams; no stream resumption after reconnect.
- **D-2026-08-26-05** — *(split, moved out)*. Originally one bullet — "Go server on `coder/websocket`; JS
  client on `ws`, streams exposed as Node `Duplex`" — but these are two independent **implementation**
  decisions, not protocol decisions, so this id is retired here and split into:
  - `D-2026-08-26-05a` in `ws-mixer-go/docs/DECISIONS.md` (WebSocket library choice for the Go
    implementation).
  - `D-2026-08-26-05b` in `ws-mixer-js/docs/DECISIONS.md` (WebSocket library choice and `Duplex` stream
    exposure for the JS implementation).
- **D-2026-08-26-06** — `register` is mcpwarp's concept, not ws-mixer's — ws-mixer exposes a generic,
  opaque `app` message instead.
- **D-2026-08-26-07** — Strict handling of unknown control message types (`t`), reconciled with forward
  compatibility via `capabilities` negotiation.
- **D-2026-08-26-08** — Keepalive: in-band `ping`/`pong` on stream 0, `ping_interval` 30 s, `ping_timeout`
  90 s.
- **D-2026-08-26-09** — Connection replacement and connection pooling are application policy, built on a
  server-side connection-accept hook plus `Conn.Drain(reason)` — not something ws-mixer decides.

## 2026-08-27

Project owner's approvals.

- **D-2026-08-27-01** — Client-SDK requirements generalized: token provider with one refresh-retry on 401;
  disconnect reason carries `httpStatus`/`phase`. Requested by the mcpwarp CLI; applies to all client SDKs.
  See [`CLIENT-SDK.md`](./CLIENT-SDK.md).
- **D-2026-08-27-02** — `UNSUPPORTED` (0x0a / 4010) may be sent by the client, not just the server: when
  `welcome.v` is not a supported version (or `welcome` is otherwise unsupported), the client closes with
  `UNSUPPORTED` 4010.
- **D-2026-08-27-03** — OPEN above `last_stream_id` after `drain` is a connection error, strict: a client
  receiving `OPEN` with `id` > `last_stream_id` after `drain` MUST treat it as connection `PROTOCOL_ERROR`
  (4001), not a stream-scoped `REFUSED_STREAM`.
- **D-2026-08-27-04** — Control-channel flood limits made normative: an implementation MUST accept at least
  50 messages/second sustained and a burst of 100 on stream 0; it MAY close with `ENHANCE_YOUR_CALM` (4009)
  when a peer exceeds burst 100 (token bucket, 50/s refill). Payload cap 16 KiB unchanged.

## 2026-09-20

Project owner's approvals. `D-2026-09-20-01` and `D-2026-09-20-02` are driven by the mcpwarp tunnel needing
them before anything goes public; `D-2026-09-20-03` was found while implementing the Go and JS SDKs.

- **D-2026-09-20-01** — `0x0e` named `APPLICATION_CLOSE` (WS close 4014), connection-level only. Replaces
  the `0x0e` `*(reserved)* … left free for an application-level close reason` note that lived only in
  `WIRE.md` §2.8's table — never itself a logged decision. The mcpwarp tunnel is now actually using it
  (closing over-cap connections with 0x0e/4014, reason `CONNECTION_LIMIT: ...`), and an unnamed code renders
  as `INTERNAL_ERROR` in every SDK, which is misleading for a code an application sent on purpose. It may be
  sent by either side's application via the SDK's close API; `ws-mixer` itself never emits it. Application
  error codes `>= 0x1000_0000` remain unusable for a connection close (`ws_close = 4000 + code` is only a
  legal close code for `code ≤ 0x3e7`, and those codes additionally don't fit the 16-bit close-code field)
  and stay RESET-only. `D-2026-08-26-09` (connection replacement and pooling are application policy, built
  on an accept hook plus `Conn.Drain`) is unaffected — `APPLICATION_CLOSE` is a new close reason, not a new
  mechanism. Reconnect classification (WIRE.md §2.9): non-fatal, but start at `cap` rather than normal
  backoff — an application close is a deliberate server-side decision made after `welcome`, where the
  attempt counter has just been reset, so normal backoff would never climb; a `4014` received before
  `welcome` uses normal backoff instead, same as any other pre-`welcome` failure. See
  [`WIRE.md`](./WIRE.md#28-error-codes).
- **D-2026-09-20-02** — Client-SDK disconnect reason gains `closeReason` — the reason of the close frame
  received from the peer, present whenever one was received (empty when this side initiated the close, or
  when the SDK closed on `error` without reading the peer's close). Also normative: both a close received
  between the 101 and `welcome` and the client's own 10 s welcome timeout are reported with
  `phase: "handshake"` — the former with the peer's `wsCode`/`closeReason`, the latter with a locally
  generated `wsCode: 4001` — never as a dial failure (a bug found in the Go SDK). See
  [`CLIENT-SDK.md`](./CLIENT-SDK.md).
- **D-2026-09-20-03** — Closing with an error code whose `4000 + code` isn't a legal WS close code (any
  `code > 0x3e7`, reachable off the wire whenever a peer's `error.code >= 0x1000_0000` and the receiver
  closes on it) MUST send WS close `4002` (`INTERNAL_ERROR`'s close code) instead of attempting the illegal
  code — libraries otherwise send no close frame at all, or throw, leaving the peer a bare `1006`.
  `error.code` and the SDK-reported `wsCode` stay semantic (the real, unclamped code) so both peers still
  agree on what happened; only the wire close code is clamped. Found while implementing the Go and JS SDKs.
  See [`WIRE.md`](./WIRE.md#28-error-codes), [`CLIENT-SDK.md`](./CLIENT-SDK.md).
