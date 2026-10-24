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
  (`D-2026-09-20-01`'s `APPLICATION_CLOSE`) — `errorCode` is always `14`, `errorName` is always
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

## 2026-09-25

Project owner's approval. `D-2026-09-25-01` came out of the v0.4.0 conformance review; approved by Anatoly.

- **D-2026-09-25-01** — The application-close API takes a message only, never a caller-supplied error code:
  it always closes with `APPLICATION_CLOSE` (`0x0e`/4014). [WIRE.md §2.8](./WIRE.md#28-error-codes) makes
  `APPLICATION_CLOSE` the only code an application may close a *connection* with; a caller-supplied code would
  let a healthy connection close with a protocol-fault code, and produce an `errorCode`/`errorName` pair that
  isn't in §2.8's table at all. This applies to both the client and server SDK close APIs — §2.8 lets either
  side's application use `APPLICATION_CLOSE` — and the harness `close` command's `0`/`14` restriction covers
  both roles. Changed: `CLIENT-SDK.md`'s Application close row, source (4) of the Disconnect reason shape row,
  and the `wsCode` row; `WIRE.md` §2.8's clamp sentence; `CONFORMANCE.md`'s `close` command row and §6 step 5;
  `D-2026-09-20-09`'s source (4). The harness `close` command keeps its `code` field for compatibility but
  accepts only `0`/absent or `14`. See [`CLIENT-SDK.md`](./CLIENT-SDK.md), [`WIRE.md`](./WIRE.md#28-error-codes),
  [`CONFORMANCE.md`](./CONFORMANCE.md).

## 2026-10-13

Project owner's approvals. `D-2026-10-13-01` is driven by browser-class client SDK runtimes, which cannot set
upgrade headers at all; `D-2026-10-13-02` was found while bringing ws-mixer-server up against Zotero.

- **D-2026-10-13-01** — The `Authorization` header on the upgrade becomes optional; `hello.token` stays
  required. A client sends `Authorization: Bearer <token>` whenever its runtime can set it; a server MUST accept
  an upgrade with no `Authorization` header and authenticate on `hello.token` alone. When the header is present
  it must be a well-formed `Bearer <token>` (else HTTP `401`, as before) and `hello.token` must equal it (else
  `error{UNAUTHORIZED}` + close `4011`, as before). A `hello` without a valid `token` field stays a malformed
  `hello` (`PROTOCOL_ERROR`, §2.7's field table). No close code, HTTP status or other wire rule changes. Reason: the browser `WebSocket` API, Electron renderers and Firefox-based
  hosts (e.g. a Zotero plugin) cannot set request headers on an upgrade, and §2.1's Forbidden row already rules
  out the usual workarounds (token in the query string or in `Sec-WebSocket-Protocol`), so a mandatory header
  shut those runtimes out of `ws-mixer.v1` entirely. Security is unchanged: every server already had to verify
  `hello.token`, so the header was only ever a second copy of the same credential. The cost is on the server:
  it loses its cheap pre-upgrade reject for header-less clients, holds an un-authenticated socket until `hello`
  (bounded by the 10 s hello timer, §2.9), and so SHOULD bound how many such sockets it holds itself. Changed:
  `WIRE.md` §2.1's Auth row and request example, §2.7's `hello.token` field row, §2.9's handshake diagram;
  `CLIENT-SDK.md` gains a Token on the upgrade row; the control schemas' `token` description. See
  [`WIRE.md`](./WIRE.md#21-websocket-handshake), [`CLIENT-SDK.md`](./CLIENT-SDK.md).
- **D-2026-10-13-02** — A server MUST NOT reject an upgrade for its `Origin` value, including `null`, unless
  its operator configured an `Origin` allowlist (deployment policy, outside this spec; a rejection there → HTTP
  `403`, fatal per §2.9). Reason: cross-site WebSocket hijacking relies on an ambient browser credential
  (a cookie) that the browser attaches to an attacker page's upgrade; `ws-mixer.v1` authenticates on
  `hello.token` (and the `Authorization` header when present), which a hostile page cannot obtain, so an
  origin check adds no protection against hijacking (the drive-by pre-`hello` sockets it would also have cut off
  are bounded by §2.1's Auth-row SHOULD instead) and only answers real browser-class clients with HTTP `403`. Found in the
  Zotero live test: Zotero's hidden chrome window sends `Origin: null` on the `wss` upgrade, and
  ws-mixer-server's `coder/websocket` `Accept` defaulted to same-origin and rejected it; ws-mixer-server v0.5.1
  fixes it. Node, Go and Rust SDK clients send no `Origin`, so SDK tests never exercised the check. The
  conformance harness has no HTTP-upgrade-rejection path at all (`CONFORMANCE.md` §1.3), so `Origin: null`
  acceptance is not conformance-tested either, which is why no SDK test hit it. Changed: `WIRE.md` §2.1 gains
  an Origin row, and the browser-class note under its request example points at it. See
  [`WIRE.md`](./WIRE.md#21-websocket-handshake), [`CONFORMANCE.md`](./CONFORMANCE.md).

## 2026-10-14

Project owner's approvals. `D-2026-10-14-01` is driven by the Go, Rust and Python SDKs that need a mcpwarp layer
to implement against; `D-2026-10-14-02` through `-05` resolve inconsistencies found while specifying that layer
from ws-mixer-js HEAD (`d45d3b4`, `SDK_VERSION` `0.10.1`) and its docs; `D-2026-10-14-06` collects the points
where the ws-mixer-js session implementing spec `v0.6.0` found `MCPWARP.md` silent; `D-2026-10-14-07` does the
same for the ws-mixer-rust session implementing `v0.6.1`; approved by Anatoly.

- **D-2026-10-14-01** — The mcpwarp application layer is specified in this repo, as
  [`MCPWARP.md`](./MCPWARP.md) with `spec/mcpwarp.schema.json` and `spec/fixtures/mcpwarp/`, in two tiers.
  Tier 1, the wire layer (envelope, identity, registration and take-over, request bridging, runtime
  constraints), binds every agent and edge. Tier 2, the SDK profile (`expose()`, the tunnel controller, its
  status codes and state machine, `describeStatus` and its locale tables), binds any SDK that ships the
  tunnel-controller API. Both are written to ws-mixer-js 0.11.0's intended behaviour; every place HEAD differs
  is listed in `MCPWARP.md` §11 for 0.11.0 to implement. The ws-mixer core is unchanged: it still has no
  mcpwarp types, `app.body` stays opaque to it, and `D-2026-08-26-06` stands — the layer is documented next to
  the core, not merged into it. Reason: the only definition of the layer was ws-mixer-js source and its
  README, so a second SDK would have had to reverse-engineer behaviour, bugs included, and a cross-language
  contract belongs where the pin currency already is (`spec.pin`, [`REPOS.md`](./REPOS.md)). The fixtures are
  data each SDK runs in its own CI; the Go conformance runner does not read them. Ships as spec `v0.6.0`.
  Changed: new `MCPWARP.md`; `OVERVIEW.md`'s intro and "Where to go next" (mcpwarp material no longer "lives
  outside the spec"); `REPOS.md`'s repo table and "Where things live"; `WIRE.md` §2.7's `app` paragraph (points
  at `MCPWARP.md` instead of "mcpwarp's own schema"); `CONFORMANCE.md` §1.4. See [`MCPWARP.md`](./MCPWARP.md),
  [`REPOS.md`](./REPOS.md).
- **D-2026-10-14-02** — MCP Streamable HTTP semantics belong to the application's handler, not the SDK. The
  SDK MUST forward method, path, headers (including `mcp-session-id`) and body unchanged, MUST NOT inject,
  strip or generate `Mcp-Session-Id`, and MUST NOT special-case `GET`, `DELETE` or JSON-RPC notifications. The
  only responses it generates on `/mcp` for a well-formed request are `413`,
  `400` `{"error":"request cannot be represented as a web Request"}`, `500` `{"error":"handler error"}`,
  `500` `{"error":"internal error"}`, `503` while disabled or closing, and the target adapter's `502`/`504`
  (`MCPWARP.md` §4.5). Reason: an earlier task list put session-id generation, `405` + `Allow: POST` for a
  `GET` without an SSE `Accept`, and `202` for notifications on the SDK because they are visible on `/mcp`; an
  SDK that did them would fight the MCP server library that already does, and would tie the transport to one
  MCP revision. ws-mixer-js already behaves this way. Changed: `MCPWARP.md` §4.7. See [`MCPWARP.md`](./MCPWARP.md#47-mcp-semantics-belong-to-the-handler).
- **D-2026-10-14-03** — Precedence on `/mcp` is `404` (path) > `503` (disabled/closing) > `413` (body cap) >
  handler, in every adapter, and the `413` reason phrase is `Content Too Large` (RFC 9110). Reason: never read
  a body that will be refused anyway — a disabled service should not buffer up to 4 MiB to then answer `503`.
  Found while specifying: ws-mixer-js's `handler`/`target` adapters read the body before the availability
  check (`413` beat `503`), its Node `server` adapter did the opposite, and that adapter alone said
  `Payload Too Large`. Changed: `MCPWARP.md` §4.4, §4.5, §11 rows 5–6. See
  [`MCPWARP.md`](./MCPWARP.md#44-precedence).
- **D-2026-10-14-04** — `INVALID_NAME` is its own `TunnelStatusCode`, split from `INVALID_CONFIG`: a service
  name that fails `isValidServiceName`, or an `INVALID_NAME` register refusal, is `error`/`INVALID_NAME`; every
  other option error stays `INVALID_CONFIG`. The code set is therefore 13. And a `BAD_REQUEST`,
  `NOT_IMPLEMENTED` or unknown register refusal is terminal for the tunnel controller before **and** after the
  first registration: `error`/`INTERNAL`, the same mapping as a first-register refusal. A local invalid name
  or `kind` still passes through `connecting` before `error`, as in ws-mixer-js HEAD. The `onRetry` payload
  keeps ws-mixer-js's field name `reason` (an earlier draft said `cause`): renaming it would break every
  existing host for no behavioural gain, and the fixtures' `retry.cause` is a fixture-side classifier, not
  the callback field. A ws-mixer `drain` with `reason:"replaced"` is a take-over, handled exactly like a
  `conflict` op: `TAKEN_OVER` (rejection before settle, `conflict` event after), sticky until the next
  `configure()`, and no `register` on any later `welcome`, the drain replacement's included. Reason: a host can
  point at the name field only if the code says it is the name;
  ws-mixer-js HEAD left a post-success `BAD_REQUEST`/`NOT_IMPLEMENTED`/unknown refusal at
  `connecting`/`INTERNAL` with nothing ever retrying it — a status that claims progress where none can happen;
  an unknown code means the edge speaks something newer than the SDK, which waiting will not fix; and an edge
  that replaces free-tier connections by draining the old one (WIRE.md §2.9) never sends that connection
  `conflict`, so an agent treating the drain as a hand-over re-registers on the replacement and takes the name
  back — two installs of the same name would take it from each other forever. ws-mixer-js HEAD never reads the
  drain reason. Changed: `MCPWARP.md` §2.5, §3.3, §5.4, §5.6, §5.9, §6.5–§6.8 (T7, T19, T23, T31, T40, T41, T43,
  new T45), §10.4, the six status-text locale files (`rules.json` `newInSpec`), §11 rows 1–2 and 15 and its
  not-deviations line. See [`MCPWARP.md`](./MCPWARP.md#67-transitions).
  Note (`D-2026-10-15-05`): the rationale above does not hold for the deployed edge. It never sends
  `drain{replaced}` and has no free-tier drain policy; its take-over is the per-name `conflict{taken_over}`
  op, and a draining connection hands its names over silently, so the "two installs … forever" scenario does
  not occur. The `drain{replaced}` rule stands as future-proofing.
- **D-2026-10-14-05** — The status-text tables are normative data, shipped as JSON in
  `spec/fixtures/mcpwarp/status-text/` (`rules.json`, `cases.json`, one file per locale: `en`, `de`, `es`,
  `fr`, `ja`, `zh-CN`). An SDK ships them string-identical (it derives its own tables from the JSON; the
  strings, not the files, must match); changing, adding or removing a string is a spec release, not an
  SDK-local edit. Reason: the same status must read the same in every SDK and host, and the tables were
  otherwise a TypeScript literal that each port would retype by hand. Changed: `MCPWARP.md` §7, §10.5. See
  [`MCPWARP.md`](./MCPWARP.md#7-status-text-tier-2).
- **D-2026-10-14-06** — v0.6.1 clarifications from the JS implementation; each ruling below is now normative
  text. Ships as spec `v0.6.1`. Reason: the ws-mixer-js session implementing spec `v0.6.0` found the spec
  silent on these points, so each SDK would otherwise have picked its own answer. Profile tier:
  - P1 `onRetry` for an `OVERLOADED` re-send: `attempt` `1` for the first re-send after the refusal, +1 per
    consecutive re-send, reset by a successful `registered` (§5.9).
  - P2 drain replacement dial: `onRetry` `{attempt: 0, delayMs: <jitter>, reason: "drain"}`; a hand-over is
    not a failure, the backoff counter is not consulted (§5.9). `delayMs` is the actual WIRE.md §2.9 jitter
    waited before the dial (`0–2000 ms`), not `0`; the tunnel runner checks it ≤ the `retry` event's
    `delayMs`.
  - P3 `drain{replaced}` take-over with no known server id: `serverId` omitted, never `""` (§5.4, §5.6; §11
    row 16).
  - P4 a failure after the upgrade but before `welcome` (handshake failure, welcome timeout) is a failed dial:
    T27–T29, never T30 (§6.7 Pre-`welcome` failure; fixture `tunnel/34_t27_pre_welcome_failure_retrying`).
  - P5 `<CODE>: <message>` log lines: `CODE` is the `McpwarpError` code when the failure has one, else the
    `TunnelStatusCode` (§6.9).
  - P6 `registered`/`disable`/`enable`/`conflict` on a superseded connection after the replacement's `welcome`
    are stale: the handle ignores them, so the tunnel never sees them (§2.7, §5.9, §6.8 Drain, T38; fixture
    `tunnel/35_t38_stale_app_messages_ignored`).
  - P7 the one-shot token-refresh redial is a scheduled reconnect: the `onRetry` payload is exactly
    `{attempt: 1, delayMs: 0, reason: "token-refresh"}` (§5.9; §11 row 17).
  - P8 a take-over closes the taken-over connection immediately, on a `conflict` (T34) and on a
    `drain{replaced}` (T45) alike: no `unregister`, in-flight streams cut, sticky until the next `configure()`
    (§3.3 Sticky, §5.9 Drain, §6.8 Take-over close). ws-mixer-js already does this for `conflict`
    (`conflicted()` → `detach()` → `handle.close()`).
    Refined by `D-2026-10-14-07` R4 / §11 row 18: that close still sends `drain{client_requested}` and waits a
    grace while streams are open, which the take-over path must skip.
  - P9 `tunnel/README.md` gains "Running the fixtures": observing `onRetry`, driving `disconnect.fatal`, the
    exact snapshot comparison rule.

  Wire tier:
  - W1 §2.8 rule 8 (`registered.services[].url` non-empty) holds for every entry, other names included:
    validation is syntactic over the whole message; a violation is `MALFORMED_ENVELOPE` for all of it (§2.3,
    §2.8).
  - W2 its message is `"registered.services[].url must be a non-empty string"`; fixtures' `message_contains`
    is a substring match, and they use a prefix (§2.8 rule 9).
  - W3 an adapter MAY stream the request body; the cap still applies (overshoot ≤ one transport chunk,
    ≤ 64 KiB). Three-way: no response byte → `413`; head written, response unfinished → RESET `CANCEL`;
    response complete → CLOSE with the response intact (a RESET there can drop bytes the edge has not
    forwarded; as W5) (§4.2; fixture `bridging/body_413_streamed_reset`, new bridging keys `inRest`,
    `outReset`). `*_reset` fixtures apply only to streaming adapters; a buffering-only runner skips them with
    a note (§10.3).
  - W4 after any SDK-generated refusal (`404`, `503`, `413`, `500`) with an unread body the agent drains it
    (window updates) and CLOSEs, never RESETs; the edge's own body cap bounds the drain (§4.4 Ending a refused
    stream).
  - W5 a body that turns out malformed after a refusal was written still ends with CLOSE, not RESET; after a
    streaming app's own response head it follows the W3 table (§4.4).
  - W6 how a runner reaches each bridging `sdkState` (§10.3, `spec/README.md`); fixtures
    `bridging/disabled_chunked_body_503`, `bridging/head_mcp_503`.

  Changed: `MCPWARP.md` §2.3, §2.7, §2.8, §3.3, §4.2, §4.4, §5.4, §5.6, §5.9, §6.7, §6.8, §6.9, §10.3, §10.4,
  §11 rows 16–17; `spec/README.md`; `tunnel/README.md`; `check-fixtures.mjs` (`inRest`, `outReset`, head-only
  `HEAD` replies); `COUNTS.json`. See [`MCPWARP.md`](./MCPWARP.md).
- **D-2026-10-14-07** — v0.6.2 clarifications from the Rust implementation; each ruling below is now normative
  text. Ships as spec `v0.6.2`. Reason: the ws-mixer-rust session implementing spec `v0.6.1` (crate
  `ws-mixer-mcpwarp`) found these points silent, JS-only or self-contradictory, so each SDK would otherwise have
  picked its own answer. Profile tier:
  - R3 a `target` with `http:` MUST be supported, `https:` is OPTIONAL; an SDK without https support MUST
    reject the option synchronously as a configuration error (§6.1): thrown by `expose()` / `createTunnel()`
    option validation, never a tunnel status (not T8), never at request time (§5.2 row 3, §6.1).
  - R5 an observable HTTP `401` on the upgrade (`httpStatus` 401, no `errorName`) is `error`/`UNAUTHORIZED`
    like close `4011`, and `403`/`404` `error`/`DISCONNECTED_FATAL`; an unobservable one is a failed dial
    (`connecting`/`RETRYING`) (§1 Upgrade-status mapping, §6.7 T27–T29, T32, T33; `dial_failed.httpStatus`;
    fixtures `tunnel/36_t32_http_401_upgrade_unauthorized`,
    `tunnel/37_t27_http_401_upgrade_unobservable_retrying`).
  - R6 the drain replacement dial's `onRetry` `delayMs` is the actual delay before the dial, backoff jitter or
    the drain's `retry_after_ms` hint alike (§5.9).
  - R11 `onRetry` does not fire for a replacement dial that `drain{replaced}` abandons (no retry is
    scheduled) (§5.9; R7).

  Wire tier:
  - R1 socket injection is runtime-specific: a runtime with a WebSocket API MUST accept an injected constructor
    or factory; a native SDK MUST expose an equivalent connector hook, at minimum the TLS configuration (§8).
  - R2 `hello.meta.client` is `"sdk-<lang>"` (`sdk-js`, `sdk-go`, `sdk-rust`, `sdk-python`); `version` is the
    mcpwarp layer's own package version; `hello.agent` stays the transport's (§3.1).
  - R4 a take-over close (`conflict` or `drain{replaced}`) is `error{NO_ERROR}` on stream 0 + WS close `1000` at
    once, skipping WIRE.md §2.10's drain and grace; never `APPLICATION_CLOSE` (§3.3 Sticky, §6.8 Take-over
    close; §11 row 18).
  - R7 on `drain{replaced}` the layer MUST abandon any replacement dial that has not yet completed its upgrade; a
    replacement that has already sent `hello` (sent in the same poll as the `101`, so it cannot be held back)
    MUST be closed with `error{NO_ERROR}` + WS `1000` before any `welcome` handling, MUST NOT be reported as
    connected and MUST NOT register; the edge MUST tolerate a `hello` followed by an immediate close. `onRetry`
    does not fire for the abandoned dial in either case (§3.3 Drain `replaced`, §6.8).
  - R8 envelope defect check order is normative — in a `registered.services[]` entry `name`, `id`, `url`,
    `created`, the checker's order — so a multi-defect message reports the same error everywhere (§2.8 rule 10;
    fixtures `envelope/invalid/registered_multiple_defects`, `registered_defects_id_before_url_created`,
    `registered_defects_url_before_created`; §11 row 19).
  - R9 key order: senders SHOULD use the canonical order, receivers MUST NOT depend on it, an encoder MUST
    produce §2.2's JSON value; byte identity is checked only against fixtures (§2.1, §2.2, §10.2, §12).

  Repositories: R10 `REPOS.md` lists `ws-mixer-rust` (with the sibling crate `ws-mixer-mcpwarp`): six
  repositories.

  Changed: `MCPWARP.md` §1, §2.1, §2.2, §2.8, §3.1, §3.3, §5.2, §5.9, §6.1, §6.7, §6.8, §8, §10.2, §10.4, §11 rows
  18–19, §12; `REPOS.md`; `tunnel/README.md` (`dial_failed.httpStatus`, drain `delayMs`); `check-fixtures.mjs`
  (`httpStatus` range; a `MALFORMED_ENVELOPE` fixture's `message_contains` must name the first defect);
  `COUNTS.json`. See [`MCPWARP.md`](./MCPWARP.md).
  Note (`D-2026-10-15-05`): R4, R7 and R11 handle a `drain{replaced}` that the deployed edge never sends (its
  take-over is the per-name `conflict{taken_over}` op); they stand as future-proofing. ws-mixer-go records its
  side as `D-2026-10-14-01`…`-04` in its own `docs/DECISIONS.md`.

## 2026-10-15

Project owner's approvals. Anatoly approved accepting a token provider on the tunnel's `TunnelConfig.token`
(`D-2026-10-15-03`) and an edge request header `X-Mcpwarp-Server-Id` for routing (defined in spec `v0.7.0`,
not here), and confirmed multi-service connections as option (a) — a multi-service wire tier plus a
multi-service controller profile — forthcoming as spec `v0.7.0`; nothing of it is in this release.
`D-2026-10-15-01` collects the wire rulings asked for by the ws-mixer-python session; `-02` the request-bridging
rulings from the Rust, Python and JS reviews of §4; `-03` the profile additions from the Python, Rust and JS
sessions; `-04` the cross-language naming from all four SDK sessions; `-05` the deployed-edge facts reported by
the mcpwarp-saas session (`apps/tunnel` at `f81cd76`). Ships as spec `v0.6.3`.

- **D-2026-10-15-01** — Wire rulings from the Python SDK; each is now normative text. Reason: the ws-mixer-python
  session found these points silent or untestable, and JS and Go had already diverged on them. Rulings:
  a client's `drain{client_requested}` carries `last_stream_id` = its `highest_opened` and `deadline_ms` = the
  grace it actually waits (`5000`); an `OPEN` after its own drain is `RESET(REFUSED_STREAM)`, never a
  connection error (it can cross the drain; its id is above the `last_stream_id` sent), and the server SHOULD
  stop opening; a received drain's old connection is retired at `deadline_ms + 1000` ms (`30000` absent), timed
  from the replacement's `welcome`, or once it has no streams, survivors ended with `CANCEL`, then
  `error{NO_ERROR}` + `1000` — ws-mixer-js's own `D-2026-10-12-04`, adopted here; `drain.retry_after_ms` binds:
  wait at least that long before the replacement dial, up to 2000 ms jitter on top, `random(0, 2000)` when
  absent; the `STREAM_LIMIT` → `ENHANCE_YOUR_CALM` escalation is a MAY (suggested 20 refusals in 10 s, counting
  only OPENs above the declared cap); integer fields accept any integral JSON number (`1.0` is `1`), never a
  boolean, and senders write plain integers; the control string limits count code points (JSON Schema
  `maxLength`), senders SHOULD also stay within the byte count; a WINDOW is checked stale before its increment,
  so an increment-0 WINDOW for a retired id is discarded and counted; an SDK's stream-reset API SHOULD reject
  connection-only codes locally. From ws-mixer-js 0.11.1: a locally closed connection ignores every inbound
  frame, and a dial scheduled before the close never reaches the socket factory. And the agent's own graceful
  close carries no `errorCode`/`errorName` (`wsCode` `1000` only); Python's temporary-failure marker is
  `ws_mixer.TokenUnavailableError`, matched over the `__cause__` chain (10 links, cycle-safe). Changed:
  `WIRE.md` §2.3, §2.5, §2.7 (drain fields, integer and string-limit rules), §2.8, §2.9 (Client side of a drain,
  Reconnect table), §2.10 items 12 and 14; `CLIENT-SDK.md` Disconnect reason shape, Provider failure;
  `MCPWARP.md` §5.9, §11 rows 20–23, 30, 32–35, 40, 45; control fixtures (`hello` with `"v": 1.0` / `"v": true`,
  non-BMP `error.message` at and over the limit). See [`WIRE.md`](./WIRE.md#29-sequences),
  [`CLIENT-SDK.md`](./CLIENT-SDK.md).
- **D-2026-10-15-02** — Request-bridging rulings (§4). The forwarded target is percent-encoded from the
  received bytes exactly once: the allowed set (RFC 3986 unreserved, sub-delims, `:` `@` `/` `?`, and `[` `]`)
  passes unescaped, `%` is never touched (existing escapes are never decoded, a lone `%` stays), everything
  else is escaped with uppercase hex, the fragment is cut at the first `#` in origin-form and absolute-form
  alike, an SDK MAY escape `'` as `%27` in the query wherever it hands the app a normalised URL object, and
  target mode joins the target's own query and the request's verbatim with `&`. A handler or target status
  `< 200` or `>= 600` → `500 handler error`; the head cap counts the terminating blank line;
  `400 request cannot be represented as a web Request` applies in handler and target modes; `\mcp` is `400
  unsupported request target form`, not `404`; after a `HEAD` request line every SDK-generated reply is
  head-only, parse errors included; §4.4's fallback `503` applies only before the handle's first successful
  registration; the content-encoding re-serve applies only when the client actually decoded. Informative: a
  local target's `3xx` passes through verbatim. Reason: the Rust, Python and JS reviews of §4 found these
  under-specified, and the SDKs produced different bytes (JS re-encoded UTF-8 queries, Go rewrote a lone `%`
  and escaped `#`, JS answered `500` where Rust answered `400`). Changed: `MCPWARP.md` §4.2–§4.6 (§4.5 gains
  the minimum unrepresentable set: `CONNECT`, `TRACE`, `TRACK`, a NUL byte in a header value), §10.3, §11
  rows 24–27, 36, 38, 39 (38–39 marked fixed in ws-mixer-go v0.9.0), 43, new 47–48; bridging fixtures
  `target_utf8_query`, `target_lone_percent`, `target_fragment_dropped`, `target_fragment_dropped_absolute_form`,
  `target_allowed_set`, `target_unrepresentable_400`, `handler_connect_400`, `head_mcp_400_headonly`,
  `mcp_handler_status_600_500`. See [`MCPWARP.md`](./MCPWARP.md#43-path-rule).
- **D-2026-10-15-03** — Profile additions. `TunnelConfig.token` is a string or a provider, with the transport's
  provider semantics: called per dial; one refresh retry on `401`/`4011` (`onRetry` `{attempt: 1, delayMs: 0,
  reason: "token-refresh"}`); a marked temporary failure is a failed dial (`connecting`/`RETRYING`); an unmarked
  one `error`/`DISCONNECTED_FATAL`; `4011` after the refresh `error`/`UNAUTHORIZED`; compared by identity for
  idempotence. §6.2 check 4 becomes "neither a non-empty string nor a provider", logged
  ``INVALID_CONFIG: `token` must be a string or a callable``. Identity means the same function object, for a
  bound method the same receiver and function. `status` is the last snapshot delivered to listeners and MAY lag
  a call in progress. The connector hook sits on `expose()`'s and the tunnel constructor's options, MAY also
  sit on `configure()`. The never-echo rule covers `token`, `name` and `url`. §6.9 drops the dashboard warn
  line; only the `CODE: ` prefix is fixed. `close()`/`dispose()` in `conflict` closes immediately and resolves
  once the take-over close has completed. On acceptance (§2.7): every op from a superseded connection is ignored,
  `error` included; `registered`, `disable`, `enable` delivered after their connection ended are ignored,
  `conflict` and `error` still apply; a `disable` with an unknown id is held until a `registered` brings it,
  never adopted. The drain replacement's `onRetry` `delayMs` includes `retry_after_ms`. Reason: the Python tunnel
  review, the Rust actor-based controller and JS 0.11.1 each hit a point the profile left open; the token
  provider brings the tunnel up to what `expose()` and the transport already accept (JS, Rust and Go adopt it:
  §11 rows 31, 41, 44). Changed: `MCPWARP.md` §2.7, §5.2, §5.7, §5.9, §6.2, §6.4, §6.7 (T6, T38, new T46–T51),
  §6.8, §6.9, §8, §10.4 (events `token_provider {result: ok|unavailable|error}`, `drain {reason?, conn?,
  retry_after_ms?}`), §11 rows 28–29, 31, 41–42, 44, new 46, §12; tunnel fixtures
  `38_token_provider_refresh_retry`, `39_token_provider_unavailable_retrying`, `40_token_provider_error_fatal`,
  `41_drain_retry_after_honoured`, `42_disable_unknown_id_held`, `44_stale_disable_after_disconnect_ignored`,
  `45_dispose_in_conflict_immediate`. See [`MCPWARP.md`](./MCPWARP.md#67-transitions).
- **D-2026-10-15-04** — Cross-language naming. `CLIENT-SDK.md` gains an informative transport naming table keyed
  by the JS names, with the Go (v0.8.0 surface, v0.9.0 names for `HTTPClient`, `OnReconnect`, `Superseded`,
  `CloseImmediately`), Rust (`ws_mixer` 0.2.0, incl. `no_redial_on_drain` and `close_immediate`) and Python
  (proposal) columns, `n/a` where an SDK has no such item. `MCPWARP.md` §9 gains the Python names, the handler
  row (`async Request -> Response`; `asgi_handler(app)` adapts ASGI 3) and an informative ASGI mapping (§9.1).
  `hello.meta.client` is one of `sdk-js`, `sdk-go`, `sdk-rust`, `sdk-python` (never `sdk-py`), `meta.version` at
  most 100 bytes (the edge truncates); the integration id is one constant UUID per integration product, never
  generated per install or device, and a per-install identity would get its own field. Reason: four SDKs now
  implement the transport and the layer, and each session reported its names; the Python proposal said
  `sdk-py`, which the spec never allowed. Changed: `CLIENT-SDK.md` (new table); `MCPWARP.md` §2.4, §3.1, §9,
  §9.1, §12. See [`CLIENT-SDK.md`](./CLIENT-SDK.md#transport-naming-across-sdks),
  [`MCPWARP.md`](./MCPWARP.md#9-naming-tier-2).
- **D-2026-10-15-05** — Deployed-edge facts, recorded as corrections. The edge never sends `drain{replaced}`
  and has no free-tier drain policy: its only drain is ws-mixer's own (`rollout`, on shutdown or a per-pod
  control call), and a draining connection hands its names over silently. Take-over is per name: the losing
  connection gets `{v, op: "conflict", id, name, reason: "taken_over"}` (`taken_over` is the only reason) and
  keeps its other names. `disable` is `{v, op, id, reason}` with no name (key by id), `enable` is
  `{v, op, id, name}` (key by name or id); both fan out to every connection of the owner. `CONNECTION_LIMIT` is
  an `error` op followed by `APPLICATION_CLOSE`, WS close `4014` with a reason starting `CONNECTION_LIMIT:`
  (`4009` was the old code), at 10 connections per owner **per pod**. `TOO_MANY_SERVICES` is a whole-frame
  `error` op (cap 100) with no `registered` reply. The `drain{replaced}` rules (`D-2026-10-14-04`, `-07` R4, R7,
  R11) stand as future-proofing; their rationale is corrected by a note on each, not rewritten. Reason: the
  mcpwarp-saas session read the tunnel's code (`apps/tunnel/internal/agentconn/agentconn.go`, mcpwarp-saas
  `f81cd76`) and found the spec describing an edge policy that was never implemented. Cross-reference:
  ws-mixer-go `D-2026-10-14-01`…`-04` (its `docs/DECISIONS.md`), the Go side of the same rulings (ws-mixer-go
  v0.9.0). Changed: `WIRE.md` §2.8 (`CONNECTION_LIMIT` example), §2.9 (the two free-tier passages marked
  historical); `MCPWARP.md` §2.6, §2.7 (edge op shapes), §3.3, §11 row 37 (Go's take-over close); notes on `D-2026-10-14-04` and `-07`; tunnel
  fixture `43_too_many_services_error_op`. See [`MCPWARP.md`](./MCPWARP.md#33-conflict-and-take-over),
  [`WIRE.md`](./WIRE.md#29-sequences).

## 2026-10-18

Project owner's approvals. Anatoly chose option (a) on 2026-10-15 — a multi-service wire tier plus a
multi-service controller profile, with the single-service text kept as a conforming subset — and approved the
two edge request headers `X-Mcpwarp-Server-Id` and `X-Mcpwarp-Server-Name` the same day. The wire facts come
from the mcpwarp-saas session's reading of the deployed edge (`apps/tunnel`, mcpwarp-saas `f81cd76`, on
ws-mixer-go v0.8.0); the controller behaviour is modelled on the Go CLI (mcpwarp/cli v0.2.4, `ab462de`, on
ws-mixer-go v0.7.0: `internal/tunnel/tunnel.go`, `internal/registry/registry.go`, `internal/cli/up.go`,
`docs/DESIGN.md`), which records no D-entries of its own on these points. Where the two disagree, the edge wins
on the wire and the CLI's tolerant merge by name is the model for the controller. Ships as spec `v0.7.0`.

- **D-2026-10-18-01** — Multi-service wire tier. A `register`/`unregister` carries 1..100 distinct entries
  (`maxItems: 100`, was `1`); a `register` frame with more → whole-frame `error{TOO_MANY_SERVICES}`, no
  `registered`; `unregister` frames SHOULD also stay ≤ 100 (no error defined). Each `registered`
  answers exactly one `register` and lists exactly that frame's entries, each in `services[]` or `errors[]`
  (`errors[].id` only on `SERVER_DISABLED`); partial success is normal; a reply listing none of the frame's
  names counts as all-transient and is re-sent with the shared backoff (no handle state change for
  single-service agents); a later
  `register` is additive. `unregistered` is `{v, op, services: [{name, id}]}`, no `errors`, names not held
  silently absent, other names untouched. Every `welcome` re-registers the full configured list minus local
  removals, local disables and taken-over names (dashboard-disabled and refused names included);
  same-session re-register is idempotent; a reconnect's new session takes over the old connection's holds.
  Take-over is per name: `conflict{id, name, reason:"taken_over"}` costs the loser that name only, its other
  names stay live, and it never re-registers the name until an explicit `enable(name)` or `configure()`;
  a `conflict` for a name never held is ignored. `disable` (by id) and `enable` (by name or id) fan out to
  every connection of the owner; an unknown `disable` id is held per connection, cleared by a matching
  `enable`, a take-over of the name or the connection's end, and never carried across a reconnect. Transient entries are retried
  per name with the §3.2 backoff. Reason: the edge already implements batches (`maxServicesPerRegister = 100`,
  `agentconn.go:1131`) and the CLI already sends them; the v0.6.x one-entry rule described neither. Changed:
  `MCPWARP.md` §2.1 (One service per message scoped to single-service agents, new Batch row), §2.2 (batch
  examples), §2.3 (Multi-service replies), §2.6 (`TOO_MANY_SERVICES` row: above 100, never for 1..100), §2.7
  (multi-service acceptance; Held `disable` lifetime row), §3.1–§3.3 (new rows; the Drain `replaced` row marked
  "not sent by the deployed edge"), §5.9 (Held `disable` row), §10.2, §10.6, §11 rows 51 and 52; envelope fixtures `register_batch_valid`, `register_batch_101_invalid`,
  `registered_partial_errors`, `unregistered_subset`, `conflict_per_name`; `spec/mcpwarp.schema.json`
  `services` `maxItems: 100`. See [`MCPWARP.md`](./MCPWARP.md#21-carrier).
  Note (`D-2026-10-19-01`): "re-sent with the shared backoff" is superseded: a reply listing none of the frame's
  names is re-sent with §3.2's transient backoff, and `OVERLOADED` keeps its own counter.
- **D-2026-10-18-02** — Stream routing on a multi-service connection. No target rides `OPEN`; the agent selects
  the service from the request head: by `X-Mcpwarp-Server-Id` (header name case-insensitive) matched against
  `registered.services[].id` when present, with no fallback when it matches nothing; else by the `Host`
  header's first label split at its **last** `-` (usernames are `^[a-z0-9]{3,20}$` with no `-`; service names
  §2.4, so the split is unambiguous) and matched against the configured names. `X-Mcpwarp-Server-Name` MAY
  also be present and is never routed by. Both headers are optional and edge-added; an edge that sets them
  first removes the client's copies. An agent honours them only with the SDK option `trustServerIdHeader`
  (default `false`) on, which operators enable once their edge is known to add and strip them; otherwise
  routing is by `Host`. Selection runs after the path check, so
  `404` still beats `503`; no service selected, or a locally disabled, unregistered, refused or never-active
  service → `503 service unavailable`; dashboard-disabled → `503 service disabled`; taken over → RESET
  `CANCEL`. Reason: the edge picks the connection by the `Host` label (`internal/reqpath/pipe_server.go:29`) and
  forwards the raw request with `Host` kept, so the label is the only hint today; a server-id header survives
  renames and costs the edge little (mcpwarp-saas, 2026-10-15). A per-stream target on `OPEN` would be a new
  ws-mixer stream-metadata feature and stays out of scope. Changed: `MCPWARP.md` §4.8 (new; the
  `trustServerIdHeader` option, default off, gates step 1 and the headers' trust), §4.5 (an adapter whose
  runtime HTTP parser rejects an unrepresentable request before the path is seen MAY answer the `400` before
  §4.4's checks), §6A.1, §9.2; bridging fixtures `multi_service_route_by_header` (`runnerRequirement`
  `trust-server-id-header`), `multi_service_route_by_host`, `multi_service_unknown_service_503`,
  `multi_service_unknown_service_non_mcp_404`.
  The "available from mcpwarp tunnel `<version>`" placeholder is filled in when mcpwarp-saas tags the release.
  See [`MCPWARP.md`](./MCPWARP.md#48-multi-service-routing).
  Note (`D-2026-10-19-06`): the placeholder is filled: "mcpwarp tunnel 0.6.0".
  Note (`D-2026-10-24-01`): superseded in part: `trustServerIdHeader` is the host controller's option, not an
  SDK option, and §9.2's names are informative.
- **D-2026-10-18-03** — Multi-service controller profile and status vocabulary. `createServiceGroup(options)`
  with `configure({enabled, token, services: [{name, kind?, handler | target, targetTimeoutMs?}], url?,
  webSocket?})`, `stop()`, `dispose()`, `addService()`, `removeService()`, local `enable(name)`/`disable(name)`,
  and a `ServiceGroupStatus {state, code?, lastError?, services}` whose `services` map holds a
  `ServiceStatus {state, id?, url?, code?, lastError?}` per name. Per-service states `pending`, `active`,
  `disabled`, `rejected`, `taken_over`, `unregistered`, by the CLI's precedence (`internal/cli/up.go:387-402`:
  local disable, awaiting reply, `SERVER_DISABLED`, other refusal, dashboard disable, active) with removal and
  take-over added. The connection keeps §6's state machine and its 13 codes; no new `TunnelStatusCode`; the
  group never enters `disabled` and reaches `conflict` only by a whole-connection `drain{replaced}`. Partial
  failure never moves the group. The one new fatal case — nothing registered since the connection started,
  nothing in flight, every service refused — is `error`/`INTERNAL` with the new `lastError` `No service could
  be registered.`; any `SERVER_DISABLED` entry keeps the group alive, since its dashboard `enable` arrives on
  that connection. A services-only `configure()` diffs by name and never redials. New status-text keys
  `service.pending`, `service.active`, `service.disabled`, `service.rejected`, `service.taken_over`,
  `service.unregistered` and `lastError.NO_SERVICE_REGISTERED`, six locales each, new in this spec;
  `describeServiceStatus` renders a `ServiceStatus`, and `describeStatus` shows the localized
  `NO_SERVICE_REGISTERED` detail for that one `lastError`. Reason: the CLI runs several servers on one
  connection and every SDK that grows the same feature needs one observable contract; mapping the fatal case
  onto `INTERNAL` keeps the 17-pair table and every existing locale key valid. The CLI's own rule (fatal unless
  every error is `SERVER_DISABLED`) would close a connection that a disabled service still needs; the profile
  departs from it there (`MCPWARP.md` §11 row 50). Changed: `MCPWARP.md` §1 (tier row), §5.9 (`onRetry`
  special cases: with a provider, the first rejected token — `401` at the upgrade or `4011` before `welcome` —
  is reported as a non-fatal `disconnect`, `phase` `dial`/`handshake`, then `onRetry` `{1, 0, "token-refresh"}`
  and the redial), §6A (new), §7 (data files row), §6.6 (one row), §9.2 (new), §10.6, §11 rows 50 and 53, §12;
  `CLIENT-SDK.md` Rejected token row; status-text locale files and `rules.json` `newInSpec`;
  tunnel fixtures `ms_01_…` onward. See
  [`MCPWARP.md`](./MCPWARP.md#6a-multi-service-hosts-informative).
  Note (`D-2026-10-24-01`): superseded in part: §6A is informative guidance for hosts, no SDK ships
  `createServiceGroup`, and the seven status-text keys are OPTIONAL in an SDK's tables.
- **D-2026-10-18-04** — One frame in flight and the `OVERLOADED` re-send. An agent keeps at most one
  `register`/`unregister` outstanding per connection; the next goes out after the reply, a whole-frame `error`
  op for it, or the connection's end. `OVERLOADED` means the frame was dropped unprocessed (per-pod semaphore
  of 256 in-flight app messages shared by every connection on the pod, `agentconn.go:538`, `:906-914`) and
  gets no reply; every agent, single-service included, MUST re-send the dropped frame (`register` or
  `unregister`) after `min(1000·2^(n−1), 30000)` ms, `n` = consecutive `OVERLOADED` (or all-transient) replies
  since the last successful reply; any successful reply (a `registered` with at least one non-transient entry,
  or an `unregistered`) resets it. There is no "re-send once". An agent MAY instead re-send every unanswered
  frame; one in flight is recommended. Reason: neither `OVERLOADED` nor `TOO_MANY_SERVICES` names the frame it answers, so without one
  in flight an agent cannot know what to re-send or which names were refused; the CLI never re-sends and so
  silently loses a dropped `register` until the next reconnect. Changed: `MCPWARP.md` §2.6 (`OVERLOADED` row,
  Frames in flight), §3.2 Transient retry, §5.9, §6.7 T15, §6A.7, §11 row 49, §12; tunnel fixtures for the `OVERLOADED` re-send and the whole-frame refusal. See
  [`MCPWARP.md`](./MCPWARP.md#26-errorcode).
  Note (`D-2026-10-19-01`): the counter shared by `OVERLOADED` and all-transient replies is superseded: `OVERLOADED`
  has its own counter, reset by any other reply, and transient refusals keep §3.2's register backoff.

## 2026-10-19

Clarifications after the v0.7.0 implementation round: no wire change, no new status code, no new locale key.
The rulings were given to the SDK sessions by message on 2026-10-18 and 2026-10-19 and bind from then; this
section records them as text. Sources: the ws-mixer-python v0.7.0 audit, ws-mixer-js 0.11.2 (shipped
2026-10-18) and its 0.11.3 plan, the `ws-mixer-mcpwarp` 0.1.1 audit (Rust), ws-mixer-go v0.9.1 (`f522835`) and
the v0.10.0 review, and mcpwarp/cli v0.2.7 (`70a867a`). Anatoly approved the edge header change of
`D-2026-10-18-02` in the mcpwarp-saas session, to be built after this release. Ships as spec `v0.7.1`.
`D-2026-10-19-05` and `-06` ship as spec `v0.7.2`.

- **D-2026-10-19-01** — Frames in flight. Two counters: `OVERLOADED` has its own, `n` = consecutive `OVERLOADED`
  replies on the connection, delay `min(1000 × 2^(n−1), 30000)` ms, reset by any non-`OVERLOADED` reply (a
  `registered`, an `unregistered`, a whole-frame `error` op); transient refusals keep §3.2's register backoff
  (1 s doubling, cap 30 s, reset on a successful register, carried across reconnects; a restart through
  `configure()` or a `stop()` starts a new one; after a reply mixing transient and non-transient entries it is
  reset and the transient names wait the n = 1 delay). One in flight holds in
  normal operation: a trigger while a frame is outstanding queues until the reply; after `OVERLOADED` the slot
  stays reserved and triggers during the pause fold into the one re-send (the frame is re-queued, not released).
  Only `register` triggers fold, into the re-send of a dropped `register`, within 100 names: the dropped frame's
  names first in their order, then the folded-in names in trigger order; a name removed or locally disabled
  during the pause is left out, and its `unregister`, if the connection may hold it, queues behind the re-send;
  any other trigger queues behind it. A `register` dropped by `OVERLOADED` was never processed, so a name carried
  only by a dropped frame is not held and gets no `unregister`. A re-send with no names left is not sent and
  frees the slot; with nothing outstanding or queued, a `connecting` group is evaluated as G1's "nothing to
  send" (`online` at once unless G5 holds) and an `online` group is unchanged (G4).
  Any reply that settles the frame cancels every pending re-send timer for it. A whole-frame `error` op with any
  code other than `OVERLOADED` and `CONNECTION_LIMIT`, unknown codes included, answers the outstanding frame;
  `UNSUPPORTED_VERSION` is then fatal (T24); single-service: T25, nothing re-sent; multi-service: G7. One that
  answers an outstanding `unregister` settles it like an `unregistered`: the frame's names leave the
  controller's held set, logged `warn`. A
  `registered` listing none of the frame's names is re-sent with the transient backoff in both profiles.
  `close()` MAY send its `unregister` while a `register` is outstanding; its wait ends on the `unregister`'s own
  reply by frame order (an `unregistered`, or a whole-frame `error` op whose turn it is) or the 2000 ms cap;
  while closing, a `register` dropped by `OVERLOADED` is abandoned, never re-sent, and duplicate `unregister`s
  are harmless. One controller owns a connection's mcpwarp ops; a host multiplexing several on one connection
  serializes their frames and routes each reply by frame order, never by name. Reason: the Python audit found
  the v0.7.0 counter merging `OVERLOADED` with all-transient replies (fixture `26_t16` already expects 1000 ms
  for an `OVERLOADED` right after a transient retry); JS and Python both sent a third `register` from a stale
  timer after success; Rust 0.1.0 had the same gaps (§11 row 60); Rust's questions on the close wait, a
  single-service whole-frame error and several controllers per connection; the Go v0.10.0 review's close wait
  by frame order. The review of the v0.7.1 draft asked for the fold-in's scope and order and for the transient
  counter's restart and mixed-reply delay; Anatoly ruled that a whole-frame `error` op answering an `unregister`
  settles it. A dropped frame left no state on the edge, so an `unregister` for its names would be a wasted
  frame; an emptied re-send gets no reply to drive G2, so without G1's rule a `connecting` group would wait for
  the next `welcome`. Changed: `MCPWARP.md` §2.3 Empty reply, §2.6
  (`TOO_MANY_SERVICES` row, One in flight: fold-in scope and order, emptied re-send, `OVERLOADED` re-send), §2.7
  (`registered`, `enable` and `unregistered` rows, multi-service `unregistered` and `error` rows), §3.2
  (Transient retry: restart and mixed reply, `OVERLOADED`, new One controller per connection row, Unregister),
  §5.7 step 2, §5.9 `onRetry` special cases, §6.7 T13, T25, T26, §6A.2 (may hold), §6A.6 (G7, Emptied
  re-send), §6A.7 (One in flight, Correlation, Backoff counter), §6A.8, §10.6 (`app_error` row), §12; tunnel
  fixtures `ms_12_overloaded_reserved_slot_fold_in`, `ms_13_transient_then_overloaded_separate_counters` and
  `ms_14_overloaded_all_removed_no_resend`; the group
  checker's two streaks and the single-service `OVERLOADED` delay check. See
  [`MCPWARP.md`](./MCPWARP.md#26-errorcode).
  Note (`D-2026-10-24-01`): superseded in part: the group rules here (G1, G4, G7, the held set) are §6A's
  recommendations for hosts; the frames-in-flight rules stay wire tier.
- **D-2026-10-19-02** — Held `disable` and per-name `conflict`. A held `disable` is also cleared by a replacement
  connection's `welcome` (drain hand-over), besides a matching `enable` (by name or id), a take-over of the name
  and the connection's end. When a later `registered` brings the held id, the registration is recorded (url,
  id, `registered` event) and the hold then applies (`disabled` event), back to back; the tunnel shows one
  `disabled` snapshot (T50). A connection "holds" a name once it received a `services[]` entry for it; a
  `conflict` for a name only in an outstanding `register`, or known only through `SERVER_DISABLED.id` or
  `enable`, is ignored. Informative: an agent MAY also reset that service's in-flight streams locally. Reason:
  Python and JS 0.11.2 already clear holds on the replacement's `welcome`; the Go v0.10.0 review needed rulings
  on whether the registration counts and which names a `conflict` can reach. Changed: `MCPWARP.md` §2.7 (Unknown `disable`
  id, Held `disable` lifetime, multi-service `conflict` row), §3.3 Per name, §5.4 (`disable` row: `registration`
  present after a held `disable`), §5.9 Held `disable`. See
  [`MCPWARP.md`](./MCPWARP.md#27-inbound-acceptance).
- **D-2026-10-19-03** — Profile and transport clarifications. A single-service handle never selects by `Host`:
  every `/mcp` request reaching a registered handle goes to its handler; §4.8's selection is the multi-service
  controller's only. Informative: an SDK MAY clamp any computed delay (backoff, `drain.retry_after_ms`,
  `Retry-After`) to its runtime's timer maximum (JS and Python: 2^31−1 ms). Phase is decided by order: a
  `welcome` followed by a close in the same delivery burst is a connected-phase disconnect, and the SDK never
  exposes that connection as current. The §4.8 Trust gate placeholder reads "a future mcpwarp tunnel release
  (0.6.0 or later)"; the header change is approved and built after this release, and the version is filled in
  once the tunnel tags it. Reason: Rust proposed the §4.4 note for single-service handles; Python and JS clamp
  retry hints; JS 0.11.2 reported a same-burst close as `handshake` (§11 row 59). Changed: `MCPWARP.md` §4.4,
  §4.8 Trust gate, §5.9 (Timer clamp row); `CLIENT-SDK.md` Reconnect / backoff, Disconnect reason shape. See
  [`CLIENT-SDK.md`](./CLIENT-SDK.md).
  Note (`D-2026-10-19-06`): the placeholder is filled: "mcpwarp tunnel 0.6.0".
- **D-2026-10-19-04** — Implementation status, naming and deviations. §9 Go: the tunnel's token provider is
  `TunnelConfig.TokenSource` (an interface compared by `==`, like the socket factory) with the
  `TokenSourceFunc` adapter, and `ServiceGroupConfig.TokenSource` likewise; the §9.2 Go names are confirmed as
  written; `ServiceStatus.Code` is a plain string, the group's `Code` a `StatusCode`. §11: JS rows 18, 19, 24,
  25 fixed in 0.11.1 and rows 20–23, 26–31, 47, 52, 53 in 0.11.2, new rows 54–59 for 0.11.3; Go rows 40, 41
  (`TokenSource`) and 46 fixed in ws-mixer-go v0.9.1; Rust rows 42 and 44 fixed in
  `ws-mixer-mcpwarp` 0.1.1, new row 60; Python rows 45 and 48 implemented in ws-mixer-python 0.1.x
  (unreleased, pins v0.7.0); Go CLI row 49 for the release that integrates ws-mixer-go v0.10.0, row 50 partly
  fixed in v0.2.7, new informative row 61 (state vocabulary). Reason: every SDK session reported against v0.7.0
  or its v0.6.3 pin. Changed: `MCPWARP.md` §9, §9.2, §11. See
  [`MCPWARP.md`](./MCPWARP.md#11-deviations-from-sdk-heads).
- **D-2026-10-19-05** — Rulings after the `v0.7.1` tag. A scheduled transient re-send (§3.2 Transient retry)
  counts as queued: a `connecting` group whose re-send empties while any name still has a pending transient
  re-send stays `connecting`/`RETRYING` (G3) until that re-send is answered; G1's "nothing to send → `online`"
  applies only with nothing outstanding, queued or scheduled. A name removed or locally disabled and then
  re-added or re-enabled within the same `OVERLOADED` pause folds into the re-send, and its owed `unregister` is
  cancelled; it takes its place among the folded-in names in trigger order, after the dropped frame's remaining
  names, and does not return to its original slot. The transient counter resets on a `registered` with at least
  one non-transient entry — a success, a `SERVER_DISABLED` entry or any refusal such as `CONFLICT` — or an
  `unregistered`; a whole-frame `error` op leaves it unchanged and resets only the `OVERLOADED` counter. During
  an `OVERLOADED` pause, in both profiles, an `error` op or an empty `registered` answers nothing (the dropped
  frame is not on the wire). Single-service, as §6A.2's "may hold": `close()` skips its `unregister` only when
  every `register` this connection sent was dropped by `OVERLOADED` (none is outstanding) and no `registered`
  ever arrived on it; a `register` answered by a whole-frame `error` op or by an empty reply still gets an
  `unregister`; a `registered` naming the service received during an `OVERLOADED` pause settles the handle and
  cancels the re-send. Reason: the emptied re-send and the remove-then-add rulings came from the Go v0.10.0
  review; the counter examples follow Python's fix of its transient counter (the text already said so and only
  gains the examples); the single-service sentences came from the JS 0.11.3 review and match the multi-service
  "may hold". Changed: `MCPWARP.md` §2.6 (One in flight, `OVERLOADED` re-send), §3.2 (Transient retry,
  Unregister), §5.7 step 2, §6A.6 Emptied re-send, §6A.7 (One in flight, Correlation, Backoff counter). See
  [`MCPWARP.md`](./MCPWARP.md#26-errorcode).
- **D-2026-10-19-06** — Tunnel header release and implementation status. mcpwarp tunnel 0.6.0 (mcpwarp-saas
  `932c765`, built against §4.8 at `v0.7.1`) is the release that adds and strips
  `X-Mcpwarp-Server-Id` and `X-Mcpwarp-Server-Name`: the strip is case-insensitive over every spelling of both
  names and also covers a chunked request's declared trailers; both are set once, at the pod holding the route,
  from the service record. §4.8's placeholder now reads "mcpwarp tunnel 0.6.0". Implementation status:
  ws-mixer-js v0.11.3 (`fbcc862`) fixes §11 rows 54–59; ws-mixer-go v0.10.0 (`4f87e0d`, `spec.pin` `v0.7.1`)
  ships §9.2 as written plus the Go-only `ServiceGroupOptions.Metrics` and `(*ServiceGroup).Client()`, which
  §1 allows; ws-mixer-python is green on `v0.7.1` (unreleased); Rust `ws-mixer-mcpwarp` 0.1.1 is in progress;
  mcpwarp/cli v0.2.5 fixed §11 row 51. Reason: mcpwarp-saas built the approved header change as tunnel 0.6.0, and
  the SDK sessions reported against `v0.7.1`. Changed: `MCPWARP.md` header, §4.8 (Edge headers, Trust gate),
  §9.2, §11 (rows 49–51 paragraph, rows 51 and 54–59). See [`MCPWARP.md`](./MCPWARP.md#48-multi-service-routing).

## 2026-10-24

Project owner's decisions. Anatoly decided on 2026-10-24, in the spec session, that no SDK ships a
multi-service controller (`D-2026-10-24-01`), and on 2026-10-23, answering the CLI's question, that `/mcp`-only
forwarding stays for every host (`-02`). `-03` records the small rulings given to the SDK and server sessions by
message after `v0.7.2`; `-04` the implementation status. No change to frames or the envelope, no new status code, no fixture removed.
Ships as spec `v0.8.0` (minor: a scope change).
`D-2026-10-24-05`, the note under `-01` and the update of `-04` ship as spec `v0.8.1`.

- **D-2026-10-24-01** — §6A is informative; no SDK ships a multi-service controller. Every SDK has the same
  single-service scope (`expose()`, the tunnel controller); publishing several services on one connection is the
  job of a multi-service host (the mcpwarp CLI in Go, mcpwarp desktop in Rust), which builds its own controller
  on the wire tier. §6A becomes "Multi-service hosts (informative)": the recommended controller for such a host,
  as an SDK-independent algorithm; its RFC keywords are recommendations for hosts. The wire-tier rules a host
  cannot avoid stay normative where they are: batches of 1..100 (§2.1 Batch), additive `register` and per-name
  replies (§2.3), one frame in flight and the `OVERLOADED` re-send (§2.6), the `disable`/`enable` shapes and
  per-name matching (§2.7), re-register on every `welcome` and host-side serialization of several controllers
  (§3.2), per-name take-over (§3.3), routing headers and the Trust gate (§4.8). `trustServerIdHeader` is the
  host controller's option; an SDK does no routing and has none. Reason: the owner's — every SDK gets one scope,
  so there is no exception to remember (cognitive load). Consequences: ws-mixer-go v0.11.0 removes its
  `ServiceGroup` controller (`NewServiceGroup`, `ServiceGroup*`, `ServiceConfig`, `ServiceState`,
  `ServiceStatus`, `ServiceGroupStatus`, `DescribeServiceStatus`, `StatusMessages.Services`, the group fixture
  runner, the `service.*` status-text tables) and keeps the wire helpers hosts build on (`RegisterEntry`,
  `EncodeRegisterList`, `EncodeUnregisterList`, `MaxServicesPerMessage`, the per-name decoded ops,
  `ServerIDHeader`/`ServerNameHeader`); the CLI builds its own layer (v0.3.0); mcpwarp desktop multiplexes
  host-side. Fixtures are reclassified, none removed: host guidance (SDK runners skip it by default) is tunnel
  `ms_01`..`ms_14`, the four `multi_service_*` bridging fixtures, the encode side of envelope
  `register_batch_valid` and `register_two_services`, the status-text `cases.json` cases with `input.service` and
  the two group cases using `lastError.NO_SERVICE_REGISTERED` (`rules.json` `hostOnlyCases`), and the keys
  `service.*` and `lastError.NO_SERVICE_REGISTERED` (`rules.json` `hostOnly`, OPTIONAL in an SDK's tables);
  everything else stays SDK conformance, every envelope decode case included. Supersedes in part
  `D-2026-10-18-02`, `D-2026-10-18-03` and `D-2026-10-19-01` (a note on each). Changed: `MCPWARP.md` header,
  Terms, §1, §2.1 (One service per message), §2.6 (`TOO_MANY_SERVICES` row, One in flight: pointers to §6A
  marked informative), §2.7 (multi-service `conflict` row), §3.2 (One controller per connection), §3.3 (Per name:
  clearing a take-over is the application's local decision), §4.4, §4.8 (step 1 wording, Trust gate, a note
  under the precedence table), §6.6 (one row), §6A (title, intro, §6A.7 Correlation, §6A.8, §6A.9), §7,
  §9 (wire helper rows, the Go `DescribeStatus` cell), §9.2 (table removed, informative note), §10.1, §10.2, §10.5, §10.6 (Fixture status),
  §11 (rows 49–51 and 61 as host notes, the SDK paragraph), §12 (host checklist); `spec/README.md` (Fixture
  classification), `tunnel/README.md`, status-text `rules.json` (`hostOnly`, `hostOnlyCases`) and `cases.json`
  description, `check-fixtures.mjs` comments. See [`MCPWARP.md`](./MCPWARP.md#6a-multi-service-hosts-informative).
  Note (`D-2026-10-24-04`): ws-mixer-go v0.11.0 (`c162fb7`, `spec.pin` `v0.8.0`) shipped the removal.
- **D-2026-10-24-02** — `/mcp`-only forwarding (§4.3) stays for every host, multi-service hosts included: no
  per-service path option. Decided by the owner on 2026-10-23, answering the CLI session. The CLI's v0.3.0
  documents it as a breaking change: legacy `/sse`, `/messages` and custom paths move to `/mcp`. Reason: the
  owner's — the same model as the Anki tunnel's single `/mcp` endpoint. Changed: `MCPWARP.md` §6A intro and §12
  host checklist (pointers only; §4.3 already says it). See [`MCPWARP.md`](./MCPWARP.md#43-path-rule).
- **D-2026-10-24-03** — Small rulings. (1) During an `OVERLOADED` pause an empty `registered` or an `error` op
  answers nothing and leaves the `OVERLOADED` counter unchanged (the next `OVERLOADED` is n + 1); "resets on any
  non-`OVERLOADED` reply" means a reply that answers the outstanding frame (ruled 2026-10-23 for ws-mixer-python).
  (2) A server SHOULD NOT send `retry_after_ms` above 60 000 ms; informative: the deployed mcpwarp tunnel sends
  none on a drain (both of its drain paths set a deadline of at most 25 s, no `retry_after_ms`, not
  configurable, unchanged in tunnel 0.6.0), and its only `Retry-After` is the HTTP `503` at the upgrade, fixed
  5 s (ruled 2026-10-23 for ws-mixer-server; facts from mcpwarp-saas). (3) Frames received between `welcome` and
  the attachment of the connection as current are queued and delivered, not dropped (from ws-mixer-go
  `D-2026-10-23-01`, its own log: a `drain` in that window was closed as stale and the client redialed per the
  close instead of handing over; rare, timing only). (4) A server-only consumer's `js.pin` names a ws-mixer-js
  release whose `spec.pin` is at least the consumer's `spec.pin`, ignoring spec tags that change only the
  mcpwarp layer (ruled 2026-10-23 for ws-mixer-server). Reason: questions from the Python, Go and ws-mixer-server
  sessions after `v0.7.2`. Changed: `MCPWARP.md` §2.6 `OVERLOADED` re-send, §5.9 (`onRetry` special cases),
  §6A.7 (Correlation, Backoff counter), §12 (Tier 1 `OVERLOADED` item); `WIRE.md` §2.9
  Client side of a drain (Replacement dial); `CLIENT-SDK.md` Handler delivery; `REPOS.md` Pin / tag policy. See
  [`MCPWARP.md`](./MCPWARP.md#26-errorcode).
- **D-2026-10-24-04** — Implementation status. ws-mixer-go v0.10.1 (`690968e`, `spec.pin` `v0.7.2`,
  2026-10-24) places a name removed and re-added within one `OVERLOADED` pause among the folded-in names in
  trigger order (§11 row 62), makes `Stream.Reset` reject connection-only codes locally (sentinel
  `wsmixer.ErrResetCode`; WIRE.md §2.8's SHOULD binds both roles, §11 row 63), handles a `GOING_AWAY`/`1000`
  close after a received `drain` as the hand-over even when the close is observed before the drain message, and
  retires a stream on `Reset`/`CloseWrite` before queuing the frame; ws-mixer-go v0.11.0 (`c162fb7`, `spec.pin`
  `v0.8.0`) removes `ServiceGroup` (`-01`) and queues frames received before a connection becomes current (`-03`
  (3); ws-mixer-go `D-2026-10-23-01` closed). mcpwarp tunnel 0.6.0 is live in production since 2026-10-24
  (mcpwarp-saas `1025905`, chart 0.10.0, infra `c7fb7ac`, 3 pods): operators may turn `trustServerIdHeader` on.
  Rust `ws-mixer-mcpwarp` 0.1.1 (with ws-mixer 0.2.1) pins `v0.8.0` at ws-mixer-rust `1f66fe5`; its three fixes
  for the `v0.7.2` single-service rules are in 0.1.1, so §11 row 60 stays "fixed in ws-mixer-mcpwarp 0.1.1".
  ws-mixer-js 0.11.4 (`a98a261`) pins `v0.8.0` and fixes 0.11.3's Handler delivery (§11 row 64). mcpwarp/cli
  v0.2.5 fixed §11 row 51 and v0.2.7 row 50 in part; rows 49 and 50's rest move to the CLI's own layer, v0.3.0. ws-mixer-python pinned `v0.7.2`, green, with two behaviour fixes (the `close()` skip
  condition; an empty `registered` during the pause); ws-mixer-python 0.1.0 (`e6f1052`) pins `v0.8.0`,
  unreleased: §11 rows 45 and 48 name it. ws-mixer-server v0.6.0 (`643b30f`) pins spec `v0.8.0`, ws-mixer-js
  0.11.4 and ws-mixer-go v0.11.0. Changed: `MCPWARP.md` §4.8 Trust gate (tunnel 0.6.0 live), §11 (rows 49–50
  action, new rows 62–63); at `v0.8.1`: the introduction (JS 0.11.4), §9.2, §11 (rows 45, 48, 60 and 62, the rows 49–51 paragraph, new row 64
  and its paragraph). See
  [`MCPWARP.md`](./MCPWARP.md#11-deviations-from-sdk-heads).
- **D-2026-10-24-05** — Rulings after the `v0.8.0` tag. (1) Frames in flight: while a frame is outstanding (on
  the wire, unanswered) a whole-frame `error` op with any code other than `OVERLOADED` and `CONNECTION_LIMIT`
  answers it; once `OVERLOADED` has dropped it and it is re-queued for the pause nothing is on the wire, so an
  `error` op or an empty `registered` during the pause answers nothing and leaves the `OVERLOADED` counter
  unchanged (the next `OVERLOADED` is n + 1). Single-service: a `registered` naming the service that arrives
  during the pause settles the handle, cancels the re-send and resets the `OVERLOADED` counter (the re-queued
  frame counts as answered); a stray `registered` with nothing outstanding and no pause changes the `OVERLOADED`
  counter not at all (the transient counter follows §3.2). In the `close()` case an `unregister` may be on the
  wire behind the dropped `register`; its `unregistered` answers that outstanding frame and resets the counter.
  (2) Schema verdicts (envelope `register_batch_valid` and `register_two_services`, `register_batch_101_invalid`)
  are checked by `check-fixtures.mjs` in this repo's CI, not by SDK runners; an SDK runner asserts wire verdicts
  on decode cases only, as `spec/README.md`'s verdict rule says. (3) Informative: a statically typed SDK whose
  config fields cannot carry JSON `null` or a wrong type MAY skip the cases of tunnel `07_invalid_config` its
  config type cannot express, at the finest granularity its types force (per case where possible:
  `ws-mixer-mcpwarp` skips only the `kind: "ftp"` event and its two snapshots; the whole fixture only when
  interleaving leaves no choice: ws-mixer-go skips it since v0.9.0), provided the SDK's own tests cover the
  expressible checks (whichever of the unset token, unset name and invalid `kind` its config type can express);
  dynamically typed SDKs (JS, Python) run it in full. (4) A connection that closes in the same delivery burst as its `welcome` never becomes current: its held
  frames are not delivered and its disconnect is reported with `phase` `"connected"`; a connected event emitted
  before it is allowed. Reason: the ws-mixer-js 0.11.4 review ((1): a `registered` during the pause), the
  `ws-mixer-mcpwarp` 0.1.1 re-pin questions ((1): the counter is the `OVERLOADED` one; (2); (3): per-case skip;
  (4)), the ws-mixer-python 0.1.0 re-pin ((1): the `close()` case; (2)) and the ws-mixer-go v0.11.0 runner
  report ((1): outstanding versus re-queued; (3)). Changed: `MCPWARP.md` §2.6 `OVERLOADED` re-send, §2.7
  (`registered` row), §3.2 (Transient retry row), §10.1 (Multi-service row), §10.6 (Fixture status, `07_invalid_config` note); `CLIENT-SDK.md` Handler delivery;
  `spec/README.md` Fixture classification. See [`MCPWARP.md`](./MCPWARP.md#26-errorcode).
