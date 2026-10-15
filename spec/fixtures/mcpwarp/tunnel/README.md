# Tunnel controller fixtures

Event sequences for the tunnel controller (`createTunnel()`, [MCPWARP.md §6](../../../../docs/MCPWARP.md)).
Each SDK runs them in its own CI against a fake edge and a fake clock. Shape:

| Key | Meaning |
|---|---|
| `description` | what the fixture pins, with the spec rule it comes from |
| `config` | the base `TunnelConfig`; the token is always `"t"` |
| `events` | ordered inputs, each an object with exactly one key (the event name) whose value is its payload |
| `expect` | every `TunnelStatus` snapshot the primary listener receives, in order, compared exactly (a key not listed MUST be absent) |
| `expectDials` | number of WebSocket dials (upgrade attempts) the SDK started during the whole fixture |

## Runner contract

1. Create the tunnel (any handler; `integration` a valid UUID), then subscribe the primary listener before the first
   event. Its immediate snapshot is `expect[0]`.
2. After each event, let the SDK run until it is quiescent: nothing runnable, no timer advanced. Only `tick` and
   `retry` move the clock. The runner does not wait for the promise a `configure`/`stop`/`dispose` call returns
   (a later event may be what lets it resolve); every such promise MUST have resolved by the end of the fixture.
3. `configure` payloads are merged over `config` (shallow); a field whose value is `null` is passed as absent
   (`undefined` / `nil` / `None`). The merge is always over `config`, never over an earlier `configure`.
4. Connection-level events address the most recent dial unless the payload has `conn` (1-based dial ordinal over
   the whole fixture). A service name in an edge message is always the name of the config that dialed that connection.
5. The fake edge answers `unregister` with `unregistered` and completes a close at once, except for a connection
   under `hold`.

## Events

| Event | Payload | Meaning |
|---|---|---|
| `configure` | partial `TunnelConfig` | call `configure(merged)` |
| `burst` | array of `configure` / `stop` events | make these calls back to back without yielding between them |
| `stop` | `{}` | call `stop()` |
| `dispose` | `{timeoutMs?}` | call `dispose({timeoutMs})` (`timeoutMs` absent → `dispose()`) |
| `subscribe` | `{}` | subscribe a second listener; the one snapshot it receives immediately is appended to `expect` at this point; then unsubscribe it |
| `connected` | `{conn?}` | the pending dial succeeds: upgrade done, `hello` sent |
| `dial_failed` | `{httpStatus?, conn?}` | the pending dial fails before an upgrade (network error, or a refused upgrade seen as close `1006`); a redial is scheduled unless `httpStatus` is an observable `401`/`403`/`404` (fatal). `httpStatus`: the upgrade was refused with this HTTP status and the SDK can observe it (MCPWARP.md §1 Upgrade-status mapping): `401` (static token) → `error`/`UNAUTHORIZED` (T32), `403`/`404` → `error`/`DISCONNECTED_FATAL` (T33), anything else a failed dial with a redial; absent = no observable status, a plain failed dial (T27–T29). A failure after the upgrade but before `welcome` is written as a `disconnect` on a connection with no `welcome`, and is a failed dial too (MCPWARP.md §6.7 Pre-`welcome` failure) |
| `welcome` | `{conn?}` | the edge sends `welcome`; the SDK sends `register` |
| `registered` | `{services: [{url, id, created}], conn?}` | the edge answers the outstanding `register` with `registered` for the agent's name |
| `register_error` | `{code, message?, id?, conn?}` | the edge answers the outstanding `register` with `registered.errors[]` = one entry for the agent's name |
| `app_error` | `{code, message?, conn?}` | the edge sends `{"op":"error"}` with this `AppErrorCode` |
| `disabled` | `{reason?, conn?}` | the edge sends `disable` for the agent's server id (`reason` absent → omitted on the wire, decoded as `disabled`) |
| `enabled` | `{conn?}` | the edge sends `enable` for the agent's name; the SDK sends `register` |
| `conflict` | `{reason?, conn?}` | the edge sends `conflict` for the agent's name and server id (`reason` default `taken_over`) |
| `disconnect` | `{fatal, code?, wsCode?, conn?}` | the connection's transport closes. `wsCode` is the WebSocket close code, `code` its [WIRE.md §2.8](../../../../docs/WIRE.md#28-error-codes) name; `fatal` says whether the ws-mixer reconnect rules ([WIRE.md](../../../../docs/WIRE.md)) give up (`true`) or schedule a reconnect (`false`). On a connection that had `connected` but no `welcome` it is a handshake-phase close (any `wsCode`) or the SDK's own welcome timeout (`code` `PROTOCOL_ERROR`, `wsCode` `4001`): a failed dial (T27–T29), not a lost connection |
| `drain` | `{reason?, conn?}` | the edge sends `drain` on the connection (`reason` default `rollout`); the SDK dials a replacement and keeps serving on the drained one until it ends. `reason` `replaced` is a take-over instead: handled exactly like `conflict` (no replacement dial) |
| `hold` | `{conn?}` | the fake edge keeps the connection open while the SDK closes it (as an in-flight request would): the edge never answers `unregister` and never closes it, until a `disconnect` addresses it; the SDK's own close budget (`unregister` wait ≤ 2 s, then drain grace ≤ 5 s) still runs, so fixtures follow with `tick {"ms": 7000}` |
| `retry` | `{delayMs, cause}` | advance the clock until the retry the SDK scheduled has fired, by at most `delayMs`. `cause`: `dial` (redial after `dial_failed` or a pre-`welcome` `disconnect`), `lost` (reconnect after a non-fatal `disconnect`), `register` (re-send after a transient `register_error`; `delayMs` is exact), `overloaded` (re-send after `app_error OVERLOADED`; exact), `drain` (replacement dial; jittered) |
| `tick` | `{ms}` | advance the clock by `ms` |

`lastError` values in `expect` are the SDK's constants (§6). None contains the config's name, url or token value
(the token `"t"` is a single letter, so check the token by equality, not by substring).

## Running the fixtures

Events are what the runner **injects**; snapshots, dials and `onRetry` calls are what it **observes**. A
fixture never injects an `onRetry` call or a fatality verdict that the SDK is supposed to produce itself.

### `onRetry`

The runner passes an `onRetry` in the tunnel options that records every call (`{attempt, delayMs, reason}`)
and does nothing else. No event calls it; the SDK does (MCPWARP.md §5.9, §6.8). For every `retry` event the
runner MUST have recorded, after the event that scheduled that retry and before it moves the clock, one call
that matches the event's `cause`:

| `cause` | The recorded call |
|---|---|
| `register` | `delayMs` equals the event's `delayMs`; `reason` starts with `register failed: ` |
| `overloaded` | `delayMs` `1000`; `reason` starts with `register failed: OVERLOADED: `; `attempt` `1` for the first re-send after the refusal, one more for each consecutive re-send, back to `1` after a successful `registered` |
| `drain` | `attempt` `0`, `reason` `"drain"`; `delayMs` (the delay actually waited: the drain jitter, `0–2000`, or the drain's `retry_after_ms` hint) ≤ the event's `delayMs` |
| `dial`, `lost` | `delayMs` ≤ the event's `delayMs`; `reason` (the transport's text) is not compared |

An `onRetry` call with no `retry` event after it is not a failure: a later `configure`, `stop`, take-over or
fatal error may cancel the retry it announced. `onRetry` calls never produce a snapshot of their own; status
moves only per T14, T15, T27, T28.

### `disconnect.fatal`

`fatal` is the verdict WIRE.md §2.9's reconnect rules give for that close; the fixture states it, the SDK must
reach it. Every `{fatal, code, wsCode}` in this corpus is what those rules give with the fixtures' static token
`"t"` (`1006`, `4012`, `4014` and a pre-`welcome` `4001` reconnect; `4010` and `4011` are fatal). A runner whose
fake sits below the ws-mixer client (a fake WebSocket) closes the connection with `wsCode`, preceded by
`error{code}` when `code` is given, and MUST observe that the SDK's disconnect reason has the same `fatal`. A
runner that fakes the ws-mixer client itself injects the disconnect reason `{fatal, wsCode, errorName: code}`
verbatim; it then tests the tunnel's reaction only, and the SDK's own reconnect tests must cover the verdict.

`dial_failed` with `httpStatus` works the same way: a fake WebSocket factory refuses the upgrade and reports
that status the way the runtime's factory contract allows (JS `WebSocketLike.upgradeFailure.status`), so the
status is observable; a runner that fakes the client injects the disconnect reason
`{phase: "dial", fatal, httpStatus}` with no `wsCode` and no `errorName` (CLIENT-SDK.md's Disconnect reason
shape row), `fatal` per WIRE.md §2.9 with the static token: `401`, `403`, `404` fatal, anything else not.
`dial_failed` without `httpStatus` is a refusal the SDK cannot see (or a network error): never fatal.

### Snapshot comparison

At the end of the fixture (after the last event and quiescence, every `configure`/`stop`/`dispose` promise
resolved), the list of snapshots the primary listener received, plus the one each `subscribe` event appended at
its position, MUST equal `expect` exactly:

1. same length, in order — no extra snapshot at the end, none missing;
2. each snapshot has exactly the keys of its `expect` entry: a key holding `undefined`, `null`, `nil` or `None`
   is present and therefore a mismatch, not "absent" (MCPWARP.md §6.4);
3. every value is compared as an exact string — no trimming, case folding or Unicode normalisation.

The `status` getter is not compared, only listener deliveries. When `expectDials` is present, the number of
WebSocket factory calls (upgrade attempts) over the whole fixture MUST equal it.
