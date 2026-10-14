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
| `dial_failed` | `{conn?}` | the pending dial fails before an upgrade (network error, or a refused upgrade seen as close `1006`); the SDK schedules a redial |
| `welcome` | `{conn?}` | the edge sends `welcome`; the SDK sends `register` |
| `registered` | `{services: [{url, id, created}], conn?}` | the edge answers the outstanding `register` with `registered` for the agent's name |
| `register_error` | `{code, message?, id?, conn?}` | the edge answers the outstanding `register` with `registered.errors[]` = one entry for the agent's name |
| `app_error` | `{code, message?, conn?}` | the edge sends `{"op":"error"}` with this `AppErrorCode` |
| `disabled` | `{reason?, conn?}` | the edge sends `disable` for the agent's server id (`reason` absent → omitted on the wire, decoded as `disabled`) |
| `enabled` | `{conn?}` | the edge sends `enable` for the agent's name; the SDK sends `register` |
| `conflict` | `{reason?, conn?}` | the edge sends `conflict` for the agent's name and server id (`reason` default `taken_over`) |
| `disconnect` | `{fatal, code?, wsCode?, conn?}` | the connection's transport closes. `wsCode` is the WebSocket close code, `code` its [WIRE.md §2.8](../../../../docs/WIRE.md#28-error-codes) name; `fatal` says whether the ws-mixer reconnect rules ([WIRE.md](../../../../docs/WIRE.md)) give up (`true`) or schedule a reconnect (`false`) |
| `drain` | `{reason?, conn?}` | the edge sends `drain` on the connection (`reason` default `rollout`); the SDK dials a replacement and keeps serving on the drained one until it ends. `reason` `replaced` is a take-over instead: handled exactly like `conflict` (no replacement dial) |
| `hold` | `{conn?}` | the fake edge keeps the connection open while the SDK closes it (as an in-flight request would): the edge never answers `unregister` and never closes it, until a `disconnect` addresses it; the SDK's own close budget (`unregister` wait ≤ 2 s, then drain grace ≤ 5 s) still runs, so fixtures follow with `tick {"ms": 7000}` |
| `retry` | `{delayMs, cause}` | advance the clock until the retry the SDK scheduled has fired, by at most `delayMs`. `cause`: `dial` (redial after `dial_failed`), `lost` (reconnect after a non-fatal `disconnect`), `register` (re-send after a transient `register_error`; `delayMs` is exact), `overloaded` (re-send after `app_error OVERLOADED`; exact), `drain` (replacement dial; jittered) |
| `tick` | `{ms}` | advance the clock by `ms` |

`lastError` values in `expect` are the SDK's constants (§6). None contains the config's name, url or token value
(the token `"t"` is a single letter, so check the token by equality, not by substring).
