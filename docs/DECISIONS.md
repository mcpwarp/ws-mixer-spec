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
`D-2026-09-20-04` through `D-2026-09-20-07` are owner decisions taken after the v0.3.1 cross-SDK review:
`-04` resolves a `CLIENT-SDK.md`/`WIRE.md` contradiction inherited from the monorepo text; `-05` came from
the mcpwarp CLI, which found that a server closing right after `welcome` was redialled about once a second;
`-06` settles a Go/JS divergence (Go reported a post-101 transport death as `dial`, JS as `handshake` with an
invented close code); `-07` closes a gap where a reconnecting Go client had no application-close API.
`D-2026-09-20-08` was also raised by the mcpwarp CLI: its refresh-token provider failing while the machine
was briefly offline left the client permanently dead, because every provider failure was unconditionally
fatal. `D-2026-09-20-09` was found during the v0.3.1 cross-SDK review: the JS and Go SDKs diverged on
`errorCode`/`errorName` for a bare close, and JS additionally mislabelled an HTTP `401`/`403` upgrade
rejection as `UNAUTHORIZED`. `D-2026-09-20-10` was found by the same review after a Go test flaked: the Go
client's delivery loop could drop an `app` message received immediately before the peer's `error`/close, and
both SDKs could end a stream cleanly — or, in Go, hang a `Read` — when the tunnel died without a close frame.

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
  *Rationale amended by `D-2026-09-20-05`: the "attempt counter has just been reset, so normal backoff would
  never climb" clause above assumed the reset-on-`welcome` rule that entry supersedes (`attempt` no longer
  resets at `welcome`, only once a connection has been stable). The classification itself — non-fatal,
  start at `cap` — is unchanged; it is kept as an explicit "refused on purpose" signal rather than a
  mechanism to make backoff climb.*
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
- **D-2026-09-20-04** — Rejected token, before `welcome`, gets one treatment regardless of which of the two
  forms it took (HTTP `401` on the upgrade, or `error{UNAUTHORIZED}`/close `4011` after the 101): when
  `token` is a provider, the SDK calls it once more and redials immediately (no backoff); a second rejection
  is fatal. When `token` is a static string, the **first** rejection is fatal — no retry, no backoff loop,
  because the same string cannot start working and retrying forever only hammers the auth service and hides
  the real problem. A `4011` received after `welcome` is unaffected and stays fatal. This resolves a standing
  contradiction between `CLIENT-SDK.md`'s "Fatal set" row (which scoped the refresh-retry to "the one
  refresh-retry above" without saying what happens with a static token) and `WIRE.md` §2.9's fatal row (which
  called `4010`/`4011` "fatal — never reconnect", unqualified) — text that dated from before the token-provider
  requirement (`D-2026-08-27-01`) was added and was never reconciled with it. See
  [`WIRE.md`](./WIRE.md#29-sequences), [`CLIENT-SDK.md`](./CLIENT-SDK.md).
- **D-2026-09-20-05** — The backoff `attempt` counter resets only once the connection has been **stable** —
  stayed up for `stable = 10000 ms` past `welcome` — not on `welcome` itself. A server that sends `welcome`
  and immediately closes was resetting the counter every cycle under the old rule, so the client redialed
  roughly once a second forever; the same hot-loop failure the old rule was already written to avoid one step
  earlier (resetting on the 101). This supersedes the "attempt resets ONLY on `welcome`" rule stated in
  `WIRE.md` §2.9 and re-frames `D-2026-09-20-01`'s rationale for `4009`/`4014` starting at `cap`: that entry's
  "the attempt counter has just been reset, so normal backoff would never climb" is no longer true (`attempt`
  no longer resets at `welcome`) — start-at-cap for those two codes is kept, but now as an explicit
  "refused on purpose" signal rather than a mechanism to make backoff climb. The same re-arm-at-stability
  rule applies to `4013`'s one-immediate-attempt budget, for the same reason: a budget re-armable by a mere
  `welcome` is re-armable by a server that welcomes and immediately closes. Drain-driven reconnects are
  unaffected — they are not governed by `attempt`. Prior art checked before picking the number:
  - Docker's restart policy: "A restart policy only takes effect after a container starts successfully. In
    this case, starting successfully means that the container is up for at least 10 seconds and Docker has
    started monitoring it." (<https://docs.docker.com/engine/containers/start-containers-automatically/>) —
    a stability window before the policy re-arms, at 10 s, which is where `stable`'s value comes from.
  - gRPC's connection-backoff doc takes a different approach — it resets immediately on a confirmed accept,
    not after a stability window: "We choose to reset the Backoff when the SETTINGS frame is received, at
    that time point, we know for sure that this connection was accepted by the server."
    (<https://github.com/grpc/grpc/blob/master/doc/connection-backoff.md>) — closer to ws-mixer's old,
    now-superseded reset-on-`welcome` rule. Docker's number is used here rather than gRPC's immediate reset
    because gRPC's own reset point (a confirmed accept) is exactly what produces the hot loop this decision
    fixes for ws-mixer, where `welcome` can be sent by a server that is about to fail. See
    [`WIRE.md`](./WIRE.md#29-sequences), [`CLIENT-SDK.md`](./CLIENT-SDK.md).
- **D-2026-09-20-06** — `phase` defined once, normatively, in `CLIENT-SDK.md`'s disconnect-reason shape row:
  `"dial"` up to and including the HTTP upgrade response, `"handshake"` from the 101 until `welcome`,
  `"connected"` from `welcome` onward. The `Handshake-phase close` row's two named cases (a close frame
  between the 101 and `welcome`; the welcome timeout) gain a third: any other failure in that window —
  transport death with no close frame, an unreadable frame — is `phase: "handshake"` too, with `wsCode`
  whatever the WebSocket library reports for an abnormal closure (typically `1006`) or absent if the library
  reports none; an SDK MUST NOT fabricate a ws-mixer close code for a close that never happened. A connect
  the application itself cancels in this window is not a handshake failure. See
  [`CLIENT-SDK.md`](./CLIENT-SDK.md).
- **D-2026-09-20-07** — On an SDK with an automatic reconnect loop, the application-close API (`D-2026-09-20-01`'s
  `APPLICATION_CLOSE`) MUST also stop that loop: the client closes and stays closed, reporting exactly one
  non-fatal disconnect. It cannot be implemented by closing the underlying connection out from under the
  reconnect loop, which would just get redialed. `docs/CONFORMANCE.md`'s `close` command row states the same
  rule for a client adapter driving a reconnecting client. See [`CLIENT-SDK.md`](./CLIENT-SDK.md),
  [`CONFORMANCE.md`](./CONFORMANCE.md).
- **D-2026-09-20-08** — A token provider that fails MAY mark the failure **temporary**, through an explicit,
  SDK-defined signal (Go: an exported sentinel `ErrTokenUnavailable`, matched with `errors.Is`; JS: an
  exported `TokenUnavailableError` class), and the SDK then treats it like any other failed dial — `phase:
  "dial"`, `fatal: false`, normal backoff, no immediate retry — rather than the unconditional-fatal treatment
  every provider failure got before. The signal is opt-in only: an SDK MUST NOT infer "temporary" from the
  shape of an arbitrary error (no duck-typed `retryable` property or method), since an accidental match would
  turn a genuinely fatal provider failure into an endless retry loop. A marked failure on the Rejected
  token row's one refresh-retry still spends that budget — it does not get a second attempt. Raised by the
  mcpwarp CLI: its refresh-token provider couldn't reach the auth server while the laptop was still waking
  from sleep, and because every provider failure was fatal, the client went permanently dead instead of
  reconnecting once the network came back — the same class of transient failure a DNS error on dial already
  gets retried for. An unmarked provider failure is unaffected and stays fatal exactly as before. See
  [`CLIENT-SDK.md`](./CLIENT-SDK.md), [`WIRE.md`](./WIRE.md#29-sequences).
- **D-2026-09-20-09** — The disconnect reason's `errorCode`/`errorName` are now normatively defined from
  four sources, and never synthesised otherwise: (1) a peer's `error` message that preceded the close —
  `errorCode` is that message's code, unchanged; (2) a **bare** close — a close frame carrying a ws-mixer
  code in `4001`–`4999` with no preceding `error` — where the SDK derives them mechanically,
  `errorCode = wsCode - 4000` and `errorName` from WIRE.md §2.8's table (unknown → `INTERNAL_ERROR`), since
  `ws_close = 4000 + code` is mechanical and cannot be wrong, and a name is more useful to the application
  than nothing; (3) a ws-mixer error the SDK itself raises locally with no close frame from the peer at all —
  the welcome timeout (`PROTOCOL_ERROR`), any other local protocol-violation failure of the connection, and a
  missing/mismatched subprotocol echo on the upgrade, which the SDK classifies `UNSUPPORTED` even though
  there is neither a close code nor an HTTP status to derive it from; (4) the SDK's own application-close call
  (`D-2026-09-20-01`'s `APPLICATION_CLOSE`) — `errorCode` is the caller's own code, `errorName` is
  `APPLICATION_CLOSE`. `errorCode`/`errorName` are still never
  synthesised for anything that isn't a ws-mixer code: an HTTP upgrade rejection is `httpStatus` alone, and an
  abnormal closure (`1006`/no close frame) or any other non-ws-mixer close code carries neither. Found during
  the v0.3.1 cross-SDK review: the JS and Go SDKs had diverged — JS derived `errorCode`/`errorName` for a bare
  close, Go left both empty — and JS additionally labelled an HTTP `401` (and even `403`) upgrade rejection
  `UNAUTHORIZED`, which this decision also rules out: an upgrade rejection is never a ws-mixer error code, so
  it is never entitled to one. See [`CLIENT-SDK.md`](./CLIENT-SDK.md), [`WIRE.md`](./WIRE.md#28-error-codes).
- **D-2026-09-20-10** — Two related delivery guarantees, both previously left to implementation: (1) an
  event already received when the connection ends is still owed to its handler — `error` is the last message
  on the wire (WIRE.md §2.7), so a stream `OPEN`, `app` or `drain` queued ahead of it arrived before the
  connection ended and MUST still be delivered; the delivery loop drains what it already holds rather than
  discarding the backlog, and an SDK MAY therefore invoke a handler shortly after its own close/teardown call
  has returned. (2) When the connection ends for any reason other than a stream's own clean `CLOSE` —
  including an abnormal closure with no close frame at all — every stream still open MUST end with an error
  visible to its reader/writer, never as a clean end-of-stream, and a read/write pending or started
  afterward MUST fail promptly rather than hang; data already buffered and a `CLOSE`/`RESET` already received
  for that stream are still delivered first (WIRE.md §2.5's "CLOSE preserves buffered data; RESET discards
  it" is unaffected by how the connection itself ends). Found by the cross-SDK review after a Go test
  flaked: the Go client's delivery loop dropped an `app` message that had been received immediately before
  the peer's `error`/close instead of draining it first, and both SDKs could end a stream cleanly — or, in
  Go, hang a pending `Read` forever — when the tunnel died without a close frame, because neither guarantee
  was written down anywhere for either SDK to implement against. See [`CLIENT-SDK.md`](./CLIENT-SDK.md),
  [`WIRE.md`](./WIRE.md#29-sequences).
