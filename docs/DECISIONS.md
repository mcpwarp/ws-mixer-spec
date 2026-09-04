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
