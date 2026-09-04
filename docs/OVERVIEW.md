# ws-mixer-spec — overview

This repo is the `ws-mixer.v1` specification: the normative wire spec, client-SDK requirements, the
protocol decision log, the conformance fixture corpus, and the cross-SDK conformance runner. It contains no
implementation — no server, no client, no adapters. See [`REPOS.md`](./REPOS.md) for where those live.

Research inputs (evidence, prior art and rejected alternatives live there, not in the normative docs):

- [pass 1 — framing and encoding](./research/2026-08-25-framing-and-encoding.md)
- [pass 2 — flow control and stream lifecycle](./research/2026-08-25-flow-control-and-stream-lifecycle.md)
- [pass 3 — control channel and connection lifecycle](./research/2026-08-26-control-channel-and-connection-lifecycle.md)

---

## What ws-mixer is, and is not

**Is:**

- A library that carries **N independent byte streams over one WebSocket connection**, in both directions.
- **Standalone.** No mcpwarp types, no HTTP types, no MCP types anywhere in the core.
- **Symmetric in data, asymmetric in control.** Only the server opens streams; both sides read, write and close them.
- **Flow-controlled.** Every stream has a credit window, so one slow reader cannot stall the others.
- **Observable.** Every failure carries a numeric code *and* a human message that contains a number that came off the wire.
- **Small.** The wire spec is meant to be implementable by one Python developer in an afternoon.
- **Three SDKs from one spec**: Go (server), JS (client), Python (later) — all validated by the same fixtures.

**Is not** (non-goals, deliberately):

| Non-goal | Why |
|---|---|
| HTTP awareness | ws-mixer moves opaque bytes. The HTTP layer rides *above* it, in the DATA payload. |
| Stream resumption after reconnect | Requires send buffering + a resumable session + a resync handshake. Reconnect is normal and cheap instead ([pass 3 §5.3](./research/2026-08-26-control-channel-and-connection-lifecycle.md)). |
| Routing, naming, service discovery | ws-mixer has one connection and N *anonymous* streams. Who a stream is "for" is the application's business. |
| Raw TCP / TLS transport | WebSocket only, forever. This is what lets us drop the frame length field. |
| Compression | `permessage-deflate` is explicitly disabled. Compress in the layer above, where it can be content-aware. |
| Priorities, weights, connection-level window | One round-robin scheduler, one per-stream window. HTTP/2 deprecated priorities for good reason. |

---

## Where to go next

| Looking for | Read |
|---|---|
| The normative wire spec — framing, streams, flow control, control channel, error codes, sequences | [`WIRE.md`](./WIRE.md) |
| What every client SDK must do, independent of language | [`CLIENT-SDK.md`](./CLIENT-SDK.md) |
| Why a protocol rule is the way it is | [`DECISIONS.md`](./DECISIONS.md) |
| Which repo owns what, and the pin/tag policy between them | [`REPOS.md`](./REPOS.md) |
| The conformance fixtures and how they're structured | [`../spec/README.md`](../spec/README.md) |
| The cross-SDK conformance runner | [`CONFORMANCE.md`](./CONFORMANCE.md) |
| How mcpwarp itself builds on ws-mixer, and Go/JS implementation notes | that implementation's own repo (see [`REPOS.md`](./REPOS.md)) — this is product- and language-specific material that lives outside the spec |
