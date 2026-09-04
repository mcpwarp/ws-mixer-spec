# Repository map

`ws-mixer.v1` is split across five repositories, all under `github.com/mcpwarp/`:

| Repo | Visibility | License | Contents |
|---|---|---|---|
| [`ws-mixer-spec`](https://github.com/mcpwarp/ws-mixer-spec) (this repo) | public | Apache-2.0 | wire spec, client-SDK requirements, decision log, fixtures + schema + checker, the cross-SDK conformance runner + scenarios |
| `ws-mixer-go` | public | Apache-2.0 | Go module `github.com/mcpwarp/ws-mixer-go` — protocol core + Go client + its conformance adapter |
| `ws-mixer-js` | public | Apache-2.0 | npm `@mcpwarp/ws-mixer` + its conformance adapter |
| `ws-mixer-server` | private (AGPL-3.0 if opened) | AGPL-3.0 | Go module `github.com/mcpwarp/ws-mixer-server` — HTTP/upgrade layer, auth policy, server ops |
| `ws-mixer-python` | — | — | placeholder for a Python client SDK, out of scope today |

This repo owns nothing implementation-specific: no server, no client, no adapters. It owns the contract
every implementation is validated against.

## Where things live

| Topic | Lives in |
|---|---|
| Wire framing, streams, flow control, control channel, error codes, sequences (normative) | [`WIRE.md`](./WIRE.md) |
| Client SDK behavioural requirements, all languages (normative) | [`CLIENT-SDK.md`](./CLIENT-SDK.md) |
| Protocol decision log | [`DECISIONS.md`](./DECISIONS.md) |
| Conformance fixtures, schema, checker | [`../spec/`](../spec/) |
| Cross-SDK conformance runner + scenarios | [`../conformance/`](../conformance/) — see [`CONFORMANCE.md`](./CONFORMANCE.md) |
| Go implementation notes, types, usage | `ws-mixer-go` repo's own `README.md` / `docs/` |
| Go server-only HTTP/upgrade/auth layer, deployment | `ws-mixer-server` repo's own `docs/USAGE.md` / `docs/OBSERVABILITY.md` |
| JS implementation notes, types, usage | `ws-mixer-js` repo's own `docs/DESIGN.md` |
| Each repo's own implementation decisions | that repo's own `docs/DECISIONS.md`, linking back to [`DECISIONS.md`](./DECISIONS.md) for anything protocol-level |

## Pin / tag policy

- **Spec tags are the pin currency.** Every non-spec repo carries a one-line `spec.pin` file (e.g. `v0.3.0`)
  naming the `ws-mixer-spec` tag its own tests and conformance runs are validated against. CI reads it to
  fetch the matching spec checkout. Bumping it is a deliberate PR in the consumer repo — never automatic.
- **All five repos started at `v0.3.0`**, cut the same day, as one coherent, mutually-tested set. After that
  each repo's tags float independently; a consumer only moves when it bumps its `spec.pin` (or, for
  `ws-mixer-go`, `goserver.pin` in `ws-mixer-js`) on purpose.
- **Go module versions stay in `v0`** for now — no `/vN` suffix needed until a `v2`.
- **The conformance runner is a package of this repo's root module**, at `./conformance/runner` — not a
  nested module. A plain repo tag (`v0.3.0`) is therefore enough to pin the runner too; a consumer runs it
  with `go run github.com/mcpwarp/ws-mixer-spec/conformance/runner@<tag>` from a fetched checkout, or a local
  clone via `go run ./conformance/runner --spec-root <path>` (see [`CONFORMANCE.md`](./CONFORMANCE.md)).
- **The pre-split monorepo** (`mcpwarp/ws-mixer`) is archived read-only as `mcpwarp/ws-mixer-archive`; none
  of its tags carry forward into these five repos, since a tag like `v0.2.0` on the old module path would
  resolve to a tree with the wrong module path if copied verbatim. It remains the place to find pre-split
  blame and history for anything moved with a `git filter-repo --path-rename` (each successor repo's own
  README notes the archive URL and the pre-split commit it starts from, where relevant).

## Codegen (not built yet)

A future generator will live in `spec/codegen/` in this repo, read `spec/control.schema.json` and the frame
tables in [`WIRE.md`](./WIRE.md), and emit generated frame/control code into `ws-mixer-go` and
`ws-mixer-js`. Nothing here blocks it — the wire codec and control-message files in each implementation are
hand-written today, in one package/module each, with zero server logic mixed in, specifically so a generator
can replace them wholesale later. Not designed further here; noted so the door stays open.
