# ws-mixer-spec

The `ws-mixer.v1` wire specification: normative frame and control-channel spec, client-SDK
requirements, the protocol decision log, a conformance fixture corpus, and the cross-SDK
conformance runner that validates every implementation against the same fixtures.

This repo contains no implementation — no server, no client, no adapters. It is the contract every
`ws-mixer.v1` implementation is validated against.

## Layout

```
docs/
  OVERVIEW.md     start here — what ws-mixer is, and pointers to everything below
  WIRE.md         normative wire spec: framing, streams, flow control, control channel, error codes
  CLIENT-SDK.md   normative, language-independent client SDK requirements
  DECISIONS.md    the protocol decision log, with stable ids
  CONFORMANCE.md  how the cross-SDK conformance runner works
  REPOS.md        the cross-repo map and the spec-pin policy other repos use
  research/       design research passes (evidence and prior art, not normative)
spec/
  control.schema.json, control.relaxed.schema.json   JSON Schema for control messages
  fixtures/       the conformance fixture corpus (frames, control messages, connection sequences)
  tools/          check-fixtures.mjs, gen-relaxed-schema.mjs
conformance/
  runner/         the cross-SDK conformance runner (Go, package of this repo's root module)
  scenarios/      pair-mode scenarios (two SDK adapters driven against each other)
```

## Running the fixture checker

```sh
node spec/tools/check-fixtures.mjs
```

Validates every fixture in `spec/fixtures/` against both the strict and relaxed JSON Schemas and
checks the fixture count against `spec/fixtures/COUNTS.json`'s floor.

## Running the conformance runner

This repo ships no SDK adapters — each SDK builds its own (see `docs/CONFORMANCE.md` §5) and points
the runner at it:

```sh
go run ./conformance/runner \
  --spec-root . \
  --adapter go=/path/to/go-adapter/run \
  --sdk go --mode fixture
```

`--spec-root`, `--adapters-dir`, `--counts` and `--adapter` all have sane defaults for running from
inside this repo; see `docs/CONFORMANCE.md` and `conformance/README.md` for the full flag reference
and CI wiring.

## How SDKs pin this repo

Every implementation repo carries a one-line `spec.pin` file naming the `ws-mixer-spec` tag its own
tests and conformance runs are validated against (e.g. `ws-mixer-go` also carries a `goserver.pin`
consumed by `ws-mixer-js`). Bumping it is a deliberate PR in the consumer repo. See
[`docs/REPOS.md`](docs/REPOS.md) for the full pin/tag policy.

## Where each SDK lives

| Repo | What it is |
|---|---|
| `ws-mixer-go` | Go implementation: protocol core + Go client + conformance adapter |
| `ws-mixer-js` | `@mcpwarp/ws-mixer` — the JS/TypeScript client SDK + conformance adapter |
| `ws-mixer-server` | private: the HTTP/upgrade/auth server layer built on `ws-mixer-go` |
| `ws-mixer-python` | placeholder for a future Python client SDK |

See [`docs/REPOS.md`](docs/REPOS.md) for the full map, visibility, and license per repo.
