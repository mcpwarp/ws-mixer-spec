# ws-mixer — cross-SDK conformance runner

Date: 2026-08-27 · Status: **design, before any code**

Normative inputs: [`WIRE.md`](./WIRE.md) (wire spec), [`REPOS.md`](./REPOS.md) (repo layout), and
[`../spec/README.md`](../spec/README.md) (fixture formats). Where this document and WIRE.md
disagree, WIRE.md wins.

## 0. What this exists for

Today each SDK proves itself against `spec/fixtures/sequences/*.json` **in its own language, against its
own fake transport** (`go/wsmixer/sequence_test.go`, `js/test/sequence.test.ts`). That catches a wrong state
machine but not a wrong *wire*: the JS suite skips all 19 `role:"server"` fixtures because it has no server,
and the Go suite skips the three `wait_ms` fixtures. `js/test/interop.test.ts` is the only real cross-language
check and it covers exactly one happy path.

The conformance runner closes that gap with one rule:

> Every SDK is validated by the same fixtures, over a real WebSocket, against a peer it did not write.

Two things follow. Adversarial fixtures need a peer that will happily emit illegal bytes — no SDK will —
so the runner ships its **own raw wire actor**. Happy paths need two real SDKs talking to each other, so
the runner drives **SDK ↔ SDK pairs**. A new SDK joins by shipping one executable adapter and nothing else.

---

## 1. Harness protocol

Each SDK ships an executable at `conformance/adapters/<lang>/`. It reads **JSON Lines commands on stdin**
and writes **JSON Lines events on stdout**; stderr is free-form logging, captured and attached to failures.
One adapter process = **one connection**, one role. The runner spawns a fresh process per test case and
kills it after. All binary is base64 (`_b64` suffix on every such field).

Every command carries `seq` (monotonic int). Every event carries `seq` when it is a direct consequence of a
command, and omits it when autonomous (`data`, `stream_opened` on a client, `drain`, `disconnected`).
The adapter emits `ready` once at startup and `ack{seq}` when a command's local action has completed.

### 1.1 Commands (11)

| `cmd` | Fields | Role | Meaning |
|---|---|---|---|
| `set_options` | `window`, `max_streams`, `ping_interval_ms`, `ping_timeout_ms`, `hello_timeout_ms`, `time_scale`, `floor_ms`, `allow_subfloor_timing` | both | Must precede `listen`/`connect`. Maps onto the SDK's existing test-only knobs (Go `Options`, JS `ConnOptions` + `_timing`). `floor_ms` is the runner's own effective timing floor (default 300; see §3.4) — every scaled duration the adapter computes itself (e.g. a `drain.deadline_ms` it receives verbatim) is clamped to at least this, replacing each adapter's own hardcoded floor. |
| `listen` | `addr` (default `127.0.0.1:0`) | server | Start a ws-mixer server. Replies `listening{url}`. |
| `connect` | `url`, `token`, `reconnect` (`{enabled, maxAttempts, baseMs?, capMs?}`, default `{enabled:false}`) | client | Dial. Replies `connected{welcome}` or `error`. `reconnect.enabled:false` (the default, used by every fixture-mode/one-shot pair scenario) disables the SDK's own background reconnect loop entirely, so a spurious first-attempt failure fails fast instead of retrying silently forever. Only `drain_reconnect` and `application_close` set `reconnect.enabled:true`, letting the client's own reconnect loop redial after the server drains it (or closes it with `APPLICATION_CLOSE`); a successful post-drop reconnect is then surfaced as `reconnected` (§1.2), not a second `connected`. `baseMs`/`capMs` override WIRE.md §2.9's backoff `base`/`cap` (defaults 1000/60000) for that connection when present; an adapter implementing `reconnect.enabled:true` MUST honour them — they exist so a scenario exercising a start-at-cap path (`4009`, `4014`) can pin `capMs` low enough to finish inside the runner's fixed 20 s per-step timeout (`conformance/runner/driver/send.go`'s `defaultTimeout`) instead of waiting up to the real 60 s default. Additive: a fresh field on a command's *input*, not an event, so unlike `t_ms` below there is no external-consumer stability promise at stake, but the same rule applies in spirit — an adapter that doesn't yet read `baseMs`/`capMs` isn't wrong on the wire, only slow/flaky on the one scenario that sets them. |
| `open_stream` | — | server | `OpenStream()`. Replies `stream_opened{id}`. |
| `write` | `id`, `data_b64` | both | Write payload bytes (SDK chunks/credits as it sees fit). `ack` after the last byte is accepted. Every adapter always auto-delivers received bytes to the application as `data` events (no `auto_read`/explicit `read` knob exists — removed as dead code that no scenario ever exercised; see §1.3). |
| `close_write` | `id` | both | Send `CLOSE`. |
| `reset` | `id`, `code`, `message` | both | Send `RESET(code)`. |
| `send_app` | `body` | both | `app{body}` on stream 0. |
| `drain` | `reason`, `deadline_ms` | both | `Conn.Drain`. Client may only use `client_requested`. |
| `close` | `code`, `message` | both | `code` absent or `0` (`NO_ERROR`) is the SDK's own graceful shutdown (for a client, WIRE.md §2.10 step 14: `drain{client_requested}` → grace → `error{NO_ERROR}` → close `1000`, where the SDK implements it — `graceful_close` uses this). A non-zero `code` is an immediate close with that ws-mixer code and `message` via the SDK's application-close API (`error{code,message}` + WS close `4000+code`, WIRE.md §2.8) — `application_close` uses this with `code:14`. |
| `shutdown` | — | both | Flush stdout, exit 0. |

### 1.2 Events (13)

| `event` | Fields | Emitted when |
|---|---|---|
| `ready` | `sdk`, `sdk_version`, `roles` | Process start. `roles` is `["server"]`, `["client"]` or both. |
| `ack` | `seq` | The command with that `seq` finished locally. |
| `listening` | `url` | After `listen`. |
| `connected` | client role: `welcome` (verbatim object), `session`. Server role: `role:"server"`, `session`, `hello` (`{agent, meta?}` — the connecting client's `hello.agent`/`hello.meta`, verbatim; `meta` omitted when the client sent none) | Client role: got `welcome` for the first time on this process. Server role: a client's handshake completed (`OnConn`/the SDK's equivalent, fired once, after `hello` is accepted) — the server side never sees its own `welcome`, only the peer's `hello`, so it reports that instead. |
| `reconnected` | same payload shape as `connected` (`welcome`, `session`) | The client's own SDK-internal reconnect loop redialed and got a fresh `welcome` after an earlier disconnect (only possible when `connect.reconnect.enabled:true`, §1.1). Never emitted for the initial connect. |
| `stream_opened` | `id` | Locally opened (server) or `OPEN` received (client). |
| `data` | `id`, `data_b64`, `t_ms` | Bytes delivered to the application. |
| `stream_closed` | `id`, `direction` (`read`\|`write`\|`both`), `t_ms` | `CLOSE` received / sent, EOF surfaced. `read`/`write` fire the moment that one half closes, same as before; `both` is an *additional* event emitted right after whichever of the two fires second, once both halves of a stream are closed. Consumers that only care about the wire-level half-close keep matching `read`/`write` unchanged; a `both` event is simply an extra one they don't have to await. |
| `stream_reset` | `id`, `code`, `name`, `message`, `t_ms` | `RESET` received or emitted autonomously by the SDK. |
| `app` | `body` | `app` received. |
| `drain` | `reason`, `last_stream_id`, `deadline_ms` | `drain` received. |
| `disconnected` | `ws_code`, `error_code`, `error_name`, `message`, `fatal`, `close_reason`, `phase` | Connection ended, for any reason. Always emitted exactly once per connection (a reconnect that follows starts a new "connection" for this purpose — a fresh `connected`/`reconnected` up to the next `disconnected`). `close_reason` is the peer's close-frame reason verbatim when one was received, omitted otherwise (CLIENT-SDK.md's `closeReason`). `phase` is `"dial"` \| `"handshake"` \| `"connected"` (CLIENT-SDK.md's disconnect-reason shape). Both are additive fields, per the rule below. |
| `error` | `message`, `seq?`, `unsupported?`, `ok?` | A command failed, or the adapter itself broke. Never used for protocol errors — those are `disconnected`. A role-inapplicable command (§1.3, e.g. the JS adapter's `listen`/`open_stream`) replies `{"event":"error","seq":..,"ok":false,"unsupported":true,"error":"unsupported","message":"..."}`; the runner's `isUnsupported` check (`conformance/runner/adapter/adapter.go`) turns that into a SKIP rather than a FAIL. |

`t_ms` is an integer: milliseconds since the adapter process started, monotonic (not wall-clock, not
comparable across processes). It exists for external tooling that wants to eyeball relative timing of a
byte stream without parsing adapter stderr; the runner itself ignores it. This protocol is versioned here
(this document); adding a field like `t_ms` is an additive-only change and does not bump anything — see
`conformance/README.md`'s "Using the Go adapter as a fake tunnel" section for the stability promise this
protocol makes to external consumers.

An adapter is ~250–400 lines. It is a thin shell over the SDK's public API plus the one test-only timing
hook; it contains **no protocol logic of its own** — if an adapter needs to reimplement part of the spec to
pass, that is the bug.

### 1.3 Deliberately not covered by the adapter protocol

| Not covered | Why, and where it is covered instead |
|---|---|
| Raw / illegal frames | The raw actor's job (§2). An SDK that can emit an illegal frame on request is a liability. |
| Reconnect + backoff policy | Client-local, not on the wire. Per-SDK unit tests; the pair scenario `drain_reconnect` covers the observable part. |
| WS handshake failures (400/401, missing subprotocol echo) | HTTP-level, no mux frames involved. Per-SDK unit tests. |
| TLS, proxies, compression | Deployment concerns; `permessage-deflate` is simply off everywhere. |
| Metrics, hooks, log output | Not wire-observable. |
| `hello.meta` / `welcome.meta` semantics | Opaque to ws-mixer by definition. Passed through `set_options` only if a future fixture needs it. |
| `send_window` introspection | Only `window_exhaustion_then_resume.json` asserts it; needs an SDK-internals peephole every adapter would have to fake. Stays a per-SDK unit test. |
| More than one connection per process | Keeps adapter state to a single struct. The runner spawns two processes instead. |
| A pair-mode `window_exhaustion` scenario (explicit `read`/`auto_read:false`) | Both adapters briefly implemented `auto_read:false` + an explicit `read` command for this, but no scenario or fixture ever set `auto_read:false` (`set_options` always sends `auto_read:true`/omits it) or issued `read` — removed as dead code. Window credit/exhaustion behavior stays covered by each SDK's own unit tests plus `window_exhaustion_then_resume.json` (fixture mode, driven without this command). |

---

## 2. Raw wire actor

**Decision: one raw actor, implemented once inside the runner, with its own independent frame codec.**

| Property | Choice | Why |
|---|---|---|
| Where | `conformance/runner/rawactor/` | Not an adapter. No SDK ships one; no SDK can be blamed for it. |
| Codec | ~80 lines of hand-rolled `binary.BigEndian`, **not** an import of `go/wsmixer` | If the actor shared the Go SDK's codec, a Go codec bug would be invisible to the Go SDK's own conformance run. |
| Roles | Both. `rawactor.Serve(addr)` accepts an upgrade at `/v1/tunnel`; `rawactor.Dial(url)` connects. | A `role:"server"` fixture needs a raw *client*; a `role:"client"` fixture needs a raw *server*. Symmetric by construction. |
| Behaviour | None. It never auto-pongs, never credits, never validates. It sends exactly the bytes a step names and records exactly the frames it observes. | Any autonomous behaviour would make the actor a second implementation to debug. |
| Observations | An append-only log of `{t_ms, decoded_frame}` plus the WS close `{code, reason}`. | This log is the authority for every wire-level `expect` (§4). |

Its API is three calls: `Send(frameBytes)`, `Expect(matcher, timeout)`, `Log()`. Everything hostile in the
corpus — DATA over credit, DATA for a never-opened id, `WINDOW` on stream 0, a 65 545-byte message, a
high-bit stream id, silence past the hello timer — is "send these bytes" or "send nothing", so the actor
needs no special cases.

**This is what unlocks the matrix.** `role:"server"` fixtures run against the JS *client* today for the first
time (raw actor plays the server); `role:"client"` fixtures run against the Go *server* (raw actor plays the
client). Neither is possible with the in-process fake transports the SDKs use now.

---

## 3. The runner

### 3.1 Language: **Go**

| Candidate | For | Against |
|---|---|---|
| **Go (chosen)** | Ships as one static binary — CI and a laptop both run `./conformance/runner` with no `npm ci`, no `node_modules`, no venv. `os/exec` + goroutines + `context` make "two subprocesses, one raw WS peer, four deadlines" ~50 lines. `coder/websocket` is already a repo dependency and is one of the few WS libraries that will let you send a deliberately oversized message and read a close code straight off the wire. `go test ./conformance/...` is already a required CI check shape. | The Go SDK's author also owns the runner — a shared blind spot is possible. Mitigated by the independent codec (§2). |
| TypeScript | `js/test/interop.test.ts` already proves the spawn/JSON-lines pattern works under vitest; the JS SDK has a frame codec to crib from. | Needs `npm ci` before conformance can run at all, which makes it awkward as the gate on a Python-only PR. `ws` hides the close code behind an event and buffers aggressively — worse for byte-exact assertions. |

Neither choice touches the Python SDK: **the Python developer writes an adapter and never opens the
runner.** That is the property that actually matters, and both candidates have it. Go wins on "one binary,
zero setup".

Layout note (post-split): the runner is a package of this repo's own root module
(`github.com/mcpwarp/ws-mixer-spec`, at `./conformance/runner`) — it ships no SDK adapters of its own. Each
SDK's adapter lives in that SDK's own repo (e.g. `ws-mixer-go/conformance/adapter/`) with its own module,
depending on that SDK's module directly rather than via a `replace` directive, and is built into an
executable `run` shim the runner discovers under `--adapters-dir` (§5).

### 3.2 Matrix

| Mode | Source | Under test | Peer | Runs |
|---|---|---|---|---|
| **fixture** | `spec/fixtures/sequences/*.json` | the SDK matching the fixture's `role`, one at a time | raw actor in the opposite role | (#fixtures × #SDKs supporting that role) |
| **pair** | `conformance/scenarios/*.json` | both | each other | every ordered (server SDK, client SDK) pair |

With Go (server+client) and JS (client-only): fixture mode runs the 19 `role:"server"` fixtures against
Go-as-server, and the 24 `role:"client"` fixtures against both the Go and JS clients (43 (SDK, fixture)
cells total); pair mode runs Go↔Go and Go↔JS across all 9 scenarios (18 cells). Adding Python (client
first) adds one column to each with no runner change.

Pair mode uses a **command-level** DSL, not the sequence fixtures — a scenario names actors and commands,
never raw frames, because with two real SDKs nobody is in a position to emit a hostile byte:

```json
{ "description": "N streams round-trip, then drain and reconnect",
  "steps": [
    { "actor": "server", "cmd": { "cmd": "open_stream" } },
    { "actor": "server", "cmd": { "cmd": "write", "id": 1, "data_b64": "cmVx" } },
    { "actor": "client", "await": { "event": "data", "id": 1, "data_b64": "cmVx" } },
    { "actor": "server", "cmd": { "cmd": "drain", "reason": "rollout", "deadline_ms": 2000 } },
    { "actor": "client", "await": { "event": "drain", "reason": "rollout" } } ] }
```

Initial scenarios: `happy_roundtrip`, `n_streams_fanout` (3 concurrent, from `interop.test.ts`),
`app_roundtrip`, `half_close_sse` (server closes write, client streams for a while), `drain_reconnect`,
`big_stream_flow_control`, `drain_with_inflight`, `graceful_close`. (An originally-planned
`window_exhaustion` scenario built on `auto_read:false` + explicit `read` was dropped — §1.3 — since
nothing ever exercised that command pair; window credit/exhaustion is covered by
`window_exhaustion_then_resume.json` in fixture mode instead.)

### 3.3 Driving a fixture step

A fixture `send` step is one of two very different things, and the runner must tell them apart:

| Step payload | Runner action |
|---|---|
| `OPEN` | `open_stream` command, then assert the raw actor observes `OPEN(id)` |
| `DATA`, `CLOSE` | `write` / `close_write` command, then assert |
| `RESET` | `reset` command **unless** the preceding step created the condition for an autonomous reset (`STREAM_LIMIT`, `STREAM_CLOSED`, a drain deadline) — then observe only |
| `app` | `send_app` command, then assert |
| `drain` | `drain` command, unless deadline-driven |
| `hello`, `welcome`, `ping`, `pong`, `error` | **observe only** — produced by the handshake, the ping loop or the fail path |
| a `recv` step (any payload) | raw actor sends those bytes |

Classification is by message/frame type, with a sidecar `conformance/scenarios/step-driving.json` holding
the handful of `{fixture, step_index}` overrides (the RESET row above). The sidecar exists so that
`spec/fixtures/` stays untouched — see open question 1.

### 3.4 Time

`wait_ms` is never a real sleep. Every duration is multiplied by a global `--time-scale` on **both**
sides. As-built this defaults to `1/50`, not this section's originally-suggested `1/250` — raised
after real IPC/scheduling jitter across two separate processes made the faster scale flake (see
`conformance/README.md`'s "Timing floor" note):

| Fixture value | Where it lands | Scaled |
|---|---|---|
| `wait_ms: 90001` (dead peer) | runner's own sleep + adapter's `ping_timeout_ms` | 360 ms |
| `wait_ms: 25000` (drain deadline) | runner's sleep + `drain.deadline_ms` | 100 ms |
| hello timer, 10 000 ms | adapter's `hello_timeout_ms` | 40 ms |

Every scaled duration is clamped to a floor so a loaded CI box does not turn a 4 ms window into a flake.
The floor is not hardcoded per adapter: the runner computes its own effective floor (raised from an
initial 20 ms to 300 ms after real IPC/scheduling jitter across two separate processes made 20-40 ms
flake — see `conformance/README.md`) and sends it as `set_options.floor_ms`; both adapters apply that
value instead of a local constant, falling back to 300 ms only if a run never sends one at all (e.g. a
hand-invoked adapter outside the runner). `set_options.allow_subfloor_timing:true` is **required**
separately from `floor_ms`: a `welcome` carrying `ping_interval: 120` violates the spec's 5 000 ms floor
and a conformant client must reject it, so the client adapter has to be told this run is scaled. Go
exposes this today via `wsmixer.AllowSubfloorTiming(*Options)` — an unexported `Options.allowSubfloorTiming`
field settable only from `go/wsmixer/conformance_hooks.go`, which carries a `//go:build conformance` tag,
so a normal `go build ./...` of `go/wsmixer` never even sees the hook; the Go adapter must therefore be
built with `-tags conformance` (`conformance/README.md`, and the Makefile/`build.go` note for whoever
builds it). The floor check itself lives in `Conn.applyWelcome` (client.go), not in `control_messages.go`'s
`parseWelcome` — that file is a stateless wire parser shared by every `ParseControl` caller and has no
notion of this test-only escape hatch, so gating it there would have meant threading a bypass through a
widely-shared, tested code path for a test-only concern. JS exposes the equivalent via `ConnOptions._timing`,
forwarded from the public `connect()` entry point's `ConnectOptions._timing` (`js/src/client.ts`).

### 3.5 Output

```
$ build/conformance-runner --time-scale 250 --sdk go,js --report build/conformance.xml
# equivalently: cd conformance/runner && go run . --time-scale 250 --sdk go,js --report ../../build/conformance.xml

MODE     SDK        FIXTURE/SCENARIO              RESULT
fixture  go-server  credit_violation              PASS
fixture  go-server  drain_with_inflight_timeout   PASS
fixture  js-client  max_streams_exceeded          PASS
fixture  js-client  frame_before_welcome          FAIL  close_code = 4001, want 4003
fixture  py-client  *                             SKIP  no adapter at conformance/adapters/python
pair     go→js      n_streams_fanout              PASS

42 passed · 1 failed · 21 skipped (1 adapter missing: python)
```

- `--sdk go,js` filters; `--fixture <glob>` and `--mode fixture|pair` narrow further.
- A missing or non-executable adapter is a **listed SKIP**, never a silent pass and never a hard failure —
  so a Go-only PR still runs green while Python is still being written.
- `--report` writes JUnit XML (one `<testsuite>` per SDK, one `<testcase>` per fixture, adapter stderr in
  `<system-err>`); skips become `<skipped message="...">`.
- Exit code `1` on any FAIL, `0` when everything is PASS or SKIP.
- Every failure prints the raw actor's decoded frame log and the adapter's event log, interleaved by
  timestamp. This is the whole debugging story and it must be good.

---

## 4. Mapping fixture `expect` → observable events

The runner asserts **only the keys present** in an `expect` step (spec/README.md's rule), at that point in
the transcript, with a bounded wait (`5 × time-scaled` grace, ~40 ms) before failing.

| `expect` key | Raw actor observes | Adapter observes | Authority |
|---|---|---|---|
| `frame` | the next decoded frame matches `type` / `stream_id` / `payload_hex` \| `payload_length` / `code` | — | **raw actor** — this key is about bytes |
| `close_code` | WS close frame status code | `disconnected.ws_code` | **raw actor**, because a browser-shaped client may never surface the code; adapter checked as a secondary assertion when it reports one |
| `error_code` | last stream-0 `error{code}` before the close, name-mapped via §2.8 | `disconnected.error_name` | **both** — the raw actor proves it went out on the wire in the right order (`error` immediately before close), the adapter proves the SDK surfaced it to the application. Mismatch between the two is itself a failure |
| `error_code: null` | no `error` frame and no close within the grace window | no `disconnected` event | **both** — "still alive and healthy" is only true if neither side saw a death |
| `stream_reset_code` | RESET payload code, when the SDK-under-test sent it | `stream_reset.name`, when the raw actor sent it | **whichever side received it** — direction is taken from the fixture step that produced the RESET |
| `stream_state` | — | runner replays `stream_opened` / `stream_closed{direction}` / `stream_reset` into the §2.5 state machine and compares | **adapter** — state is per-SDK bookkeeping, not a wire artifact. The runner derives it from events rather than adding a `get_state` command, so an adapter cannot pass by asserting its own opinion |
| `send_window` | — | — | **not asserted** (§1.3) |

Two invariants the runner checks on every fixture, whether or not the fixture says so:

1. `close_code == 4000 + error_code` (`1000` for `NO_ERROR`) — the §2.8 mechanical rule, on live bytes
   rather than on fixture JSON, where `check-fixtures.mjs` already checks it.
2. `error` is the **last** stream-0 message before the close, and no second `error` follows a received one.

---

## 5. Repo layout, hooks, CI

This repo (`ws-mixer-spec`) ships the runner and the fixtures/scenarios it replays. It ships **no SDK
adapters** — adapters move with their SDKs, so each SDK's toolchain and version stay with that SDK's own
repo instead of making this repo depend on every implementation. This repo's own layout:

```
ws-mixer-spec/
  go.mod go.sum              root module github.com/mcpwarp/ws-mixer-spec; the runner is a package
                              of it (./conformance/runner), not a nested module
  spec/
    fixtures/, control.schema.json, tools/     the conformance oracle -- see spec/README.md
  conformance/
    COUNTS.json               unused here (this repo runs no adapters, so it checks no floor) --
                               kept only as a documented format reference; each consumer repo ships
                               its own (see below)
    README.md                 how to run the runner; how to add an adapter (the Python checklist, verbatim)
    runner/
      main.go, build.go       flags, generic adapter discovery/build, matrix expansion, report writing
      driver/                 fixture + pair driving logic, the §4 expect mapping
      adapter/                spawn, JSON-lines codec, event bus, teardown
      wire/, codes/, fixture/, report/   independent frame codec, error-code table, fixture loader, output
      rawactor/               independent codec + WS peer, both roles
    scenarios/
      *.json                  pair-mode scenarios, including `drain_reconnect` (§1.1/§1.2's
                               `reconnect`/`reconnected` coordination contract)
      step-driving.json       per-fixture step-driving overrides (§3.3)
```

**Adapters live in each SDK's own repo**, at `conformance/adapter/` (singular — this repo's
`conformance/scenarios/` and `conformance/runner/` are the only survivors of the old plural
`conformance/adapters/`):

| Repo | Adapter path | Notes |
|---|---|---|
| `ws-mixer-go` | `conformance/adapter/` | built with `-tags conformance` so `wsmixer.AllowSubfloorTiming` (that repo's `conformance_hooks.go`) exists. Also produces `ws-mixer-server`'s adapter via a `ServerBackend` interface it exports (see that repo's own docs) — the public repo tests `go-client`/`go-server` via `wsmixer.AcceptConn`, the private server repo tests the real `Listener`. |
| `ws-mixer-js` | `conformance/adapter/` | `adapter.mjs` imports the package's own built `dist/index.js`, not `src/*.ts` directly (a plain `node` process has no loader for a bare `.ts` import). |
| `ws-mixer-server` | `cmd/conformance-adapter/` | wires the real `wsmixerserver.Listener`; this is the only adapter that proves the production HTTP/upgrade layer, not just the wire core. |
| `ws-mixer-python` | (later) | not built yet |

### Finding fixtures and adapters from outside this repo

The runner has no notion of "the monorepo root" — every path it needs is a flag, each with a sane default
for the common case of running it from inside this repo:

| Flag | Default | Meaning |
|---|---|---|
| `--spec-root <dir>` | walk up from cwd looking for `spec/fixtures/sequences` | root containing `spec/fixtures/` and `conformance/scenarios/` |
| `--adapters-dir <dir>` | `<spec-root>/conformance/adapters` | directory containing one subdirectory per SDK adapter |
| `--counts <path>` | `<spec-root>/conformance/COUNTS.json` | the pass-count floor file (§3.5) |
| `--adapter name=path` | — | override discovery for one SDK with an already-built binary (repeatable, comma-separated) |

An SDK repo's CI does not check out this repo's `conformance/adapters/` at all (it doesn't exist there
anyway) — it fetches this repo at its pinned tag (`spec.pin`, see [`REPOS.md`](./REPOS.md)), builds its own
adapter locally, and points the runner at both:

```sh
go run "$SPEC/conformance/runner" \
  --spec-root "$SPEC" \
  --adapter go=./build/go-adapter/run \
  --sdk go --mode fixture \
  --counts ./conformance/COUNTS.json \
  --report ./build/conformance.xml
```

`--adapters-dir` exists for the case of *several* locally-built adapters in one directory (e.g.
`ws-mixer-server`'s `conformance-matrix` job, which builds its own adapter **and** fetches + builds
`ws-mixer-js`'s, see the CI table below) rather than naming each one individually with `--adapter`.

Every adapter, without exception, is discovered and started the same way: a subdirectory of
`--adapters-dir` containing an executable `run` shim (`docs/CONFORMANCE.md` §6, step 1). The runner has no
per-language special case — it never invokes `go build` or `npm run build` itself; that is each SDK
repo's own CI job, upstream of the runner invocation.

A build/exec failure for an SDK named explicitly on `--sdk` is **fatal** (the caller asked for that SDK by
name); the same failure for an SDK that was only discovered, or named only via `--adapter`, degrades to a
**SKIP** row per fixture/scenario — never a hard failure, so a Python-only PR run doesn't fail because Go
wasn't built.

### What CI runs where

| Repo | Job | What it runs | Fetches |
|---|---|---|---|
| **spec** (this repo) | `check-fixtures` | `node spec/tools/check-fixtures.mjs` (schema + `spec/fixtures/COUNTS.json` floor) | — |
| | `runner-selftest` | `go test ./conformance/runner/...` (`wire/`, `adapter/` against `testdata/fakeadapter`, `main_test.go`; `e2e_test.go` skips cleanly with no adapter env vars set) | — |
| | `schema-drift` | `node spec/tools/gen-relaxed-schema.mjs && git diff --exit-code` | — |
| **go** | `test` | `go test ./...` + `-race` (`GOMAXPROCS=1`) | this repo @ `spec.pin` |
| | `conformance` | build `cmd/conformance-adapter -tags conformance`; runner `--sdk go --mode fixture` | this repo @ `spec.pin` |
| **js** | `test` | `npm run typecheck && npm test` | this repo @ `spec.pin`, `ws-mixer-go` @ `goserver.pin` |
| | `conformance` | `npm run build`; runner `--sdk js --mode fixture` | this repo @ `spec.pin` |
| **server** | `test` | `go test ./...` (HTTP-layer unit tests) | — |
| | `conformance-matrix` | build its own adapter **and** fetch+build `ws-mixer-js`'s; runner `--mode all` (full fixture columns + the pair matrix); JUnit artifact | this repo @ `spec.pin`, `ws-mixer-js` @ pin |

`COUNTS.json` floors are per-consumer, not shared: each of `ws-mixer-go`, `ws-mixer-js` and
`ws-mixer-server` ships its own `conformance/COUNTS.json` with only the keys it actually exercises
(`go-server`/`go-client` for the go repo; `js-server`/`js-client` for js; all six keys, including the pair
matrix, for the server repo). This repo ships no floor of its own — it runs no adapters, so it has no
pass-count keys to guard.

CI in every consumer repo, one required job **from the first commit** — the discipline
[`OVERVIEW.md`](./OVERVIEW.md) notes [cloudevents/conformance](https://github.com/cloudevents/conformance)
lacked:

1. Fetch this repo at `spec.pin` (a composite `fetch-spec` action reading the pin file).
2. Build that repo's own adapter(s).
3. Run the runner with `--report build/conformance.xml`.
4. Upload the XML + all adapter stderr as artifacts, always.
5. Fail the job on a non-zero exit. **A SKIP does not fail; a drop in the PASS count does** — the runner
   checks the invoking repo's `--counts` file (a checked-in floor, same trick as `spec/fixtures/COUNTS.json`)
   and fails if the number of passing (SDK, fixture) cells falls below it. That is what stops "adapter got
   deleted, CI went green".

---

## 6. Adding the Python adapter

A day's work, in this order. Nothing outside the two files in step 1 may need to change.

| # | Step | Detail |
|---|---|---|
| 1 | Create `conformance/adapters/python/` | `adapter.py` (executable, `#!/usr/bin/env python3`) + `run` (a 2-line shell shim so the runner has one uniform way to start any adapter: `exec python3 adapter.py`). |
| 2 | Read stdin line by line | `for line in sys.stdin: cmd = json.loads(line)`. Dispatch on `cmd["cmd"]`. Emit with `print(json.dumps(e), flush=True)` — **`flush=True` on every line**, or the runner deadlocks. |
| 3 | Emit `ready` | `{"event":"ready","sdk":"ws-mixer-py","sdk_version":"0.1.0","roles":["client"]}`. Client-only is fine; server-role fixtures are then reported as SKIP for Python. |
| 4 | Implement 11 commands | Client-only adapters must reply `{"event":"error","seq":..,"ok":false,"unsupported":true,"error":"unsupported","message":"..."}` (not a bare `error{"message":"unsupported command"}`) to `listen` and `open_stream` — the runner's `isUnsupported` check (`conformance/runner/adapter/adapter.go`) looks for `unsupported:true` or `error:"unsupported"` specifically, and only that shape is treated as a SKIP on a role-inapplicable command rather than a FAIL. `connect` gets the same `unsupported` reply if it's asked for `reconnect.enabled:true` and the SDK has no reconnect loop of its own (an adapter whose SDK has none replies `unsupported` instead of hanging, and the runner then SKIPs that SDK's `drain_reconnect` and `application_close` cells rather than timing out — `ws-mixer-go/conformance/adapter/adapter.go`'s `connect` handler is the reference for the supported branch, since the Go SDK's reconnecting client means it always takes that path today). An adapter that does implement `reconnect.enabled:true` must also forward `reconnect.baseMs`/`reconnect.capMs`, when present, into the SDK's own backoff `base`/`cap` (§1.1) — `application_close` sets a small `capMs` specifically so its start-at-cap reconnect finishes inside the runner's step timeout. `shutdown` is real but never actually sent by this runner — every adapter is torn down by killing its whole process group (`conformance/runner/adapter/adapter.go`'s `Kill`), so an adapter only needs `shutdown` for manual/other-harness use, and must exit cleanly on SIGKILL of the group either way (no cleanup step that only `shutdown` would have triggered may be relied on). |
| 5 | Implement 13 events | `seq` is echoed on `stream_opened` (server role only — client-role `stream_opened` from a received `OPEN` is autonomous and omits it, §1), `connected`, and `listening`, and each is emitted *after* the matching `ack` for the command that caused it (ack-then-event, never the reverse — `ws-mixer-go/conformance/adapter/adapter.go`'s `listen`/`connect`/`open_stream` handlers are the reference order). `disconnected` must fire exactly once for every ending (clean, protocol error, transport drop), but its fields are conditional, not always-present: `error_code`/`error_name`/`ws_code` only when the SDK surfaces a proper protocol-level error object, a plain `message` otherwise. For a **client-role** adapter (Python is client-only, so this is the case that matters here), `fatal` MUST be the SDK's own disconnect-reason `fatal`, per CLIENT-SDK.md's fatal set — close `4010`/`4011` (only after the one refresh-retry has already failed), HTTP `403`/`404`, missing subprotocol echo, a token-provider failure, or reconnect attempts exhausted; it is **not** "the error code isn't `NO_ERROR`" — `4012`, `4013` and `4014` are all `fatal:false`. (A **server-role** adapter has no reconnect policy to be fatal *to*, so `fatal` there is looser and simply reflects whether the connection's own ws-mixer error code was anything other than `NO_ERROR`; a client-only adapter like Python's never emits that shape.) `close_reason` is present only when a reason was actually received from the peer — never the peer's echo of a close this side sent, and an SDK that stops reading right after an `error` message may legitimately never see one — and is never re-truncated or normalised; `phase` is always present (`"dial"` \| `"handshake"` \| `"connected"`). `drain` must include `message` when the underlying `DrainMsg` carries one, not just `reason`/`last_stream_id`/`deadline_ms`. `stream_closed` must carry `direction` (`read`\|`write`\|`both` — `both` is an extra event alongside whichever of `read`/`write` closed second, §1.2); `data` must fire only when bytes reach the application, since that is what credits the window; `reconnected` (same shape as `connected`) only if the adapter's SDK has its own reconnect loop and `connect.reconnect.enabled:true` was set — a client-only adapter with no reconnect support can leave it unimplemented and simply never emit it. |
| 6 | Honour `set_options` | `time_scale` multiplies every internal duration; `floor_ms` (default 300) is the floor every such scaled duration is clamped to instead of a local hardcoded constant; `allow_subfloor_timing` disables the `ping_interval ≥ 5000` and `ping_timeout ≥ 2×` checks from WIRE.md §2.10 step 2. |
| 7 | Smoke it | `cd conformance/runner && go run . --sdk python --mode pair --fixture happy_roundtrip -v` (or `go run . --adapter python=path/to/adapter` to point at a prebuilt binary) — Go server ↔ Python client, one scenario, full logs. |
| 8 | Run the matrix | `make conformance RUNNER_ARGS="--sdk python"`. Expect every `role:"client"` fixture and every pair scenario with Python as client. In pair mode, an `await.code` in a scenario step is matched against the event's `name` string field (e.g. `"STREAM_LIMIT"`), never its numeric `code` — `conformance/runner/driver/pair.go`'s `eventMatches` is the reference. |
| 9 | Wire CI | Add `python` to the workflow's SDK list and bump `conformance/COUNTS.json`. |

The adapter is the **entire** integration surface. No runner change, no fixture change, no schema change,
no change to Go or JS.

---

## 7. Open questions

1. **Should step-driving live in the fixtures or in a sidecar?** The runner has to know whether a `send RESET`
   step means "command the SDK to reset" or "watch for the SDK resetting on its own". Today's answer is a
   sidecar (`scenarios/step-driving.json`) so `spec/fixtures/` needs no edit. The alternative — an optional
   `"driver": "sdk"|"auto"` key on a sequence step — is cleaner to read and would let each SDK's own suite
   drop the same heuristic (Go's `sequence_test.go` re-derives it today, JS's does too). It is a spec fixture
   format change, so it needs a decision before the runner is written, not after.

2. **Does the JS `ConnectOptions` gain a documented test-only timing escape hatch, or does the JS adapter
   reach past `connect()` into `MixerConn`?** The scaled clock (§3.4) needs it, `_timing` already exists on
   `ConnOptions`, and it is `@internal`. Forwarding it through `ConnectOptions` is three lines but puts a
   test-only knob on the public dial API; reaching past `connect()` means the adapter tests a code path no
   user takes. Slight preference for forwarding it, marked `@internal` and stripped from the `.d.ts`.

3. **Does a `role:"client"` fixture that the Go client can also run count once or twice?** Running
   `max_streams_exceeded` against both the Go and JS clients is real coverage of two implementations; it also
   makes the pass-count floor jump every time an SDK gains a role. Leaning yes (count every cell), because
   the floor is exactly the thing that should notice a role disappearing.

4. **How much does the raw actor tolerate before it fails the test itself?** Resolved as-built: a `--strict`
   flag (default **true**) makes the raw actor fail immediately on the first observation that doesn't match
   what the current step is waiting for, rather than silently skipping past it — `--strict=false` restores
   the original, more lenient "tolerate any unmatched observation" behavior for debugging a flaky fixture
   (`conformance/README.md`, `conformance/runner/rawactor/rawactor.go`'s `SetStrict`).
