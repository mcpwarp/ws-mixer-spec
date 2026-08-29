# ws-mixer conformance runner

Implements `docs/CONFORMANCE.md`. Read that document first — this file covers how to run things,
the decisions taken on its open questions, and the gaps this implementation actually has (rather
than claiming a perfect realization of the design doc under a tight timeline).

## Running it

```sh
# Everything: builds the runner + every adapter it can find, runs the full matrix.
make conformance GO=$HOME/.goenv/versions/1.24.4/bin/go

# One SDK, with a JUnit report (path is resolved against the repo root):
make conformance GO=... RUNNER_ARGS="--sdk go --report build/conformance.xml"

# Direct invocation, equivalent to the above:
cd conformance/runner && GO=$HOME/.goenv/versions/1.24.4/bin/go go run . --sdk go,js

# Narrow to one fixture/scenario while iterating:
go run . --sdk go --mode fixture --fixture credit_violation -v
```

`GO` is expanded by the runner itself (a leading `~`/`~/` and any `$HOME` reference), not just by
the shell: `os/exec` never invokes a shell, so a literal, unexpanded `~` reaching the runner's own
environment (e.g. via a quoted `GO="~/.goenv/..."` in a script or CI config, which bash does not
tilde-expand) would otherwise try to `fork/exec` a path that doesn't exist. If the Go adapter still
can't be built after that expansion (bad path, broken toolchain, compile error), the runner exits
non-zero with the build error printed to stderr — it never silently downgrades every `go-*` key to
SKIP, since Go is the reference/only-server implementation and a build failure there is a
config/toolchain problem, not a "this SDK isn't present" gap.

Flags: `--time-scale` (default `1/50` — see "Timing floor" below), `--sdk <comma-separated names>`
(defaults to every adapter directory discovered under `conformance/adapters/*/`, currently `go,js`),
`--fixture <glob>`, `--mode fixture|pair|all` (default `all`), `--report <path>` (JUnit XML),
`--adapter name=path` (override discovery for one SDK, or add one with no directory of its own,
e.g. `--sdk python --adapter python=/path/to/adapter`), `--strict` (bool, **default true**: the raw
actor fails on any observed frame the current step doesn't expect instead of silently skipping it
— docs/CONFORMANCE.md section 2), `-v` (print failure detail immediately).

Gates this satisfies: `make check-spec` (Node-only); `go test -race ./...` inside
`conformance/runner` (codec round-trip, adapter protocol + fake-adapter driver tests, an
end-to-end run of 4 fixtures + 2 pair cells against the real adapters); `go test -race ./...` in
`go/`, both with and without `-tags conformance` — green; `npm test` in `js/` — green. Neither
`go test` in `go/` nor `npm test` in `js/` depends on the runner.

## The matrix, as actually implemented

| Mode | Go plays | JS plays | Count |
|---|---|---|---|
| fixture | server (19 fixtures) + client (24 fixtures) | client (24 fixtures) | 43 (SDK, fixture) cells |
| pair | server, client | client only | 8 scenarios × {go→go, go→js} = 16 cells |

All 43 `spec/fixtures/sequences/*.json` fixtures run against Go (19 `role:"server"` + 24
`role:"client"`); the 24 `role:"client"` fixtures also run against JS (the JS SDK has no server), reported as `js-server`
SKIPs for the 19 it can't play. All 8 pair
scenarios run go→go and go→js (`drain_reconnect`'s go→go cell is a SKIP, not a FAIL or a PASS —
see "SDK changes made" / `adapters/go/main.go`'s reconnect-unsupported reply). No orphan processes
are left behind after a run (`pgrep -f 'adapter|runner|goserver'` empty). `conformance/COUNTS.json`
is the checked-in floor on passing cells (docs/CONFORMANCE.md section 5) — it, not this paragraph,
is the source of truth for the exact current pass count.

Pair scenarios (`conformance/scenarios/*.json`, excluding the `step-driving.json` sidecar):
`happy_roundtrip`, `n_streams_fanout` (3 streams, open/write/half-close/respond/half-close),
`app_roundtrip`, `half_close_sse`, `drain_with_inflight`, `big_stream_flow_control` (1 MiB single
write, well over the default 262144-byte window — the ack only returns once several full
credit-refill rounds have happened, which is the flow-control proof), `graceful_close`,
`drain_reconnect` (server drains an in-flight stream; the JS client -- the only scenario that runs
with `connect.reconnect.enabled:true` -- sees its connection close with GOING_AWAY/4012 and its own
`MixerClient` reconnect loop redial automatically, surfaced as a `reconnected` event; a fresh stream
opened after that round-trips normally, proving the new connection is fully live rather than just
re-established).

## Decisions on `docs/CONFORMANCE.md` section 7's open questions

1. **Step-driving sidecar vs. fixture field.** Implemented exactly as the doc's stated default:
   `conformance/scenarios/step-driving.json`, keyed by `{fixture, step_index}` → `"auto"`. Only one
   entry is needed (`drain_with_inflight_timeout` step 3, the deadline-driven RESET) — every other
   autonomous RESET in the other 42 fixtures is detected structurally by this runner from the RESET's own
   `code` (`STREAM_LIMIT`/`STREAM_CLOSED` ⇒ always autonomous; anything else ⇒ a `reset` command
   unless overridden). `spec/fixtures/` is untouched.
2. **`ConnectOptions._timing`.** Forwarded, as the doc's stated preference. See "SDK changes" below.
3. **Counting a `role:"client"` fixture once or twice.** Implemented as "count every cell": both
   `max_streams_exceeded` and `frame_before_welcome` run (and are reported) against both Go and JS
   independently.
4. **Raw actor tolerance for an unmatched frame.** `--strict` (default **true**) makes
   `rawactor.Actor.Expect` fail fast on the first observation that doesn't match what the current
   step is waiting for, rather than silently skipping it and relying on the per-step timeout to
   catch a genuinely wrong transcript. Passing `--strict=false` restores the original, more lenient
   behavior (tolerate any unmatched observation) for debugging a flaky fixture against real
   scheduling noise.

## Other implementation decisions (not from section 7, but real ones)

- **§4's dual-authority expect mapping, as built** (`conformance/runner/driver/expect.go`):
  `error_code` is asserted from the **raw actor's own observed `error{}` control frame** (numeric
  `code` + `name`) as the primary authority — this is what fires even for a handshake failure the
  adapter's `OnConn`/`onConnect` never saw (`auth_failure`, `hello_timeout`, `frame_before_hello`,
  `app_before_welcome`, ...), since `go/wsmixer`'s `Listener.ServeHTTP` only calls `OnConn` (where
  the adapter's `disconnected` watcher gets attached) *after* the handshake succeeds. `close_code`
  is then checked *against* that `error{}` frame's code as an invariant (`close_code == 4000 +
  error_code`, `1000` for `NO_ERROR`, per OVERVIEW.md section 2.8), and the adapter's own
  `disconnected.error_name` is checked only as a best-effort secondary cross-check when the adapter
  happens to have reported one by then. The second standing invariant from docs/CONFORMANCE.md
  section 4 (`error` is the last stream-0 message before the close) is also checked live here
  (`errAt.After(closeObs.At)` is a failure), on top of the static check `spec/tools/check-fixtures.mjs`
  already does against the fixture JSON itself.
- **`connect` is fire-and-forget for `role:"client"` fixtures**, not `SendAndAck`'d. Two of the two
  `role:"client"` fixtures (`frame_before_welcome`, `max_streams_exceeded`) never send a welcome at
  all — the underlying `Dial()`/`connect()` call doesn't resolve *or* reject until well after the
  very steps that determine its outcome (the raw actor sending the illegal frame) have run, so
  blocking on its ack here would deadlock the replay against its own precondition.
- **Pre-replay hello sync point.** For `role:"client"` fixtures, after firing `connect`, the runner
  silently waits for the client's own `hello` to be observed before starting the scripted replay.
  This isn't asserted as fixture content — it's a synchronization point proving the client's
  dispatch pipeline is live before pushing `welcome`/`OPEN`/etc. at it. Without it, `welcome`+`OPEN`
  sent back-to-back immediately after WS accept occasionally raced the client's own post-`open`
  setup and surfaced as a flaky, spurious `"OPEN frame received before welcome completed the
  handshake"` — full duplex TCP does not guarantee anything about *when* one direction's bytes are
  application-processed relative to the other direction's send, only about intra-direction
  ordering.
- **Timing floor raised from 20ms to 300ms** (`conformance/runner/driver/timing.go`), and the
  default `--time-scale` from the doc's suggested `1/250` to `1/50`. Found the hard way: a
  20-40ms `hello_timeout_ms` raced real OS process/goroutine scheduling across two separate
  processes (the runner's own goroutine has to notice a channel signal and call `Write` before the
  adapter's own timer fires) enough to flake intermittently — and because the JS SDK's `connect()`
  is `MixerClient` (a public API with its own reconnect loop, which is what
  `docs/CONFORMANCE.md`'s open question 2 obligates the adapter to use), a single spurious timeout
  triggered an **infinite background retry storm** against a raw actor that only ever accepts one
  connection (`docs/CONFORMANCE.md` section 1: "one adapter process = one connection"), turning a
  rare timing hiccup into a silent test hang rather than a fast, visible failure. Fixed three ways:
  the JS adapter's `connect()` call defaults to `reconnect: { maxAttempts: 0 }` (an adapter has no
  business auto-reconnecting mid-fixture by default — reconnect policy is explicitly out of the
  adapter protocol's scope per section 1.3 unless a scenario opts in, which only `drain_reconnect`
  does, via `connect.reconnect.enabled:true`); the floor/scale were both raised to give real IPC
  headroom; and the 300ms floor itself is no longer hardcoded independently inside each adapter —
  the runner sends it as `set_options.floor_ms` and both adapters clamp their own scaled durations
  to that value (falling back to 300 only if a run never sends one), so raising the floor again
  later is a one-line change in the runner instead of an edit to every adapter.
- **Keepalive is only scaled where it's the point.** `ping_interval_ms`/`ping_timeout_ms` are sent
  un-scaled (30000/90000, the wire defaults) to every fixture and adapter *except*
  `ping_pong_then_dead_peer_timeout` (which needs to actually observe the watchdog fire) and
  `pong_for_unsent_id` (whose transcript waits on an autonomous `ping` the SDK only emits every
  `ping_interval`). The raw actor never auto-pongs by design (section 2: "no autonomous
  behaviour"), so scaling the keepalive down globally made an unrelated fixture's own
  process/IPC overhead alone enough to spuriously trip `KEEPALIVE_TIMEOUT` mid-test. Similarly,
  pair mode never scales `ping_interval_ms`/`ping_timeout_ms` at all: no pair scenario here waits
  out a real keepalive timeout, and scaling it down surfaced a *second*, deeper issue (next point).
- **`AllowSubfloorTiming` now actually reaches the real Go client (fixed).** `go/wsmixer`'s
  `welcome.ping_interval >= 5000ms` / `ping_timeout >= 2×ping_interval` floor (OVERVIEW.md section
  2.9/2.10) used to be checked in **two** places: `Conn.applyWelcome` (client.go, semantic
  validation, gated on the timing hook) *and* `control_messages.go`'s `parseWelcome` (the
  stateless, context-free wire parser shared by every caller of `ParseControl`, checked
  unconditionally) — so the gate on the first check was dead code; a sub-floor welcome was rejected
  by the second check regardless. Fixed by removing the floor check from `parseWelcome` entirely
  (it now just decodes `ping_interval`/`ping_timeout` as plain ints) and keeping it only in
  `applyWelcome`, the one place with the connection-scoped context to know whether this run is
  allowed to bypass it. The hook itself changed shape: `Options.AllowSubfloorTiming` (an exported,
  always-compiled-in field) is gone, replaced by an unexported `Options.allowSubfloorTiming` field
  settable only through `wsmixer.AllowSubfloorTiming(*Options)` in
  `go/wsmixer/conformance_hooks.go`, which carries `//go:build conformance` — so a normal `go
  build`/`go vet`/`go test ./...` of `go/wsmixer` never even compiles the hook in, let alone lets
  production code call it. The Go adapter (`conformance/adapters/go/main.go`) calls it when
  `set_options.allow_subfloor_timing:true`, and is therefore now built with `-tags conformance`
  (`conformance/runner/build.go`'s `buildGoAdapter`) — without that tag the adapter fails to
  compile, since `wsmixer.AllowSubfloorTiming` doesn't exist. `go test -race ./...` in `go/` is
  green both with and without `-tags conformance`.
- **`stream_state`/`stream_reset_code` for a stream the SDK never made app-visible.**
  `max_streams_exceeded`'s client-role fixture RESETs a stream the SDK refused before ever
  constructing an app-level `Stream` (`go/wsmixer/dispatch.go`'s `handleRemoteOpen` sends `RESET`
  directly on an over-limit `OPEN`, with no `OnStream` callback and no metric for the refused case)
  — so there is no adapter event to derive `stream_state`/`stream_reset_code` from, for either SDK.
  This runner falls back to its own wire-level confirmation (the raw actor observing the RESET
  frame, already required to drive the step in the first place) as ground truth for exactly this
  case, rather than adding a new SDK hook. Documented here because it's a deliberate exception to
  "state is per-SDK bookkeeping, derived from adapter events" (docs/CONFORMANCE.md section 4).
- **`connected.welcome` is best-effort on the Go side.** `go/wsmixer`'s `Conn` exposes `Session()`
  and `Meta()` but no accessor for the negotiated window/max_streams/ping_interval/ping_timeout, so
  the Go adapter's `connected` event reports `{"session": "..."}` rather than the full verbatim
  welcome object the event table describes. The JS adapter *can* report the full object (its
  `onConnect` callback receives the real `WelcomeMsg`). No fixture or scenario here asserts on
  `connected.welcome`'s contents, so this is a latent gap rather than an active one.
- **Pair-mode wire concurrency is not exercised.** The pair DSL (`conformance/scenarios/*.json`) is
  a flat, strictly-ordered step list; `n_streams_fanout` opens/writes/closes three streams but does
  so as three sequential command+await round trips per phase, not truly concurrent commands. The
  driver's event matching (`adapter.Adapter.WaitFor`, `rawactor.Actor.Expect`) *is* safe against
  out-of-script-order event arrival (each match consumes the first not-yet-consumed matching event
  by content, not a monotonic position — needed for exactly this scenario, since a real SDK's
  per-stream goroutines/callbacks don't guarantee cross-stream emission order even when the
  underlying frames arrived in wire order), so a genuinely concurrent DSL extension would mostly
  work against today's driver; there just isn't one yet.
- **JS adapter imports built `dist/`, not `src/*.ts` directly** — a deviation from
  docs/CONFORMANCE.md section 5's "no build step; imports ../../../js/src". A plain `node`
  process has no loader to resolve a bare `.ts` import. `conformance/runner/build.go` runs `npm
  run build` in `js/` once per runner invocation before spawning the JS adapter, so `dist/` is
  never stale relative to `src/` (this mattered concretely: the required `_timing` forwarding
  change to `client.ts` needs a fresh build to take effect).
- **JS `drain` command maps to `MixerClient.close()`.** The public JS API has no standalone "send
  `drain{client_requested}`, stay connected" primitive — `close()` already does exactly that
  followed by a graceful shutdown. No scenario here needs the client to remain connected after
  requesting drain, so this is an adequate stand-in, not a full implementation of the command.
- **`auth_failure`'s mismatched token is a runner-side special case**, not something the fixture
  format encodes: the raw actor deliberately dials with an `Authorization` header that does not
  match the `hello.token` it's about to send, because `go/wsmixer`'s built-in
  `performServerHandshake` rejects that mismatch before any `Authenticate` hook even runs — for
  every other fixture, header and `hello.token` match.

## SDK changes made

1. **`js/src/client.ts`** — `ConnectOptions` gained an `@internal` `_timing` field
   (`{minPingInterval?, minPingTimeout?, helloTimeout?}`), forwarded verbatim into the `MixerConn`
   constructor's `ConnOptions._timing`. Three lines plus the field's doc comment. This is the one
   change `docs/CONFORMANCE.md` section 3.4 anticipated as required.
2. **`go/wsmixer/conn.go` / `client.go` / `control_messages.go` / `conformance_hooks.go`** —
   `Options` gained an unexported `allowSubfloorTiming bool` (test-only), which `Conn.applyWelcome`
   (client.go) checks before enforcing the `ping_interval`/`ping_timeout` wire floors; the same
   check was removed from `control_messages.go`'s `parseWelcome` (previously unconditional, making
   the hook dead code — see "AllowSubfloorTiming now actually reaches the real Go client" above).
   The field is settable only via `wsmixer.AllowSubfloorTiming(*Options)` in the new
   `go/wsmixer/conformance_hooks.go`, gated behind `//go:build conformance` so it never reaches a
   normal build. This supersedes the originally-anticipated exported `Options.AllowSubfloorTiming`
   field with a build-tag-gated hook, closing the gap the first version left open.

`go test -race ./...` in `go/` is green both with and without `-tags conformance`; `npm test` in
`js/` is green.

## Layout

```
conformance/
  README.md              this file
  runner/                Go module (own go.mod; does not import go/wsmixer)
    main.go, build.go     CLI flags, adapter discovery/build, matrix expansion
    wire/                 independent frame codec (docs/CONFORMANCE.md section 2)
    rawactor/              the raw wire actor, both roles
    adapter/               spawn, JSON-lines codec, process teardown
    fixture/               spec/fixtures/sequences/*.json loader
    driver/                fixture + pair driving logic, the section 4 expect mapping
    codes/                 the OVERVIEW.md section 2.8 error code table (independent copy)
    report/                stdout table + JUnit XML
    e2e_test.go            go test entrypoint (docs/CONFORMANCE.md section 5's TestMain hook)
  adapters/
    go/     go.mod, main.go   replace => ../../../go; built with `-tags conformance` (build.go),
                               required for wsmixer.AllowSubfloorTiming to exist
    js/     adapter.mjs        imports ../../../js/dist (see deviation above)
  scenarios/
    *.json                 pair-mode scenarios
    step-driving.json      the section 3.3 sidecar
```

## Using the Go adapter as a fake tunnel for external e2e tests

The Go adapter (`conformance/adapters/go/main.go`) is a thin shell over `go/wsmixer` with no
protocol logic of its own (docs/CONFORMANCE.md section 1). Because it just relays a real
`go/wsmixer` server over JSON-lines commands/events on stdin/stdout, it's also useful standalone,
outside the runner, as a scriptable fake tunnel endpoint for external end-to-end tests that want a
byte-exact request/response over one stream without standing up a full ws-mixer deployment.

Build it (the `conformance` build tag is required — it's what makes `wsmixer.AllowSubfloorTiming`
available; see `main.go`'s package comment):

```sh
$HOME/.goenv/versions/1.24.4/bin/go build -tags conformance -o /tmp/go-adapter ./conformance/adapters/go
```

Spawn it and drive it by writing one JSON object per line to its stdin and reading one JSON object
per line back from its stdout. Sequence to send a byte-exact request on one stream and collect the
response chunks:

1. `listen` → wait for `listening`
```json
{"cmd":"listen","seq":1}
```
```json
{"event":"ack","seq":1}
```
```json
{"event":"listening","seq":1,"url":"ws://127.0.0.1:54321/v1/tunnel"}
```
2. (an external client connects to that `url`) → `connected`
```json
{"event":"connected","session":"...","welcome":{"session":"..."}}
```
3. `open_stream`
```json
{"cmd":"open_stream","seq":2}
```
```json
{"event":"ack","seq":2}
```
```json
{"event":"stream_opened","seq":2,"id":1}
```
4. `write` the request bytes (base64), then `close_write` to signal the request is complete
```json
{"cmd":"write","seq":3,"id":1,"data_b64":"cmVxdWVzdCBib2R5"}
```
```json
{"event":"ack","seq":3}
```
```json
{"cmd":"close_write","seq":4,"id":1}
```
```json
{"event":"ack","seq":4}
```
```json
{"event":"stream_closed","id":1,"direction":"write","t_ms":12}
```
5. Observe `data` events until `stream_closed{direction:"read"}` (or `"both"`, if this side's
   write side was already closed) — concatenate `data_b64` in arrival order for the byte-exact
   response
```json
{"event":"data","id":1,"data_b64":"cmVzcG9uc2UgY2h1bms=","t_ms":47}
```
```json
{"event":"stream_closed","id":1,"direction":"read","t_ms":48}
```
```json
{"event":"stream_closed","id":1,"direction":"both","t_ms":48}
```
6. Shut down cleanly: `drain` (optional, only if you want to stop accepting new streams first)
   then `close`
```json
{"cmd":"drain","seq":5,"reason":"client_requested"}
```
```json
{"event":"ack","seq":5}
```
```json
{"cmd":"close","seq":6,"code":0,"message":""}
```
```json
{"event":"ack","seq":6}
```

If a `stream_reset` arrives instead of a clean `stream_closed`, treat the request as failed —
`code`/`name`/`message` identify why (docs/CONFORMANCE.md section 1.2).

**Stability promise:** this is the same protocol the runner speaks, versioned in
`docs/CONFORMANCE.md` (section 1.1's command table, section 1.2's event table). Changes to it are
additive only — new optional fields (like `t_ms`, added for this use case) or new event/command
kinds, never a renamed or removed field an external consumer might already depend on. Anything
already documented there is safe to build against.

## Adding a Python adapter

Follow `docs/CONFORMANCE.md` section 6's checklist verbatim — nothing here changes it. Two
concrete pointers from having actually built the other two adapters:

- Emit `disconnected` from your own equivalent of `go func() { <-conn.Done(); emit(...) }()` /
  `onDisconnect`, wired up **as soon as** a connection object exists — for a server-role adapter
  (not applicable to a client-only Python adapter, but worth knowing for later), a handshake
  failure never reaches an app-level "connection" callback at all in either reference SDK, so
  `disconnected` for that case has to come from watching the raw socket/transport layer directly,
  not from an SDK-level connection hook.
- Keep `ping_interval_ms`/`ping_timeout_ms` at realistic values unless a fixture specifically
  needs them scaled (see "Keepalive is only scaled where it's the point" above) — don't naively
  wire `time_scale` into every duration your SDK owns.
- Smoke it exactly as section 6 step 7 says: `./conformance/runner --sdk python --mode pair
  --fixture happy_roundtrip -v` (once `happy_roundtrip` — or an equivalent — exists as a pair
  scenario, which it does here) before trying the full fixture matrix.
- Stamp `t_ms` (integer, milliseconds since your adapter process started, monotonic) on every
  `data`, `stream_closed`, and `stream_reset` event, matching the Go and JS adapters
  (`docs/CONFORMANCE.md` section 1.2). It's additive — the runner ignores unknown fields — but
  keep it for consistency across SDKs.
- `conformance/COUNTS.json` (docs/CONFORMANCE.md section 5's CI pass-count floor) exists and is
  enforced on every unfiltered run; bump its `js-client`/`go-client`/etc. floors once Python starts
  passing cells, per its own `checkCounts` doc comment in `main.go`.

## Gaps

Being direct about what's incomplete, beyond what's already called out inline above:

- **No `.github/workflows/conformance.yml` yet.** `conformance/COUNTS.json` (the pass-count floor,
  docs/CONFORMANCE.md section 5) exists and is enforced by `main.go`'s `checkCounts` on every
  unfiltered run; wiring an actual CI workflow around `make conformance` is still open.
- **Python adapter**: not started (out of scope here; see the checklist above).
