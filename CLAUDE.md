# CLAUDE.md

Guidance for Claude Code working in this repo. Read `README.md` first for the pitch and status.

**pocket-ap** ("Pocket Access Point") is a self-hosted, drop-in RPC proxy for Pocket Network's Shannon protocol. Stake an app, run the binary, point your `RPC_URL` at a local listener — traffic is ring-signed with your app key and relayed straight to suppliers. **No gateway middleman.** Target user: app developers (JS/TS backends) that PATH/SAGE don't serve.

It was extracted (lifted, near-verbatim) from the sibling **SAGE** repo (`../sage`, package `protocol/shannon`). SAGE is the source of truth for the crypto/session/relay mechanics; keep the lifted code in sync with it.

## What this is / isn't

- **IS:** a transparent passthrough relay client. The RPC payload is **opaque bytes** — no per-chain parsing.
- **ISN'T:** a full gateway. QoS, heuristics, reputation, hedging, circuit breaking, observation pipeline all live in SAGE and are intentionally **absent here**. Do not add them. If a task wants gateway smarts, it belongs in SAGE, not pocket-ap.

## Commands

- `make build` — binary to `bin/pocket-ap` (CGO off)
- `make run CONFIG_PATH=local/config.yaml`
- `./bin/pocket-ap call --config local/config.yaml -d '{...}' -v` — one-shot relay + diagnostics (see below)
- `make test` / `make vet` / `make lint` / `make tidy`
- Single test: `go test ./pocket/ -run TestFoo -race -count=1 -v`

**Build note:** the cosmos-sdk / cometbft / go-ethereum dep tree is heavy. First cold build ~40s, cached after. All deps are in the shared module cache (SAGE uses the same versions), so **offline builds work**: prefix with `GOPROXY=off GOSUMDB=off`.

## Architecture

Four seams, defined as interfaces in `relay/relay.go` (consumer side), with dumb defaults. Extend at the seams — don't modify the core.

| Seam | Default impl | Role |
| --- | --- | --- |
| SessionSource | `pocket.SessionManager` | fetch/cache/rotate sessions + block poller |
| Signer | `pocket.Signer` | build + ring-sign relays |
| Selector | `selector.Random` | pick supplier endpoints (filtered by RPC type) |
| Transport | `transport.HTTP` / `transport.WS` | the front listeners |
| (FrameSigner / FrameValidator) | `pocket.Signer` / `pocket.Validator` | **WS only**: sign/verify RAW frames — no HTTP envelope, see roadmap 1 |
| (Observer) | *none by default* | **optional**: a Selector that also implements it learns how each pick turned out |
| (Sender / Validator) | `pocket.HTTPSender` / `pocket.Validator` | POST relay bytes / verify+unwrap response |

**Relay flow** (`relay.Relayer.Relay`): fetch session → select endpoints(type) → `[sign → send → validate]` with failover to the next endpoint.

**RPC types split by lifecycle, not name** (all 5 are native Shannon types):
- **Stateless** (JSON-RPC, REST, CometBFT, **unary gRPC**) → one transparent reverse-proxy adapter, `transport/http.go`. **Done.**
- **Stateful streaming** (WebSocket **only**) → `transport/ws.go` + `websockets/` + `relay.Bridge`. **Done and LIVE-VERIFIED on beta 2026-07-22** (`pnf-pocket-beta`).
- **gRPC** depends on the supplier's relay miner: impossible on poktroll v0.1.34, supported (unary + buffered server-streaming, not full-duplex) by `../pocket-relay-miner`. **Live-verified on beta 2026-07-22.** It needs a gRPC Sender, not our HTTP one — *and* it needs the gRPC-Web fallback, see roadmap 1.

### Package layout

```
cmd/pocket-ap/   CLI + wiring + lifecycle (main.go = serve, call.go = one-shot)
health/          opt-in admin listener: GET /pocket-ap/health (own port, in-memory counters)
internal/safego/ panic containment for detached goroutines (see below)
websockets/      generic protocol-agnostic WS bridge (origin-checked; SAGE lift)
config/          YAML load (value types, unknown-field-strict)
domain/          dep-free types: RPCType, Session, Endpoint, RelayInput/Result
relay/           core flow + seam interfaces (relay.go = stateless, bridge.go = WS)
selector/        random endpoint selection
transport/       front adapters (http + ws, both real)
pocket/          Pocket/Shannon impls: fullnode, session, endpoint, signer, validate, sender, ws
```

## `call` — the one-shot mode

`pocket-ap call` is a **fifth transport**: it reuses `relay.Relayer.Relay` whole and
adds no core code. Two subcommands now exist (`serve`, `call`); a bare leading flag
still dispatches to `serve`, so `pocket-ap --config x` keeps working.

- **No `SessionManager.Start()` in `call`.** A one-shot process always misses the
  session cache, and `Session()` only consults the polled height on a *cache hit*
  (`session.go:99`). The poller would cost a CometBFT round trip and change nothing.
- **Diagnostics come from wrapping the seams**, not from changing them. `Relayer`
  returns only the response, so `recSessions` (session) and `recSelector` (order +
  outcomes) in `call.go` delegate + record to give `-v` the session, the supplier, and
  each failover. `recSelector` gets the per-attempt data by implementing
  `relay.Observer` — so `call -v` is the working reference consumer of the roadmap-#2
  hook, and proof the seams compose.
- **`config.validate()` no longer requires listeners** — that is a *serve* constraint,
  enforced in `runServe`. A `call`-only config needs no listener.
- **`--compare <url>`** sends the same request straight to a URL, concurrently, and diffs.
  Verdict tiers: `identical` (bytes) → `equivalent` (same JSON, key order/whitespace) →
  `differ`. Headers/body go out verbatim on both sides so the comparison stays fair.
- **Doctrine carve-out, read before "fixing" it:** `jsonEquivalent` in `call.go` is the
  only code that parses a payload. This does **not** license parsing elsewhere. The
  opaque-bytes rule governs the *relay path* — parsing that runs on every relay and
  drives decisions (which is exactly why roadmap #2 mandates an *active* height probe
  instead of reading responses). `jsonEquivalent` runs only in `call --compare`, only to
  label a diff for a human, and feeds nothing back into routing or selection. Generic
  JSON is also not a chain format — nothing there knows what `eth_call` is.

## Origin policy — BOTH listeners, not just WebSocket

`allowed_origins` is per-listener and applies to **HTTP and WebSocket alike**. Default (empty) = no `Origin` header is allowed (native clients: node/curl/go), every browser origin rejected. `"*"` opts everything in.

**Why plain HTTP needs it, which is not obvious:** CORS stops a malicious page *reading* a cross-origin response; it does **not** stop the request being *sent*. A page can `POST` to `localhost:8545` with `Content-Type: text/plain` — a CORS "simple request", so no preflight — and the relay is signed with the app key and billed to its stake before CORS is consulted. The attacker learns nothing; you still pay. It is a blind quota drain, not data theft. Demonstrated live 2026-07-16 (attempts 0→1 for `evil.example.com`), then fixed and re-verified (403, attempts stay 0). `Content-Type: application/json` would preflight and fail — **the attack works by using the *wrong* content type**, so "we only accept JSON" is not a defence.

- The gate runs **before** the relay. A 403 after spending the relay would defend nothing.
- `websockets.OriginPolicy.Allows` is shared by both listeners (`transport.OriginPolicy` is an alias). One rule, one place — two copies of a security decision is two things to get wrong.
- **CORS response headers are written for allowlisted origins**, else the allowlist would be a lie: relayed, but the browser still refuses to hand the page the response. Written *after* the backend's headers with `Set`, so a backend's own `Access-Control-Allow-Origin` cannot produce a duplicate (browsers reject those). Native clients get the backend's headers verbatim — passthrough stays honest for everyone who is not a browser.
- **Preflights are answered locally, never relayed** — a preflight asks *this proxy* "may this origin talk to you", which the backend cannot answer and which must not cost a relay. Only `OPTIONS` **with** `Access-Control-Request-Method` is intercepted; a service's own `OPTIONS` route still passes through.

### `allowed_hosts` — DNS rebinding, and why the default is conditional

A page on `evil.com:8545` whose DNS re-answers `127.0.0.1` is **same-origin to the browser**, so CORS is never consulted and `allowed_origins` never runs — it is bypassed, not defeated. A **GET** then carries no `Origin` and would sail straight through. (POST is already covered: browsers send `Origin` on every method except GET/HEAD, so JSON-RPC was never exposed. This is what protects **REST and CometBFT**.) The prize either way is a quota drain, not data theft — what this relays is public chain data.

What gives it away is the name the browser thinks it dialled: `Host: evil.com:8545` vs `localhost:8545`. So `transport.HostPolicy` (`hosts.go`) checks it, on **both** listeners, **before** the origin check.

**The default is derived from `Addr`, and that is the whole design — do not make it unconditional:**
- **bound to loopback** → loopback `Host` only. Rebinding dead, nothing breaks, because a loopback listener can only ever *be* localhost. This is also the only case rebinding can attack.
- **bound wider** (`:8545`, `0.0.0.0`) → **not checked**. Legitimate Hosts there are a LAN IP, a Docker service name, or a proxy domain — unguessable, and enforcing would break Docker and LAN access for nothing, since anyone on that network reaches the port directly and needs no rebinding.
- **explicit `allowed_hosts`** always enforces; `"*"` disables.

**⚠️ `config.example.yaml` binds `127.0.0.1`, deliberately.** `:8545` means every interface — anyone on your wifi can POST and spend your stake, no attack required, and they send no `Origin` so they read as a native client. Binding loopback also *switches on* the Host check above. Do not "helpfully" change the example back to `:8545`.

Verified live 2026-07-16: rebound `Host` → 403 / 0 relays; cross-origin POST → 403 / 0 relays; `127.0.0.1` and `localhost` clients both relay normally.

## WebSocket frame types — our bridge is RIGHT, do not "fix" it

`websockets/bridge.go` writes `dst.WriteMessage(msg.messageType, processed)` — it echoes the frame type (text vs binary) it just read from the other side. **That is correct. Verified across the whole stack 2026-07-17.**

**Why it looks wrong and isn't.** After signing, the payload is a binary protobuf — so a client's *text* frame carries binary bytes, which RFC 6455 §5.6 says should be valid UTF-8. Nothing validates it: not poktroll, and gorilla only checks UTF-8 on *close*-frame text, never data frames. It is a hack, and it is **load-bearing**: no relay protobuf has a frame-type field (`RelayRequest`, `RelayResponse`, `RelayResponseMetadata`, `Relay` — none), so **the WS envelope's type is the only channel carrying the backend's frame type**. Normalise it and the information is gone for good.

Everyone preserves, symmetrically — each hop echoes the type it just read:
| | |
| --- | --- |
| **poktroll** (reference miner) | `bridge.go:313` (→backend), `:436` (→gateway). Zero hardcoded frame types in non-test code. |
| **PATH** (origin, first WS commit `e92e25e2`, 2025-01-29) | `websockets/bridge.go:407`, `:446` |
| **SAGE** (derived from PATH) | `websockets/bridge.go:204`, `:216` |
| **pocket-ap** (lifted from SAGE) | `websockets/bridge.go` `route()` |

`eth_subscribe` is why "echo my own inbound type" beats "pair request to response": the backend pushes N frames for one client frame, so there is nothing to pair with.

**⚠️ If a browser gets a `Blob` instead of a string on beta, or a backend rejects our frames — that is the MINER, not us.** `../pocket-relay-miner` hardcoded `websocket.BinaryMessage` on both wrapped hops (`relayer/websocket.go:547`, `:616`) while correctly preserving on its raw hops (`:576`, `:639`). Reported and fixed in source 2026-07-17. Check the miner version before touching this package. Do **not** switch our bridge to binary to "match beta" — that would break us against poktroll and against a fixed ha-relayminer.

**CLOSED 2026-07-22 — fixed upstream, redeployed, verified on the wire.** The fix is `fe10eaa` "preserve the client and backend frame type across the bridge" (2026-07-17 11:54). Beta had been running `25285f2` (dated 2026-07-14, three days older, `fe10eaa` not an ancestor), which still carried the hardcodes at `:547` and `:616`. After redeploying to current `main`, the same subscription returns **text** both direct from CometBFT and through pocket-ap — where minutes earlier the proxied path returned binary. **That flip, on a miner-only change, is what proves our bridge was right all along.** `main`'s one remaining `BinaryMessage` (`:897`) is correct: server-initiated session-expiry frame, no inbound type to echo.

⚠️ The relay-miner checkout is **not** a sibling of this repo at `../pocket-relay-miner` — this file said it was for days, one failed `ls` got recorded as "source unavailable", and two wrong conclusions were written on top of that. Locate it (`git -C <path> rev-parse --show-toplevel`) instead of assuming; the source is public at `pokt-network/pocket-relay-miner`. **Never write about what is deployed without `git log -1 <sha>` and `git merge-base --is-ancestor <fix> <deployed>`** — the deployment commit called `25285f2` "main HEAD", and it was not.

## WebSocket close codes — sanitize before the wire, always

`websockets/bridge.go` `sanitizeCloseCode` sits between `determineCloseCode` and `FormatCloseMessage`, and it is the **only** place a close code becomes a frame. Do not bypass it.

**Why it exists.** RFC 6455 §7.4.1 reserves 1005, 1006 and 1015 for what an endpoint *infers locally* — no status sent, connection dropped, TLS failed — and forbids transmitting them. But `determineCloseCode` propagates the **peer's** code by design (so the real reason survives the trip), and a supplier that drops its TCP connection hands gorilla a `*CloseError{Code: 1006}`. The whole path is inside our own code: `extractCloseInfo` captures it → `SetCloseInfo` stores it → `determineCloseCode` returns it → `FormatCloseMessage` (which special-cases only 1005) encodes it → `Shutdown` writes it to **both** peers → the receiver checks `validReceivedCloseCodes[1006]`, finds false, and rejects the entire frame: `websocket: bad close code 1006`. The client learns nothing about why it was disconnected, and the miner rejects ours the same way.

Reserved codes map to **1011** (internal server error) — the honest answer when the failure is on our side and we cannot say more. The 3000-4999 application range passes through untouched, deliberately: the bridge propagates supplier-chosen codes like 4000 for session expiry, and flattening those would destroy the reason mid-flight.

`StartBridge` also sends **1013 (try again later)** through the same sanitizer when the endpoint dial fails. That is the one failure landing *after* the client upgrade but *before* a bridge exists, so `Shutdown`'s path is unavailable; a bare `Close()` there reads to the client as its own fault when in truth the supplier we picked is down. Reconnecting reselects, which is exactly what 1013 says.

Backported from SAGE `432263e` / `4da9965` on 2026-07-22 (SAGE got them from PATH `faad4777` / `e0d450c6`). The reproduction in `websockets/closecode_test.go` is mutation-checked: with the sanitizer stubbed out the client receives literally `websocket: bad close code 1006`.

### The two peers do NOT get the same close code

`Shutdown` built **one** close frame and wrote it to both sides. pocket-ap sits in the middle —

```
client --(we are the server)-- pocket-ap --(we are the client)--> relay miner
```

— and RFC 6455 §7.4.1 defines **1011/1012/1013 as things a SERVER tells a client**: "internal server error", "service restarting, reconnect", "try again later". Sent upstream they invert the roles. Session rollover is the single most common way a bridge ends here, and it was telling the miner *"session ended, please reconnect"* — asking the miner to reconnect to **us**, which is not a thing it does.

`endpointCloseCode` maps those three to **1001 Going Away**, which RFC 6455 defines for both directions ("a server going down OR a browser having navigated away") and which says exactly what happened: the peer that dialled you is leaving. **Everything else passes through unchanged** — 1000 means the same thing both ways, and the 3000-4999 application range (the miner's own 4000 at session expiry) is propagated deliberately, same as through `sanitizeCloseCode`.

This is **not** a protocol error the way the 1006 above is — gorilla accepts 1012 on read. It is about the operator on the other end being told something true.

⚠️ **The client side must keep getting 1012.** A "fix" that changed both directions is a different bug. `TestBridge_EachPeerGetsACloseCodeThatFitsItsRole` reads **both** peers in one run for that reason, over real sockets through `Shutdown` rather than against `endpointCloseCode` directly — a mapping function the production path never consults passes its own unit tests perfectly. Mutation-checked: with the remap stubbed to identity the supplier receives 1012.

Backported from SAGE `72ac553` (2026-08-19), itself a port of PATH `d5ef007c` (#526).

**Connection cap** (`websockets/limiter.go`, backported from SAGE `b826630`): a WS connection is not an HTTP request — it is long-lived, and `transport.WS.handle` blocks on `<-bridge.Done()`, so each live connection also pins an `http.Server` handler goroutine. Rate-limiting *upgrades* does not help; the cost is in connections that never leave. The reservation sits **before** `prepare`, so a flood at capacity costs one atomic load each rather than a session lookup each, and a `defer` covers every path because the handler blocks for the connection's whole life (PATH needs a goroutine handoff here; we do not). `Acquire` uses a CAS loop rather than add-then-rollback so the counter never overshoots even transiently — wrong exactly when something is reading it to decide whether to reject.

**LIFT note:** `websockets/connection.go` `WriteMessage` sets a write deadline. PATH added that hardening in `0f960d95` (2026-07-15); SAGE never backported it, so it arrived here missing and was added 2026-07-17. Writes serialise on a mutex, so a peer that stops reading (paused tab, wedged backend) would otherwise stall the whole bridge forever. **SAGE still needs this.**

## Panics on detached goroutines — contained, not crash-fast by accident

`internal/safego` (LIFT: SAGE `internal/safego`, `8873ea1`). **Every `go` statement in this repo runs its body under a recovery.** Before 2026-08-19 none of them did.

**Why it is not a nicety.** `net/http` recovers a panic in the goroutine serving a request, so a bad type assertion in a handler costs one 500. That protection stops at the goroutine boundary, and this repo crosses it constantly: the WS bridge routes **every frame** on its own goroutine, the block poller on another, each listener on another. `pocket/pubkey.go` documents the live instance — the SDK answers "this account never signed a transaction" as `(nil, nil)`, the nil `cryptotypes.PubKey` boxed into a nil `any` blew up a single-value assertion inside `ValidateFrame`, and because that runs in the bridge's goroutine **one frame from one keyless supplier took down every listener and every app**. That specific bug is fixed; the class was uncovered.

Crash-fast is defensible. Applying it **by accident, to whichever subset of failures happens to land on a detached goroutine**, is not.

- **`Go`/`Run`/`Recover` contain and log with a stack.** `Run` is for a **loop body**, not the loop: a recovery around the whole loop contains the panic and still leaves a dead ticker. That is why `pocket/session.go` wraps `pollBlockHeight` per tick — a frozen height does not look like an outage, because `Session()` only consults it on a cache hit, so the proxy would keep serving a session the chain retired and every relay would fail at the miner.
- **`Call` converts the panic to an error wrapping `ErrPanic`.** Used where a caller already knows how to handle failure. The three `errCh <- safego.Call(…, ListenAndServe)` listeners need it because a panic that merely recovered would never reach `errCh`, and `Serve` would block on a channel nothing writes to while the listener read as running.
- **`Recover` goes FIRST in the defer list** so it runs **last** (defers unwind LIFO) and can still contain a panic raised by one of the other defers. `cmd/pocket-ap/main.go` lists it above `wg.Done()`.

**⚠️ In the WS bridge, containment alone is a wedge, not a fix.** `transport.WS.handle` parks a handler goroutine on `<-bridge.Done()` for the connection's whole life and the connection limiter releases its slot from the same defer. A routing loop that stopped without shutting down leaks **both, permanently** — an idle subscription's socket never closes on its own. So `run()` carries `defer b.Shutdown(...)`, and each read-loop pump shuts down on exit too: a read loop is one of two pumps feeding `msgChan`, and one dying quietly leaves the routing loop blocked forever. `websockets/panic_test.go` is mutation-checked against all three layers — no outer `Go` → the test **binary** dies; contained but no shutdown → `Done()` never closes; no inner `Call` → the client is told **1012 "service restarting"** for a frame that is in fact poison, instead of 1011.

**`safego.Panics()` is reported as `recovered_panics` in `/health`** (omitted while zero). Containment without visibility is its own failure mode: a caught, logged, uncounted panic turns a loud crash into a quiet one. It deliberately does **not** set `degraded` — the abandoned work decides that (a dead poller shows up as a stale height either way), and a long-lived process must not sit at 503 forever over one recovered frame.

## Conventions & gotchas

- **Do NOT reimplement the crypto.** Ring signing is `shannon-sdk` + `poktroll/pkg/crypto/rings` + `ring-go`, network-verified. The signer is a near-verbatim lift of SAGE `signer.go`. Touch it only to re-sync with SAGE.
- **Package is `pocket`, never `shannon`.** Shannon is the only live Pocket protocol (Morse was sunset); naming the generation just reintroduces Morse/Shannon confusion. `// LIFT: SAGE protocol/shannon/...` comments are source pointers, not naming.
- **A wrong-length key silently signs as the wrong app.** `deriveAppAddr` now enforces 32 bytes. Without the check, `""`, a truncated paste, and a 33-byte key each returned a **plausible `pokt1…` address and no error** — the 33-byte one returning the *same* address as the correct key. The proxy would start, derive an address for an app that is not yours, and fail every relay with "session not found" while the key looked fine. SAGE's `apps.go:58` has the same gap.
- **Signing is the SOVEREIGN app-key model.** `pocket.Signer` derives the app address *from the signing key*, so the configured key **must be the app's own key, never a gateway key.** The app key validates even when the app is delegated to a gateway (the app is always a ring member). This is the opposite of SAGE's centralized mode (which signs with the gateway key).
- **`domain` stays dependency-free.** `domain.Session.Raw` carries the `*sessiontypes.Session` (as `any`) so the signer can read the SessionHeader without domain importing poktroll.
- **CI lint must install a golangci-lint v2 binary — the version is coupled to `.golangci.yml`.** The config is `version: "2"`, and a v1 binary cannot parse it: it fails with `unsupported version of the configuration` while `make lint` passes locally (local has v2). `golangci-lint-action@v6` installs **v1** by default, which is exactly this trap. `.github/workflows/ci.yml` pins `golangci-lint-action@v7` + `version: v2.11.3` to match local, and runs `go build ./...` first — a cold type-check of the cosmos-sdk / geth / cometbft tree otherwise blows the lint timeout on a fresh runner. **Mirror of SAGE's `ci.yml`, which fixed this first; pocket-ap had drifted to the old `@v6`.** When bumping the local golangci-lint, bump the CI `version:` pin in lockstep or CI and local disagree.
- **`network: beta|main` and `--network` are a SHORTHAND for the two fullnode endpoints, never a supplement.** `config/networks.go` is the **source** for those hostnames; `config.example.yaml`, `config.schema.yaml`, `README.md` and `AGENTS.md` are copies, and `TestNetworkEndpointsAreDocumentedEverywhere` checks the copies against the source — so a rotation is one edit in Go plus a list of which docs still disagree. Three rules, each load-bearing:
  - **Both transports move together.** A config reaching one chain for sessions and another for block height fails like an outage, not like a typo. Naming the pair makes that unrepresentable, the same way `pocketd --network=beta` sets `--chain-id`, `--node` and `--grpc-addr` together.
  - **`network:` + `fullnode.*` in one FILE is an error, not a precedence rule.** Either order silently discards something the operator wrote, and the result — a full node that has never heard of your app — reads as "the network is broken". **The `--network` FLAG does override the file**, because typing it is a statement of intent for this run; that is the whole point of switching quickly.
  - **`applyNetwork` forces `grpc_insecure: false`.** Otherwise someone moving from a local node to a public endpoint keeps shipping every session query in plaintext while believing they only changed networks. Mutation-checked.
  - `main` carries `SpendsRealValue`, which drives a startup **WARN** — a log line, not a prompt, because a proxy runs under systemd where a prompt is a hang. **`config.example.yaml` ships `beta`**, so the default costs nothing; MainNet is a deliberate word.
- **config:** value types, no `*bool`/`*int`; unknown fields error at load (`KnownFields(true)`). App key from `app.private_key_hex` / `apps[].private_key_hex`, or `POCKET_APP_PRIVATE_KEY` / `POCKET_APP_PRIVATE_KEYS` (see roadmap 7). Two knobs added 2026-07-22: top-level `grpc_mode` (see roadmap 1) and per-listener `max_connections` (WebSocket only). **`max_connections: 0` means the 10000 default, NOT unlimited** — an unbounded count of long-lived connections is the exact failure being prevented, so it must not be what saying nothing gets you; negative disables it. Every new config field must also land in `config.schema.yaml` or `TestSchemaMatchesConfigStructs` fails.
- **Deps were bumped 2026-08-19 in lockstep with SAGE** — `go` 1.26.4 → 1.26.6, `poktroll` v0.1.34 → v0.1.35, `shannon-sdk` → `20260812141256`. ⚠️ **That is a record of the bump, not a statement of the current floor: `go.mod` is the authority and nothing here tracks it.** SAGE wrote a standing Go patch version into its CLAUDE.md and it drifted within days (`0dc348c`) — do not reintroduce one. The Go bump is what mattered: it closes six stdlib CVEs govulncheck reports as reachable from a request path exactly like ours — `net/http`, `crypto/tls`, `net/url`, `encoding/asn1`, `net/idna`, `html/template`. **Re-verified here rather than taken from SAGE's writeup**, because a shannon-sdk bump is what removed `SignWithRing` last time: `x/service/types/relay.pb.go` and all of `x/session` are **byte-identical** between v0.1.34 and v0.1.35 (so frame-signing byte-identity is untouched), the `RPCType_name`/`RPCType_value` maps are identical (so `rpcTypeToShared` and `pocket/endpoint.go` are unaffected), and shannon-sdk changed **no Go file at all** — only `go.mod`/`go.sum`.
- **`connectGRPC` pins `MinVersion: tls.VersionTLS12`** (`pocket/fullnode.go`, LIFT: SAGE `2f061bd` S3). `crypto/tls` already floors a client at 1.2, so this changes nothing today — it stops the floor drifting with a toolchain bump. Worth doing here because it is **one** function: the four full-node clients *and* `pocket/grpc.go`'s supplier sender all dial through it, where SAGE had two sites that could disagree.
- **`Rpc-Type` header** (`pocket/sender.go`) is how the relay miner routes to the right backend. Keep `rpcTypeToShared` in sync with `domain.RPCType`.
- **Public keys: ONE shared cache (`FullNode.PubKeyFetcher()`), and a nil key is NEVER cached.** `sdk.AccountClient.GetPubKeyFromAddress` is a bare gRPC query — a full-node round trip per signature check, which on WebSocket is per **frame** — so everything goes through `pocket/pubkey.go`. Both halves of that sentence are load-bearing:
  - **Shared**, on `FullNode`, because the signer needs the same keys the validator does: verification looks up suppliers, ring building looks up the app and its gateways, and with several apps a per-owner cache re-fetches a shared gateway's key once per app. `Signer` reading the raw `accountClient` was the bug SAGE fixed in `88302f6`; `ringCache` is keyed by session end height, so it misses at **every** rollover (minutes on beta, ~20 on mainnet) and with no singleflight every concurrent relay missed at once.
  - **Never nil.** The SDK reports "this account never signed a transaction" as `(nil, nil)` (`account.go:69` → `BaseAccount.GetPubKey()`). Caching it **panicked** on the next lookup — a nil `cryptotypes.PubKey` boxes into a nil `any`, and the single-value type assertion blew up. On the WS path that **was** fatal to the **process**: `ValidateFrame` runs in the bridge's own goroutine, which had no `recover()`, so one frame from a keyless supplier took down every listener and every app. `internal/safego` now bounds that to one bridge — containment, **not** a licence to reintroduce this. ⚠️ **Do not "complete" this with a negative TTL.** SAGE buys a 15m nil TTL + a per-address refetch gate + `invalidateRingKeys` because at 66k RPS re-asking per relay would hammer the full node; here a keyless supplier is one of ~32, is failed over immediately, and re-asking is cheaper than any machinery that makes a stale nil safe. `getOrCreateRing` catches nil itself and names the address — a keyless ring member blocks signing for the **whole app**, not one supplier.
- **`RelayMinerError` is read on validation failure** (`minerErrDetail`, `pocket/validate.go`). The miner reports its *internal* failures in that field rather than as a transport error, so without it a miner-side failure is indistinguishable from a backend one. ⚠️ Reachable on exactly **one** branch — shannon-sdk returns the response alongside the error only when `ValidateBasic` fails (`relay.go:99`); unmarshal, pubkey-fetch, nil-pubkey and signature failures all return nil. Do not document it as always available.
- **`local/` is gitignored and holds configs WITH PRIVATE KEYS.** Never commit it. Never print key values — length only, never the string. `TestNoSecretsInTrackedFiles` (`config/secrets_test.go`) fails the build if a 64-hex key reaches a tracked file, but it is a backstop, not permission to be careless. Beta smoke tests need a PNF-held app key; ask for it and put it in `local/` or `POCKET_APP_PRIVATE_KEY`. **Do not write down which repo or file to copy it out of** — a public repo should not carry directions to a live key.
- **Two full-node transports** are required: gRPC (`grpc_host_port`, for session/app/account/params) **and** CometBFT RPC (`rpc_url`, for block height). `grpc.NewClient` is lazy — construction never dials; the first `GetSession`/height poll does.
- **Public full-node endpoints — `sauron-*.infra.pocket.network`, verified live 2026-07-17.** Beta (chain `pocket-lego-testnet`): `sauron-grpc.beta.infra.pocket.network:443` + `https://sauron-rpc.beta.infra.pocket.network`. MainNet (chain `pocket`): same without `.beta`. Both TLS on `:443`, so `grpc_insecure: false`. There is also `sauron-api[.beta]…` (REST/LCD) which **we never call** — it is for `pocketd`/explorers, and is unrelated to a `rest` *listener*.
  - **The beta full-node hosts BEFORE sauron are dead** — they stopped resolving entirely (no DNS). `config.example.yaml` still pointed at them until 2026-07-17 while `config.schema.yaml` already said sauron — so the file every new user copies gave them a proxy that could not reach a node, and the schema beside it disagreed. SAGE's `local/beta-config.yaml` was already on sauron. The old names are deliberately not written down here: they resolve to nothing, and a dead host in a tracked file is something a reader can only paste by mistake.
  - The endpoints are written in **four** places — `config.example.yaml`, `config.schema.yaml`, `README.md`, `AGENTS.md`. `TestExampleEndpointsAreDocumentedEverywhere` (`config/schema_test.go`) fails if you change the example without the other three. It cannot detect a *dead* host (that needs the network, and these rotate); it exists to make the next rotation loud.
- **`pocketd` is a separate binary and pocket-ap does not need it** — it stakes the app and answers "what does the network offer". Install: `curl -sSL https://raw.githubusercontent.com/pokt-network/poktroll/main/tools/scripts/pocketd-install.sh | bash` (one binary to `/usr/local/bin`, checksum-verified, no node). **Always pass `--network=beta|main`**: it sets `--chain-id`, `--node` and `--grpc-addr` together, and without it `pocketd` queries `tcp://localhost:26657` and fails as if the network were down.

## Bugs we owe upstream

This is a near-verbatim lift of SAGE, so a bug found here is usually a bug found
there — and **nobody upstream is tracking these**.

Open right now: **nothing.**

- **poktroll's faucet client is broken on every network** (`app/pocket/networks.go:53-55`): all three hardcoded URLs are NXDOMAIN, and `--faucet-base-url` — which `pocketd faucet fund --help` tells you to use — is not a real flag, so it cannot be pointed at the faucet that does work. Found 2026-08-19 while writing the onboarding docs. **Decided 2026-08-20 not to file it** — the working beta faucet is a browser page and the README says so, which is what an onboarding reader needs. Kept here because the finding is still true and someone will rediscover it; do not spend time re-deriving it.

Previously open, now closed: **nothing else.** Both entries that used to sit here were re-checked on 2026-08-19 and neither survived — which is the point of re-checking rather than carrying a list forward.

- **"SAGE `apps.go:58` still has the missing app-key length check" was simply WRONG.** SAGE has `secp256k1KeyLen = 32` and enforces it in `buildOwnedApps` (`apps.go:77`), and `git log -S` puts it there since its initial commit. It never had the gap. ⚠️ This is the failure the relay-miner note further up warns about, repeated: a claim about another repo written from memory instead of from the source. **Read the file before writing that we are ahead of someone.**
- **"ha-relayminer's WS binary-frame fix is not deployed on beta" was stale**, and contradicted by three other places in this same file — the frame-type section's own "CLOSED 2026-07-22", the 2026-08-04 Validated row, and every beta run since, including 2026-08-19, which saw text frames.

Closed since: SAGE backported PATH's WebSocket write deadline itself (`4da9965`, 2026-07-17).

**SAGE catch-up, 2026-08-19 — four taken, the rest rejected with reasons.** Taken: `8873ea1` panic containment (`internal/safego`), `72ac553`'s `endpointCloseCode` half, the `a6cf259` dep bump, and `2f061bd`'s S3 TLS floor. Rejected, and each reason will recur:
- **PATH #522-#527 / SAGE `934d293`, `c75df1f`, `fa3242b`, `b233781`** — reputation, circuit breaker, per-operator concentration cap, QoS sync allowance, metrics label cardinality. Banned outright by the doctrine at the top of this file. #527's cardinality work is doubly moot: pocket-ap exports no metrics.
- **SAGE `blocked_domains` (in `72ac553`)** — we already have this, and better. `allow_hosts`/`deny_hosts` plus the `X-Pocket-*-Hosts` headers are **per request**; theirs is restart-only config. Do not "add" it.
- **PATH #525's WS idle reaper** — it ANDs "never established a subscription" with "sent no client frame", and each half alone is wrong. The subscription half needs payload parsing, which the opaque-bytes rule forbids; frame-silence alone is the half that reaps a legitimately quiet subscriber. Skip unless someone wants silence-only with a much longer default.
- **SAGE `09128cd` B2 (unbounded `endpointCache`)** — not applicable. Their cache is keyed on `session.SessionId`, which is new at every rollover; ours lives inside `domain.Session` under a `serviceID:appAddr` key, and `evictStaleRingsOnRollover` (`pocket/signer.go`) already bounds the other per-session cache. Its S2/X2/X3 halves (reputation cache, height getter, metric labels) have no counterpart.
- **SAGE `09128cd` S1 (error text leaking internals)** — *deferred, not dismissed.* `transport/http.go` writes `relay failed: %v`, the whole cause chain, to the client. Milder here (loopback by default, and the caller is the operator) but it is the same shape. Revisit if a listener is ever bound wider.
- **`b515bf6`/`2f061bd` CI vuln gate** — still worth taking, still not taken. If ported, take the **binary-mode** gate: source mode peaks at 4.3 GB and OOM-kills a 7 GB runner. (The dispatch problem this line used to cite as a blocker is gone — it was private-repo metering, fixed by going public on 2026-08-19.)
- **`7c1c3e4` CI cache** — **superseded 2026-08-20 by our own, which solves a problem SAGE does not have:** a *tag-triggered release* compiling five targets cold. See the release-cache bullet below.

**The traffic goes both ways — check before assuming we are ahead.** SAGE's `0e1e48b` "forward request path and verb to the supplier" is SAGE catching up to something pocket-ap has always done (`buildTargetURL` + `in.Method` in `pocket/signer.go`), and its `3f6e552` per-bridge session expiry adopts our design over its own broadcast channel. Meanwhile SAGE's `432263e`/`4da9965`/`b826630` WS hardening and its gRPC-Web work were **ours to backport**. When syncing, diff both directions.

**SAGE's 2026-08-06 perf batch, triaged (2026-08-06) — one of five transferred.** Recording the *rejections* because each has a reason that will recur:
- `bd717b9` **supplier pubkey cache** — SAGE catching up to `pocket/pubkey.go`, which we have had since the initial commit. Its blacklist classification and Prometheus metrics are doctrine-banned here. Its `RelayMinerError` half **was** worth taking.
- `88302f6` **build rings from the cached keys** — **taken**; we had the identical gap. Taken *without* SAGE's negative-TTL/refetch-gate/`invalidateRingKeys` machinery, which only exists to make a cached nil safe (see the pubkey bullet above).
- `b32b697` **drop two per-relay allocations** — the reputation-key memo has no counterpart (no reputation package), and the request-ID finding is one we already satisfy: every `slog.Default().With` here runs **once at construction**, never per relay.
- `2091bd5` **size the request body buffer** — skipped. They profiled at 66k RPS; we are ~1 rps with ~55ms of every relay being pure distance. Also not the same defect: `io.ReadAll` costs one 512B allocation, not `bytes.Buffer.ReadFrom`'s 512 bytes of headroom *per read*.
- `934d293` **PATH #522-525 selection / reputation / circuit breaker** — banned outright by the doctrine at the top of this file.

If you fix something in a lifted file, check whether SAGE has it too and record it
in this section.

## Lift provenance (keep in sync with SAGE `protocol/shannon/`)

| pocket file | lifted from |
| --- | --- |
| `pocket/fullnode.go` | `fullnode.go` |
| `pocket/validate.go` | `fullnode.go:113` + `relayer.go:237` (deserialize) |
| `pocket/session.go` | `sessions.go` (dropped `SessionExpiredEvent` — WS-only) |
| `pocket/endpoint.go` | `endpoint.go` (**added `GRPC`** to the rpc-type map, which SAGE omits) |
| `pocket/signer.go` | `signer.go` + `relayer.go:166-215` (build) + `apps.go:58` (key→addr) |
| `pocket/sender.go` | `transport.go` (`sendHTTP` + `Rpc-Type`) |
| `pocket/ws.go` | `ws_processor.go` (raw-payload sign/validate) |
| `websockets/` | `websockets/` (**origin check rewritten** — SAGE allows all origins; see roadmap 1) |
| `websockets/limiter.go` | `websockets/limiter.go` (SAGE `b826630`; the cap wires into `transport/ws.go`, not a relayer) |
| `pocket/grpc.go` (web half) | `protocol/shannon/grpc.go` (SAGE `25742f1`) — framing, `rawCodec`, `isNotHTTP2`. **Not** its `AnalyzeGRPC`: that is heuristics, banned here by doctrine. |
| `relay/bridge.go` | `ws_relayer.go`, stripped of reputation/heuristic/observe/flags/load-spread (~100 of its 421 lines were relevant) |
| `internal/safego/` | `internal/safego/` (SAGE `8873ea1`) — **minus its metrics wiring**: no Prometheus here by doctrine, so the counter surfaces through `/health` instead. `GoCtx` omitted, nothing needs it. |

## What's missing (roadmap)

1. **~~WebSocket~~ DONE (2026-07-16) — streaming gRPC still open.** `websockets/` (generic bridge) + `relay.Bridge` + `transport.WS`. Config: `rpc_type: websocket` + `allowed_origins`.
   - **✅ LIVE-VERIFIED 2026-07-22 on beta `pnf-pocket-beta`** — the first real WS relay, and it worked first try. A `subscribe` for `tm.event='NewBlock'` returned the ack plus a real 8.5KB NewBlock event at height 493120; the miner handshake, which this file expected to be the sticking point, needed no changes. Session rollover then fired exactly as designed (height reached `EndBlockHeight` → close 1012 "session ended, please reconnect"). Two caveats: the miner still forces **binary** frames (see the frame-type section — theirs, not ours), and beta sessions are short enough that a rollover lands within a minute, so a long-lived subscription reconnects often.
     - The old blocker is gone: beta now has `pnf-pocket-beta`, **32 suppliers × all five RPC types**. `pnf-anvil` is still `JSON_RPC`-only, which is why this went untested so long. Mainnet carries 1170 WS endpoints across `eth`/`base`/`bsc`/`poly`/`op`.
   - **The payload is RAW frame bytes, not an HTTP envelope.** `pocket.SignFrame`/`ValidateFrame` exist precisely because `SignRelay`/`ValidateResponse` are wrong here: the miner writes `RelayRequest.Payload` verbatim to the backend **and hashes those exact bytes for onchain proof verification**. HTTP-wrapping breaks proofs at claim time, not at test time. `buildFrameRelayRequest` is split out and tested for byte-identity for this reason alone — do not "unify" it with the HTTP path.
   - **Rollover closes the bridge; it does not re-sign.** SAGE does the same (`ws_processor.go:32`). Clients reconnect, which reselects a supplier and a fresh session. This deletes the hardest thing the old stub claimed to need.
   - **The fan-out bug is SAGE's, and we do not have it.** SAGE broadcasts expiry on one shared channel, so each bridge eats events meant for others (`ws_relayer.go:248-255`: *"Acceptable for v1 with typically 1–few bridges per service"*). Here each bridge polls `SessionManager.LatestBlockHeight()` against its own `EndBlockHeight` — no channel, no registry, so the bug cannot exist. **Do not "improve" this into a broadcast.** Height also beats re-checking via `Session()`, which has no singleflight and would stampede `GetSession` at every boundary.
   - **Height 0 never expires**: a dead poller must not look like universal expiry.
   - **`transport.WS` tracks live bridges** (`bridgeset.go`) because `http.Server.Shutdown` waits on handlers, and a bridge handler blocks until its socket closes — never, for an idle subscription.
   - **SECURITY — the origin check is a deliberate divergence from SAGE.** SAGE allows all origins (`CheckOrigin: return true`); it is an authenticated gateway. pocket-ap is unauthenticated on localhost holding the app key, and WebSocket is **not** covered by the same-origin policy — no preflight, no read restriction — so a permissive default lets any site the user visits relay with their key. Default: no `Origin` → allow (native clients); any browser origin → reject unless allowlisted. **Do not copy SAGE's upgrader.**
   - **gRPC: possible, but it depends on WHICH relay miner the supplier runs.** Corrected 2026-07-17 — an earlier version of this file said "impossible", which was wrong. It was reasoned from **poktroll v0.1.34** (the released miner), where it is true: `http_server.go:204` routes to the async path *only* for `Rpc-Type == WEBSOCKET`, everything else is one-request/one-response, and there is no streaming machinery at all. But the miner is being replaced.
   - **`../pocket-relay-miner` (ha-relayminer) supports gRPC today.** Its README claims "JSON-RPC (HTTP), WebSocket, gRPC, REST/Streaming (SSE)". What it actually does:
     - **The trailers problem has a workaround I missed.** gRPC carries `grpc-status` in HTTP/2 trailers and `POKTHTTPResponse` has no trailers field — that constraint is real. The answer is `mergeTrailersIntoHeader` (`relay_grpc_service.go:718`): fold the trailers into the response *header* map, which POKTHTTPResponse does have, and unfold client-side. Finding a constraint is not the same as proving impossibility.
     - **Scope, per `relay_grpc_service.go:653-657`:** "covers unary and buffered server-streaming; it does **not** model full-duplex streaming, which this proxy does not forward." So unary ✅, server-streaming ✅ (buffered), full-duplex ❌ — and that last one is a deliberate design line, not a hard limit.
     - **⚠️ It rides a gRPC transport to the miner, NOT the HTTP POST we use.** `mergeTrailersIntoHeader` is called only from `relay_grpc_service.go`; `proxy.go` (the HTTP path) never folds and never branches on `BackendTypeGRPC`. gRPC over our current `HTTPSender` would silently lose `grpc-status`.
   - **`pocket.GRPCSender` + `pocket.MultiSender` are BUILT (2026-07-17) and LIVE-VERIFIED (2026-07-22).** A signed relay to `/cosmos.bank.v1beta1.Query/Params` on `pnf-pocket-beta` returned `QueryParamsResponse{params:{default_send_enabled:true}}` — byte-for-byte the same answer REST gives — first attempt, no failover.
     - **⚠️ NATIVE gRPC ALONE DOES NOT WORK ON BETA. The gRPC-Web fallback is load-bearing, not a nicety.** The native attempt gets `505 HTTP Version Not Supported` / `"gRPC requires HTTP/2"` from the ingress in front of `rm.beta.infra.pocket.network`, which terminates HTTP/2 and forwards HTTP/1.1. gRPC-Web crosses it untouched because it carries its trailers as a frame **inside the body** instead of as HTTP trailers. Measured live; the native-only sender we shipped on 2026-07-17 would have hard-failed here.
     - **`grpc_mode` config (empty | `native` | `web`), default empty = auto.** Auto tries native once **per supplier host** and remembers the answer in `webOnly`, so only the first relay to a host pays the doomed handshake. The memo is keyed on the *one* error meaning "wrong framing" (`isNotHTTP2`) — anything else is a real failure and must **never** be retried in another protocol, or a broken supplier hides itself behind a second illegible error.
     - **The Sender must NOT decode the reply.** `rawCodec` + `grpc.ForceCodec` keep the wire bytes verbatim in both directions. The supplier signs the exact bytes it sent, so decoding a `RelayResponse` and re-marshaling it before `ValidateResponse` checks the signature against something the supplier never produced. Protobuf round trips are not byte-identical (field order alone breaks it), so the failure is intermittent and reads as a lying supplier. This is what the code did until 2026-07-22; `TestGRPCSender_ReturnsSupplierBytesVerbatim` pins it with a hand-encoded non-canonical response.
     - No seam changed. `relay.Sender` already takes `rpcType`, so `MultiSender` routes on it: gRPC to the miner over gRPC, everything else over HTTP POST. **That routing is load-bearing** — `mergeTrailersIntoHeader` runs only on the miner's gRPC service, so gRPC over `HTTPSender` would silently drop `grpc-status`.
     - `MultiSender.SendStream` returns a gRPC reply as ONE complete body with **no** Content-Type, so `RelayStream` treats it as a single batch. Correct because the miner buffers server-streaming and never forwards full-duplex — a gRPC reply is always one RelayResponse.
     - **We read the URL scheme for TLS; the miner's own client does not.** It strips `https://` and then dials `insecure.NewCredentials()` (`cmd/relay/grpc.go:103-107`) — fine against a local test miner, broken against any real https endpoint. Don't copy that.
     - **Test note:** Go's `http.Server` hands the HTTP/2 connection preface to the handler as a `PRI *` request rather than rejecting it. That is what makes an `httptest` HTTP/1.1 server a faithful stand-in for the beta ingress, and it is how `TestGRPCSender_AutoFallsBackToWebAndRemembers` counts native probes to prove the memo works (1 probe across 2 relays).
     - Wire constants pinned to literals in tests: `/pocket.service.RelayService/SendRelay` and the `rpc-type` metadata key. Everything else (including the fake miner) refers to them by symbol, so a change would keep both sides agreeing while no real miner answered.
   - **The gRPC LISTENER is built too (2026-07-17)** — `transport/grpc.go` + h2c in `transport/http.go`. A real grpc-go client completes calls against it in tests.
     - **h2c only on `rpc_type: grpc` listeners.** Go's `http.Server` negotiates HTTP/2 only via TLS ALPN, so without h2c a gRPC client cannot connect to a plain socket at all. Every other type is HTTP/1.1 request/response and gains nothing from it.
     - **`Grpc-Status` is unfolded from headers back into TRAILERS** (`splitGRPCTrailers` + `http.TrailerPrefix`). The miner folds it *into* headers because POKTHTTPResponse has no trailer field; a gRPC client handed the status as a header alongside a body cannot interpret the reply. `grpc-encoding` stays a header — it describes the body, not the outcome.
     - **Only for gRPC listeners.** A JSON-RPC backend that happened to send a `Grpc-Status` header gets it passed through untouched — passthrough still holds for everything else.
     - Test note: "does a gRPC call error?" proves nothing about h2c — without it the *connection* fails, with it the connection succeeds and the *call* fails. Both error. The tests use an HTTP/2 prior-knowledge client to check the protocol directly.
   - **Sequencing:** none of this is worth building until ha-relayminer is actually deployed by suppliers. Today all 15 mainnet "gRPC services" advertise the *same* URL — one operator, one relay miner, one supplier each, no failover. (Re-derive it with the `list-suppliers` query below rather than hard-coding a host here; naming an operator in a tracked file is against the rule in roadmap 5.) Check what miner they run before assuming a gRPC relay would land.


2. **~~Selector outcome-feedback hook~~ DONE (2026-07-16) → optional per-service QoS still open.** `relay.Observer` + `relay.Outcome` exist; the `Relayer` type-asserts its `Selector` and calls `Observe` once per attempt. `selector.Random` is untouched and pays nothing. `cmd/pocket-ap/call.go`'s `recSelector` implements it (that is where `call -v`'s per-attempt line comes from) — a **working reference consumer**, so copy its shape.

   Shipped shape, and why it differs from the original sketch:
   ```go
   type Outcome struct {
       ServiceID domain.ServiceID  // added: one Relayer serves ALL services
       RPCType   domain.RPCType    // added: same reason
       Success   bool
       Latency   time.Duration     // Send only — validation is our own CPU
       Err       error
   }
   ```
   `ServiceID`/`RPCType` are **not optional extras**: `main.go` builds one `Relayer` over `uniqueServices(cfg.Listeners)`, and one supplier can serve several services. Without them a supplier fast on service A and slow on B collapses into a meaningless average. A signing failure is deliberately **not** observed — it is our bug, and blaming a supplier for it would poison the feed.

   **`Select` now takes `serviceID` too** (2026-07-16): `Select(serviceID, endpoints, rpcType)`. Both halves are therefore service-aware. Chosen over one-Relayer-per-service because it was one real call site vs. rewiring `main.go`, and because `Relay(ctx, serviceID, …)` already takes serviceID, so per-service Relayers would have forced a signature change anyway. `selector.Random` ignores it.

   ### The QoS selector is REJECTED — do not build it here (decided 2026-07-16)

   This roadmap item used to end "then QoS becomes an opt-in Selector… lift from SAGE `blockconsensus.go` / `endpointstore.go` / `selector.go`". **That contradicted the doctrine at the top of this file** — "QoS, heuristics, reputation … live in SAGE and are intentionally absent here. **Do not add them.**" The QoS item was added in a commit that never touched the ban, so the contradiction was accidental, not an override. (That used to cite the SHA. It no longer can: the history was squashed to a single commit before this repo went public, which is exactly why a claim in this file should rest on something a reader can still check — the doctrine paragraph at the top of the file, not a hash.) Resolved in favour of the doctrine: height-awareness, blocks-behind heuristics and error/latency EWMA *are* the banned list, and pocket-ap's whole pitch is that it is not a gateway. **If a task wants supplier quality, the answer is SAGE.**

   What stays, and why it is not QoS:
   - `relay.Observer` / `relay.Outcome` / `relay.WithObservers` are a **seam**, not a policy. They report what happened and decide nothing. They cost nothing when nobody listens (`selector.Random` does not), and they already pay for themselves twice: `call -v`'s per-attempt line and `health`'s counters both read the feed.
   - `serviceID` on `Select` stays too: it is one parameter, it makes the seam honest, and it costs nothing.
   - **Failover already covers hard failures** (`relay.go`, tested): a dead or lying supplier is dropped and the next is tried. QoS would only add *soft* preference — slow/stale suppliers — which is exactly the gateway smarts that belong upstream.
   - **`selector.Filter` (supplier allow/deny, 2026-08-04) is NOT covered by this ban — do not delete it as QoS.** Requested by an external user: allowlist = "route to any of *these*", denylist = "any *but* these". It keeps no state, measures nothing, and never changes its mind; it is the operator declaring who they will pay, which is the *point* of a self-hosted access point rather than the gateway smarts this repo pushes upstream. The banned thing is inferring supplier quality from behaviour, and a static list infers nothing. An allowlist is also the intended way to drive selection from an **external** process, which keeps the scoring logic outside pocket-ap where the doctrine wants it.
     - **Per-request lists via header (2026-08-06) — the same feature, minus the restart.** This bullet used to end "compute the set, write it, **restart**". The external user came back on that: a dynamic QoS module cannot restart the proxy between requests, so a config-only list makes the whole external-process story a fiction. The shape they asked for, and what is now built:

       ```
       user --(request)--> QoS --(request + allowed suppliers)--> pocket-ap
       ```

       `X-Pocket-Allow-Suppliers` / `X-Pocket-Deny-Suppliers` (plus the `-Hosts` pair below), comma-separated, repeatable, honoured on **both** front doors and by `call -H`. `transport/suppliers.go` parses them; `domain.SupplierPolicy` carries them on ctx; `selector.Filter` applies them. **This is still not QoS by the ban above** — the list arrives fully formed, pocket-ap measures nothing and decides nothing. It moved the decision *further* out of this repo, which is the direction the doctrine points.
       - **⚠️ A request can only NARROW the configured lists, never widen them. Do not "simplify" this to header-overrides-config.** Both policies apply and both must permit a supplier (`configured.Permits(s) && requested.Permits(s)`). The listener is unauthenticated and holds the app key: a header that could *add* a supplier the operator excluded hands routing to anything that can reach the port, and a config `deny` — which exists to take a misbehaving supplier out — would be undoable by the misbehaving party's own traffic. Tested from both sides (`TestFilter_RequestPolicyCannotWidenTheConfigAllowlist`, `…CannotUndoAConfigDeny`).
       - **They are STRIPPED from `in.Header` before signing.** Everything left there is signed into the relay and replayed to the backend, so forwarding them would tell a supplier how the caller ranked its competitors, and put proxy-control bytes into the onchain proof. `TakeSupplierPolicy` removes as it reads — that is why it is `Take`, not `Get`. **Every list is taken before the first error is returned**: short-circuiting on a bad list would leave the later headers in the map, and the map is what gets signed. The test drives each of the four lists as the bad one, because a single case only catches a short-circuit on that one list. On the WS path it reads from a *clone* instead, because `relay.Bridge.Prepare` builds the miner handshake from scratch and nothing forwards the client's headers there.
       - **A non-`pokt1…` value is a 400 with zero relays spent**, mirroring the config-time check for the same reason: an address that never matches makes an allowlist drop everything and a denylist do nothing, and both read as "the network is broken". Config can only fail at startup; a request can be told, so it is.
       - **`Selector.Select` gained a `ctx` parameter** to carry this. Deliberately not a `RelayInput` field or a `Relay` argument: the relay core must not know a routing preference exists — it fetches a session and hands over endpoints, and what narrows them is between the front door and the Selector alone. `selector.Random` ignores it, as it ignores `serviceID`.
       - **No config knob gates it.** It cannot exceed the config ceiling, so "off" is spelled by setting `suppliers.allow`.
       - **URL/host matching is NOT supported, deliberately** — see the note after this list.
     - **Per app, not global.** Config lives on the app entry, which *is* per-service because app↔service is 1:1. A global list would be actively wrong the moment a second app exists: service A's suppliers do not serve service B, so allowing A's would leave B with nothing.
     - **Deny is applied first and wins.** A supplier is denied because it is misbehaving; a stale allowlist entry must not resurrect it.
     - **⚠️ Wire `Filter` INSIDE `relay.WithObservers`, never around it.** The `Relayer` finds its Observer by type-asserting the Selector, and `Filter` is not one — `Filter{Inner: WithObservers(...)}` compiles, relays fine, and silently stops `/health` counting. Both `main.go` and `call.go` wrap the right way round; copy that shape.
     - Empty result gets its own error naming the config, because "no endpoint" reads as "the network has no suppliers" and sends the operator looking in the wrong place. It still wraps `domain.ErrNoEndpoint`.
     - **A one-entry allowlist removes failover.** Documented in the schema, README and config example — it is the operator's call, but it must not be a surprise.
     - **HOST matching is the second axis (`allow_hosts`/`deny_hosts`, `X-Pocket-Allow-Hosts`/`X-Pocket-Deny-Hosts`, 2026-08-06). It is NOT a coarser address list — do not merge the two.**
       - **A host is a different key, not a finer one.** Several suppliers routinely share one URL, because one relay-miner operator runs many supplier stakes behind it: on beta *all 32* `pnf-pocket-beta` suppliers answer on `rm.beta.infra.pocket.network` (which is why one keepalive pool serves them — see the Latency section), and all 15 mainnet gRPC services advertise one and the same operator's host. So **"route away from this operator" is expressible ONLY by host** — the address set behind a host is session-scoped and cannot be enumerated in advance — and **"drop this one supplier" only by address.** Neither subsumes the other.
       - **Separate fields, never one mixed list.** In a mixed list an entry's meaning would depend on whether it happened to parse as an address. Instead each list **rejects the other's content**: `domain.ValidateSupplierAddr` refuses a host, `domain.ValidateHostPattern` refuses a URL *and* a `pokt1…`. A paste into the wrong list is a startup error or a 400, never a list that quietly matches nothing.
       - **⚠️ Match on the PARSED host[:port], never a substring of the URL.** A substring denylist fails **open** on `https://evil.example/rm.beta.infra.pocket.network` — failing open is the wrong direction for a deny. `TestSupplierPolicy_HostMatching` runs every case through **both** an allowlist and a denylist for this reason; a matcher that is wrong in one direction only would otherwise pass.
       - **The scheme's default port is filled in** (`https`/`wss` → 443, `http`/`ws` → 80). Suppliers advertise `https://rm.beta.infra.pocket.network` with no port, so without this an operator who writes the `:443` they can see in a browser matches nothing. A pattern with no port matches any port; a pattern with one is exact.
       - **`*.example.com` is subdomains, NOT the apex** — the conventional reading. Someone who wants both writes both.
       - **An unparseable URL fails CLOSED when a host policy is in force.** We cannot check it against the policy, and an operator who named the hosts they will pay must not be routed to one we could not identify. It would fail to dial anyway.
       - **An endpoint with no URL for this rpc type is passed through untouched**, not host-judged: it is about to be dropped for not supporting the type, and that is the Selector's call. Judging it here would make the diagnostics blame the host policy for an rpc-type mismatch.
       - **`PermitsEndpoint` is what the Filter calls**, not `Permits`: both axes, one place. `Permits` (address only) stays because it is meaningful alone and is what the address tests pin.

   If someone re-proposes this: the argument for it is the "blocks apart" staleness problem (a supplier N blocks behind head serves stale data, and failover cannot see that because the response is *valid*). That is real. It is also **SAGE's problem to solve**, and solving it here would need an active per-service height probe — a whole subsystem — because parsing relay responses would break the opaque-passthrough rule.
3. **~~SSE / NDJSON streaming~~ DONE (2026-07-17) — CONFIRMED WORKING against a real supplier by a team member, 2026-07-27.** `relay.RelayStream` + `relay.StreamSender` + `pocket.HTTPSender.SendStream` + `transport.HTTP.serveStream`.
   - **It works. What is missing is COVERAGE, not confidence.** A team member relayed a real stream through this path and confirmed it. Treat the feature as working. What this repo still lacks is a *recorded* run — no request/response pair in the Validated section, and no live test that exercises it — because no inference service reachable from our beta app is staked. **The practical consequence is regression risk: if the streaming path breaks tomorrow, nothing here catches it.** The unit tests cover the delimiter splitting and per-batch validation; they cannot cover the miner actually batching a live token stream. Point `call` at an inference service the moment one is reachable and write the run down. Original finding below, kept because the reasoning is the spec: Investigated 2026-07-17 against `../pocket-relay-miner`, **which is the miner deployed on beta** (no service configured for it yet).
   - **It rides the HTTP path we already use.** No new RPC type: the five backend types map 1:1 to the on-chain enum, and SSE is just a REST relay whose *response* streams. The miner decides from the **backend's** `Content-Type` — `isStreamingResponse` (`relayer/proxy.go:1974`) matches `text/event-stream` and `application/x-ndjson`.
   - **The wire format:** the miner reads the backend stream, batches chunks (100ms / 100KB / 100 chunks — `relayer/http_stream.go:20-35`), **signs each batch**, and writes them into one long-lived response separated by the literal `||POKT_STREAM||`. So one request → one response → **N signed RelayResponses in the body**.
   - **Why we broke (fixed):** `Send` does `io.ReadAll`, then `ValidateResponse` tried to unmarshal the whole delimited blob as ONE RelayResponse. `SendStream` returns the body still open and `RelayStream` splits it.
   - **The HTTP front now ALWAYS streams** (`Options.Stream`), because only the response can say whether it is a stream. A non-streaming response is one batch and comes out byte-identical — verified live on beta.
   - **`StreamDelimiter` is the miner's constant, not ours.** A test pins the literal `||POKT_STREAM||`: every other test uses the symbol, so changing it would keep the suite green while silently breaking every real stream.
   - **`streamClient` has no Timeout** (`pocket/sender.go`) — deliberately. The normal client's whole-request deadline would sever a token stream mid-answer; streaming lifetime belongs to ctx.
   - **Failover stops at first delivery.** Once a batch reaches the client we are committed: continuing on another supplier would splice two token streams together invisibly.
   - **How to detect it:** `HandleStreamingResponse` copies the backend's headers onto the miner's response (`http_stream.go:175-177`), so `Content-Type: text/event-stream` (or `application/x-ndjson`) on the *miner's* reply means "the body is delimited batches".
   - **Reference client to mirror:** `cmd/relay/stream.go:243-300` — a `bufio.Scanner` with a split on the delimiter, 256KB buffer ("LLM responses can have large chunks"). Each token is a **complete marshaled RelayResponse**, verified independently, so `ValidateResponse` works per batch with no change.
   - **The gotcha they document (`cmd/relay/stream.go:232-240`):** a delimiter-terminated batch is whole. A *trailing* token with no delimiter is either a complete final batch (clean EOF — keep it) or a truncated protobuf (reader error/timeout — drop it, or its failed signature check discards every valid batch before it). Get this wrong and long streams either lose data or blow up at the end.
   - **What we would need:** the `relay.Sender` seam returns `[]byte`, which cannot express a stream — same shape of mismatch as WebSocket. So a streaming flow beside `Relayer`/`Bridge`: read the body incrementally, split on the delimiter, validate + unwrap each batch, and **write+flush to the client per batch** (`transport/http.go` must flush, or tokens arrive in one lump and the 100ms batching was pointless).
   - **Why it matters more than gRPC:** this is the LLM token-streaming path, beta already has a `text-generation` service, and it needs no new transport, no h2c, and no trailer games.

4. **~~REST / gRPC passthrough testing~~ DONE 2026-07-22 — all five transports live on beta.** This item read "BLOCKED ON BETA, do not retry there" and said the only way forward was a mainnet app stake with real POKT. **That is obsolete.** Beta gained `pnf-pocket-beta` — the Pocket beta chain relayed through PNF's own suppliers — with **32 suppliers × all five RPC types**:

   ```sh
   pocketd query supplier list-suppliers --network=beta -o json | jq -c \
     '[.supplier[]?|.services[]?|select(.service_id=="pnf-pocket-beta")|.endpoints[]?.rpc_type]|group_by(.)|map({t:.[0],n:length})'
   # [{"t":"COMET_BFT","n":32},{"t":"GRPC","n":32},{"t":"JSON_RPC","n":32},{"t":"REST","n":32},{"t":"WEBSOCKET","n":32}]
   ```

   **The app is `pokt1cxaxhx5hc347svmrtupzyuw6cfu5k6jg6sw76d`** — staked 2000 POKT for `pnf-pocket-beta`, delegated to the pnf-beta gateway. PNF holds its key; ask for it rather than looking for a copy, and put it in `local/` or `POCKET_APP_PRIVATE_KEY`. It is an **app** key, which is what pocket-ap's sovereign model needs — delegation does not matter, the app is always a ring member. `local/beta-config.yaml` (gitignored) wires it to one listener per RPC type on 8545-8549.

   `pnf-anvil` is still `JSON_RPC`-only, and our old app (`pokt1e3scnf3t…`) is staked for it alone — which is why four relay paths went untested for a week. **Use `pnf-pocket-beta` for anything that is not EVM-specific.** Note it is a *Cosmos* chain: `eth_chainId` correctly returns "Method not found" there.

   Re-check with: `pocketd query supplier list-suppliers --network=<net> -o json | jq '[.supplier[]?|.services[]?|.endpoints[]?.rpc_type]|group_by(.)|map({type:.[0],count:length})'`
5. **~~Distribution~~ SHIPPED 2026-08-20 — `v0.1.2` is live on GitHub, Homebrew and ghcr (`v0.1.1` the same day was the first with artifacts). npm is built and deliberately deferred.**
   - **Verified end to end, not just green:** `brew tap pokt-network/tap` + `brew install pocket-ap` puts a binary on PATH that completed a **real signed relay** against beta `pnf-pocket-beta` — 32 endpoints, first attempt, 327ms. The formula's four `sha256` values were also diffed against the release's own `checksums.txt` (4/4 match), because a formula that installs cleanly can still point at the wrong artifact.
   - **⚠️ `v0.1.0` IS A TAG WITH NO RELEASE. Do not "fix" it by moving it.** It was tagged, its release failed (below), and by then `proxy.golang.org` had already cached it — `/@v/v0.1.0.info` returns 200. A cached tag's content is immutable: re-pointing it hands anyone who already fetched it a checksum mismatch. **The next version number is always the answer; a published tag never moves.** This also makes tag-pushing an expensive way to test anything, which is why the credential check below is dispatchable.
   - **⚠️ `gh` in Actions authenticates from `GH_TOKEN` — and nothing else.** The release preflight exported the tap token as `HOMEBREW_TAP_GITHUB_TOKEN` only, so `gh api` ran with **no credential at all** and died before reaching GitHub; `2>/dev/null || echo "unreachable"` then discarded the reason and the script printed its own hardcoded guess: that the token could not see the tap. The token could push the whole time. **That is what killed `v0.1.0`, and it burned a version number on a diagnosis that was fiction.** Two rules fall out of it: point `GH_TOKEN` at the credential the step exists to test, and never swallow the stderr of the tool whose answer you are reporting — a failure branch's guess reads exactly like evidence.
   - **`.github/workflows/tap-token-check.yml` is dispatchable AND is the release's preflight** (`uses:` + `secrets: inherit`). One copy, deliberately: two copies of a check is two things to get wrong, and the copy that never runs on its own is the one that rots. Run it with `gh workflow run tap-token-check.yml` before tagging — it costs a 10s job instead of a version number.
   - **Done and testable without a tag:** `pocket-ap version` (ldflags-stamped, goreleaser's default var names), `make dist` (cross-compiles all 5 targets), `.goreleaser.yaml`, `Dockerfile`. Verify with `goreleaser release --snapshot --clean`.
     - **A `v*` tag triggers a real release.** `.github/workflows/release.yml` runs goreleaser: five binaries, `.deb`/`.rpm`/`.apk`, GitHub release, formula push, ghcr images. Tag deliberately.
     - **The tap needs its own credential** — `HOMEBREW_TAP_GITHUB_TOKEN`, an org secret shared with this repo. `GITHUB_TOKEN` is scoped to this repository and cannot write to the tap. The preflight checks `.permissions.push` **before** anything publishes, because goreleaser pushes the formula *after* the release is out, and discovering a bad token there is the worst possible moment.
     - **Homebrew 6 refuses third-party taps until trusted** — `brew install` fails with `Refusing to load formula … from untrusted tap` until `brew trust`. It is in the README install snippet as `brew trust --formula pokt-network/tap/pocket-ap`, the narrow grant, rather than `brew trust pokt-network/tap`, which covers every formula the tap ever carries. **A two-line install snippet is now wrong**; do not "simplify" it back.
     - **`brews:` is DEPRECATED in goreleaser** (`homebrew_casks` replaces it). 2.17.1 still writes a working formula and only warns, but `goreleaser check` exits non-zero over it, and CI pins the floating `version: "~> v2"` — so a v2.x that drops it would fail the release at config parse. It fails *before* publishing, so the cost is a failed run, not a broken release.
     - **npm is BUILT and DEFERRED (decided 2026-08-20) — do not pick it up without a reason from the list in `npm/README.md`.** The scaffolding is written and verified against a real build; what is missing is a home and a demand, not work. **The deciding argument is that the publishing identity is unsettled, not that the work is hard** — a package name is permanent and so is whoever can publish it, so **ask internally rather than assuming a scope or account is available**. The secondary ones: pocket-ap is a **daemon, not a library**, so nobody puts it in `dependencies` (brew, tarballs and `docker run` already serve the dev-tool case; `npx pocket-ap call …` is the only thing npm genuinely adds), ~104 MB per platform lands in `node_modules`, and it is six version-locked packages per release. Revisit once the publishing identity is settled, or if a real user asks — the demand today is **inferred, not reported**. **The shape is optionalDependencies, NOT a postinstall download — do not "simplify" it back.** A wrapper package (`pocket-ap`, unscoped, what people type) declares one payload package per platform (`<scope>/pocket-ap-<os>-<cpu>`) as optional deps with `os`/`cpu` set, so npm installs exactly one. A postinstall that fetches from the GitHub release is the obvious alternative and has three failure modes this does not: it breaks under `--ignore-scripts` (pnpm's default, and enforced in many companies), it needs install-time network so it fails offline and behind proxies, and it has to verify `checksums.txt` itself or an install trusts the network completely.
       - **The shim uses `spawn`, not `spawnSync`** — `serve` is long-lived, and a blocked parent cannot forward a signal. Under Docker or systemd the stop signal lands on node; not forwarding it turns shutdown into a timeout then a kill, skipping the proxy's own shutdown path.
       - **`generate.mjs` reads `dist/artifacts.json`, never a glob of `dist/`.** Directory names carry GOAMD64/GOARM64 suffixes (`pocket-ap_linux_arm64_v8.0`) that move with build settings, and a glob matching nothing would publish an empty package.
       - **Platform packages publish BEFORE the wrapper.** The wrapper pins them by exact version, so the other order opens a window where an install resolves the wrapper and finds no binary.
       - The publish job **cannot fail a release**: with no `NPM_TOKEN` it reports a skip. `npm-token-check.yml` is the dispatchable credential probe — the lesson from the tap token, which was first exercised by tagging and cost a version number.
       - `TestNPMTargetsMatchGoreleaser` checks the platform list **both ways** against `.goreleaser.yaml`. ⚠️ Its first version searched the whole shim file and passed against a deleted entry, because every platform name also appears in the comment explaining the Go/Node naming mismatch — it now slices the `PACKAGES` table alone. Node disagrees with Go on two of five names (`amd64`→`x64`, `windows`→`win32`), which is why this is checked and not eyeballed.
     - LICENSE is done (MIT, matching poktroll/PATH/relay-miner).
   - **The Go build cache is SHARED between `main` and the tag-triggered release, and that direction is the whole trick.** goreleaser spent ~11m40s per release compiling cosmos-sdk / cometbft / go-ethereum cold for four of five targets. Two things had to be true, and neither is obvious:
     - **`setup-go`'s cache actively could not help.** It derives one key from (os, go version, `go.sum`), shares it across every job, and **saves only on a miss** — so the release restored CI's *host-architecture* cache, was told "hit", and therefore never stored the cross-architecture objects that made it slow. Both workflows now set `cache: false` and manage `actions/cache` themselves.
     - **A tag run CAN restore a cache saved on the default branch.** GitHub's docs are ambiguous here and one reading says the opposite, which would make the whole thing a no-op. **Measured with a throwaway probe workflow and a non-release tag** (`hit=true`) rather than reasoned about — the probe was deleted once it had answered. A cache saved from a *pull request* is scoped to that PR and warms nothing, which is why `ci.yml`'s `crossbuild` job is `push`-only.
     - `make dist` populates it, using the same `CGO_ENABLED=0` and `-trimpath` as goreleaser so the objects are the ones it reuses; differing `-ldflags` only affect the link. Measured: **10m48s cold, 18s warm** (plus ~38s to restore 2.2 GB). Every push also refreshes the entry's access time, which is what defeats GitHub's 7-day eviction between releases. ⚠️ **Keep the two `key:` values identical** — they are in different files and nothing checks them.
     - It pays a second way: `windows/amd64` used to be built for the first time *during a release*, on a tag that cannot be moved if it breaks.
   - **The repo is public and every commit ships.** No third-party names in tracked files (attribute neutrally or link the public issue), and no key material, ever. `TestNoSecretsInTrackedFiles` is the backstop, not the policy.
   - **Sizes are the constraint.** The 929-module cosmos/cometbft/geth tree builds a **153 MB** binary; `-s -w` takes it to **102 MB**, ~30 MB gzipped (measured 2026-08-19, darwin/arm64). Stripping is not optional — it is what makes npm/brew tolerable. Every build path sets it, **including `make build` as of 2026-08-19**: this line claimed that while `make build` was the one path that did not, so the binary a developer tested was 50 MB unlike the one that shipped. `-s -w` drops the symbol table and DWARF, so `dlv` cannot debug a `make build` binary — build without `-ldflags` for that. **Panic stack traces are unaffected** and this was verified rather than assumed: Go builds them from the pclntab, which stripping keeps, so `internal/safego`'s `debug.Stack()` still names every frame in a stripped binary.
   - **All 5 targets cross-compile clean** (darwin arm64/amd64, linux amd64/arm64, windows amd64) — verified, no CGO despite geth in the tree.
   - **Coverage:** npm covers the target user (JS/TS devs, every OS); brew covers macOS **and Linux**; tarballs+docker cover servers/CI/containers. That is effectively everything.
   - **Official apt/Debian is a non-starter — do not attempt it.** Debian packages every dependency separately; that is 929 source packages plus a sponsor plus a ~2-year freeze cycle for software tracking a live chain. Nobody in this class (Docker, Grafana, HashiCorp, kubectl) is in official apt; they all self-host a signed repo. `.deb`/`.rpm`/`.apk` for `dpkg -i` are already in the goreleaser config — that is the 95% answer.
6. **~~`/health`~~ DONE (2026-07-16) — Prometheus `/metrics` deliberately deferred.** `health/` serves `GET /pocket-ap/health` → 200 `ok` / 503 `degraded`, opt-in via `admin.addr` (empty = no listener; `call` never starts one).
   - **Own port, never a relay listener.** `transport.HTTP` proxies every path it gets (`mux.HandleFunc("/", …)`), so mounting health there would steal a route from the proxied service — a Cosmos REST backend really serves `/health`. The path is namespaced for the same reason plus log clarity and ingress-safety; a bare `/health` 404s with a hint rather than aliasing.
   - **Health signal = block-height staleness**, not a synthetic ping. A failing poller is the leading indicator of breakage: height goes stale → `Session()` stops noticing expiry → relays hit dead sessions. Degraded past `3 × blockPollInterval`. `SessionManager.PollState()` exists for this; `pollBlockHeight` used to swallow the error entirely.
   - **Never calls the full node.** Probes are frequent; a health check that dialled out would turn every prober into load on the node. It reports what the poller already knows.
   - **503 = readiness, NOT liveness.** Restarting will not fix an unreachable full node — wiring this to a k8s liveness probe produces a restart loop during an upstream outage.
   - **`recovered_panics`** was added 2026-08-19 alongside `internal/safego` — see that section for why a contained panic still has to be visible somewhere. It does not affect the ok/degraded verdict.
   - **Nothing is stored.** Counters are in-memory, per-process, reset on restart, never written or exported. A metrics *store* would violate the "observation pipeline lives in SAGE" rule — do not add one. Prometheus `/metrics` is deferred on purpose: `client_golang` is only an **indirect** dep today, and making it direct is a real cost for a proxy people run in a terminal. The counters are ready if a scraper ever justifies it.
   - **`relay.WithObservers`** exists because the `Relayer` takes its Observer from the Selector alone — health counters would otherwise occupy the seat a QoS selector needs. It fans out and preserves a self-observing Selector's own feed. **Use it to attach any future Observer.**
7. **~~Multi-app~~ DONE and LIVE-VERIFIED on beta 2026-08-04 — app rotation still open.** Built after external user feedback reported that "multi-service" was unusable: several listeners with different `service_id`s were always accepted, but one key can only ever serve one of them, so the feature was decoration. **That report was right, and the protocol says why.**
   - **An app stakes for EXACTLY ONE service.** poktroll `x/shared/types/service_configs.go` `ValidateAppServiceConfigs`: `if len(services) != 1 { "application must have exactly one service" }`. **Do not "add" multi-service to an app** — it is rejected onchain, and the objection to changing that upstream is the right one: it would worsen the stake fragmentation sessions already suffer. So **multi-service is spelled multi-app**, and that is not a workaround, it is the shape of the protocol. Related upstream discoverability issue: https://github.com/pokt-network/poktroll/issues/1994.
   - **`service_id` is therefore derivable, and is now optional everywhere.** key → app addr → `GetApp` → `ServiceConfigs[0].ServiceId`. `pocket.Signer.ServiceID` reads it **through the signer's own `appCache`**, which is the same entry `SignRelay` needs to build the ring — so discovery costs no extra round trip. A configured `service_id` is kept as an *assertion*: startup compares and **fails on mismatch**. That is the honest version of the "double check for people new to Pocket" rationale the required field used to serve — it now catches a wrong key instead of just being typed twice.
   - **Config: `app:` (single) or `apps:` (list), never both** — both would make "which key signs" depend on load order. `POCKET_APP_PRIVATE_KEYS` (comma-separated) is the multi-app env form, read **only when the config names no app at all**, or a machine-wide export would silently add apps to every config on the box.
   - **The relay core did not change.** `MultiSigner` (`pocket/apps.go`) routes on `session.AppAddr` — every seam already takes a session, and a session already knows its app. `SessionManager` swapped one `appAddr` for `map[serviceID]appAddr`; its cache key was already `serviceID:appAddr`, so nothing else moved.
   - **`relay.Bridge.AppAddr` is GONE — the handshake now reads `session.AppAddr`.** A single configured address would be wrong for every app but one, and the miner rejects a handshake whose `App-Address` is not the app that signs the frames. Taking it from the session makes the mismatch unrepresentable.
   - **`/health` reports `apps: [{address, service_id}]`, replacing `app_address`.** Breaking JSON change, deliberate: with several apps there is no "the" app, and the pairing is what an operator needs.
   - **Two apps on ONE service is rejected** (`NewSessionManager`). That is stake rotation — the remaining half of this item — and picking one silently would send half the traffic to an app the operator thinks is configured.
8. **edge/wasm SDK** — only if edge demand appears. Must swap gRPC → cosmos REST/LCD (gRPC's HTTP/2 trailers can't run on edge/browser).
9. Minor: `SessionManager.Stop()` exists but is unused — the poller exits on ctx cancel at shutdown, which is sufficient.

## Validated

**2026-08-20 — FIRST PUBLIC RELEASE, and the Homebrew path proven on a real machine.** `v0.1.1` (`v0.1.0` shipped nothing — see roadmap 5).

| what | result |
| --- | --- |
| GitHub release | 12 assets: 5 archives, 6 Linux packages, `checksums.txt` |
| downloaded archive vs `checksums.txt` | `pocket-ap_darwin_arm64.tar.gz: OK` |
| version stamp in the shipped binary | `pocket-ap 0.1.1 (commit 136b0dd, ... darwin/arm64, go1.26.6)` — tag and commit both land |
| size | **104 MB** stripped, matching the ~102 MB measured on 2026-08-19 |
| formula `sha256` vs the release's own checksums | **4/4 match** — a formula can install cleanly and still point at the wrong artifact |
| `brew install pocket-ap` | installed, caveats (key handling, loopback) displayed |
| `brew test pocket-ap` | passed |
| **relay through the brew-installed binary** | beta `pnf-pocket-beta`, `32 in session, 32 support comet_bft`, **attempt 1 ok in 327ms** |

That last row is the whole point: what a user gets from `brew install` completes a signed relay against a live network, which no amount of green CI proves on its own.

Two findings, both now in the README and roadmap 5: **Homebrew 6 blocks untrusted third-party taps**, so the install snippet needs a `brew trust` line; and `brew audit` reports one cosmetic complaint (`version 0.1.1 is redundant with version scanned from URL`) about a file goreleaser generates, so fixing it needs a template override and is not worth one.

**Beta TestNet, 2026-08-19 — ONBOARDING, run from zero.** A brand-new app, staked from nothing, to answer "are the docs enough for a machine to use Pocket Network?" They were **not**: no doc mentioned `pocketd keys add`, `stake-application`, a stake config, or key export. The README and AGENTS.md sections that now exist are a transcript of this run, not a reconstruction.

App `pokt1qvhsmxpv73knshu4fkcq3j48vvx8qnjzxkzc0h`, staked 1000 POKT (exactly `min_stake`) for `pnf-pocket-beta`, key held in a throwaway keyring under the scratchpad.

| step | result |
| --- | --- |
| `keys add` → faucet → `stake-application` → `keys export` → relay | worked end to end, first relay `attempt 1 … ok` in 467ms |
| **`delegatee_gateway_addresses: []`** | **the sovereign model with NO gateway delegation at all** — stronger than the PNF app, which is delegated. The app is always a member of its own ring, so a bare stake suffices. |
| `service_id` configured | none — derived from the key, as designed |
| config used | `config.example.yaml` verbatim with only the two `fullnode` lines switched to beta |

Three things this run found, all now documented:
- **`--fees` is required on the stake tx**, and omitting it returns ~40 lines of Go stack trace whose last six words are the message: `insufficient fees; got:  required: 1upokt`.
- **`keys export --unarmored-hex --unsafe` is the join between the two tools** and was documented nowhere. pocket-ap wants raw hex; the keyring stores armored. An agent that got through staking would still stall here. Needs `--yes` or it hangs a script on a prompt.
- **`pocketd faucet fund` is dead on ALL THREE networks** — `shannon-testnet-grove-faucet.{alpha,beta}.poktroll.com` and `shannon-grove-faucet.mainnet.poktroll.com` are NXDOMAIN (`poktroll app/pocket/networks.go:53-55`), and the flag its own `--help` names for overriding the base URL, `--faucet-base-url`, does not exist. The working beta faucet is a browser page at `faucet.beta.testnet.pokt.network`, which does not speak the `POST /{denom}/{address}` route that command expects. **Owed upstream.**

**Beta TestNet, 2026-08-19 — the SAGE/PATH catch-up, live.** Service `pnf-pocket-beta`, config `local/beta-config.yaml`, after the `poktroll v0.1.35` / `go 1.26.6` bump. The dep bump is why this run mattered: a shannon-sdk bump is what removed `SignWithRing` last time, so signing had to be re-proved on the wire and not just in the protobuf diff.

| what | result |
| --- | --- |
| `call` comet_bft `GET /status` | `pocket-lego-testnet` height 573020, **first attempt, no failover**, 395ms — ring signing intact on v0.1.35 |
| `serve` WebSocket `:8548` | ack + 8.5KB NewBlock, **text** frames — raw-frame signing intact, miner accepted the handshake |
| **session rollover close code** | client received **1012 `"session ended, please reconnect"`** — the half that must NOT change, unchanged |
| miner's side of that close | **no `bad close code` anywhere in the run** — the miner accepted the 1001 it now gets |
| `/health` `recovered_panics` | **absent** across the whole run, i.e. zero. `omitempty` doing its job |

⚠️ **The 1001 the miner receives is not directly observable from here** — the assertion that it gets 1001 while the client gets 1012 lives in `TestBridge_EachPeerGetsACloseCodeThatFitsItsRole`, which reads both peers over real sockets. What the live run adds is that the miner *accepts* it and that the client side did not regress. `local/wsclose/` (gitignored) is the probe: `local/wsprobe` reads exactly two frames and exits, so it cannot see a close code.

**Beta TestNet, 2026-08-06 — PER-REQUEST SUPPLIER LISTS via header, live.** Service `pnf-pocket-beta`, 32 suppliers, through **both** front doors. Supplier **A** = `pokt1dap7pc…z32l`, **B** = `pokt1h4zy9d3…00da`.

| what | result |
| --- | --- |
| `X-Pocket-Allow-Suppliers: A` (call) | `32 in session, 1 support comet_bft`; A answered 3/3 |
| `X-Pocket-Deny-Suppliers: A` (call) | `32 in session, 31 support comet_bft`; 5 runs, 5 different suppliers, **never A** |
| **narrow-only, config ceiling = A** | header asks for **B** → refused, `config allow/deny 1/0; request allow/deny 1/0`. **A header cannot widen the config.** |
| header asks for **A+B**, ceiling = A | intersects to A alone and relays — narrowing works while widening does not, in the same request |
| header denies A, ceiling = A | refused with `request allow/deny 0/1` — the error names **which** list emptied the set |
| `serve` json_rpc `:8545` + header | 3 pinned relays, 3 successes |
| `serve` WebSocket `:8548` + header | bridge opened on **A** (`supplier=pokt1dap7pc…` in the log), ack + 8.5KB NewBlock, **text** frames |
| malformed value, both listeners | **400** on HTTP and on the WS handshake; `/health` `attempts` stayed at **3** — a refused request costs **zero relays** |
| `/health` still counting | yes — proof `Filter` is still wired INSIDE `WithObservers` |

**Same day — the HOST axis, live.** This is the axis an address list cannot express, and beta is the proof: all 32 suppliers answer on one host, so one entry moves all 32.

| what | result |
| --- | --- |
| `X-Pocket-Deny-Hosts: rm.beta.infra.pocket.network` | **whole service unroutable** — `hosts 0/1`, 32 suppliers gone on one entry. No address list can do this. |
| `X-Pocket-Deny-Hosts: *.infra.pocket.network` | same — wildcard matches the subdomain |
| `X-Pocket-Allow-Hosts: rm.beta.infra.pocket.network` | all 32 kept, relay ok |
| `…:443` (port the URL leaves implicit) | all 32 kept — **the scheme-default port fill-in works on a real supplier URL** |
| `…:8545` (wrong port) | `0 support comet_bft` — a named port is exact |
| host axis + address axis together | `32 in session, 1 support` — both applied, ANDed |
| config `deny_hosts` + header `allow_hosts` for the same host | still refused (`config hosts 0/1; request hosts 1/0`) — **narrow-only holds on the host axis too** |
| URL in a host list | refused everywhere: startup (`config: apps[0] supplier host … is a URL, not a host`), `call`, HTTP **400**, WS handshake **400** |
| operator address in a host list | refused, naming the list it belongs in |
| `serve` HTTP + WS with `allow_hosts` | relay ok; WS ack + 8.5KB NewBlock, text frames |
| `serve` HTTP with `deny_hosts` | **502** with no supplier left; `/health` `attempts` unchanged — zero relays |

`local/wsprobe` gained optional trailing `"Name: value"` args for this (it dialled with `nil` headers before).

**Beta TestNet, 2026-08-04 — MULTI-APP, SERVICE DISCOVERY and SUPPLIER ALLOW/DENY, live.** Two staked apps in ONE process, six listeners, config `local/beta-config.yaml`.

| what | result |
| --- | --- |
| service discovery | **neither app configured `service_id`** — both derived from their key at startup: `pokt1cxaxhx5…` → `pnf-pocket-beta`, `pokt1e3scnf3t…` → `pnf-anvil` |
| two apps, one process | `pocket-ap up listeners=6 apps=2 services="[pnf-pocket-beta pnf-anvil]"` |
| relay via app 0 (:8545) | CometBFT `status` → `pocket-lego-testnet` |
| relay via app 1 (:8550) | `eth_chainId` → `0x7a69` (anvil) — a **different app, key and session** in the same process |
| session isolation | `/health` showed both apps paired with their services, **separate sessions and endpoint sets** (32 vs 38) |
| WebSocket (:8548) | ack + 8.5KB NewBlock, **text** frames — `App-Address` now comes from `session.AppAddr` and the miner accepted the handshake |
| allowlist (1 of 32) | `32 in session, 1 support json_rpc`; the same supplier answered 4/4 |
| denylist (1 of 32) | `32 in session, 31 support json_rpc`; the denied supplier never reappeared in 5 runs |
| policy isolation | app 1 has no policy and stayed at `38 in session, 38 support json_rpc` — app 0's list did not leak |

Two things fell out of the run, neither a code change: the relay miner's WS **text**-frame fix is confirmed deployed (this is what the frame-type section predicted), and `pnf-anvil` still advertises a supplier at `us-east-rm-01-pnf-anvil-json.test.com`, which does not resolve — failover handled it (`attempts 2, successes 1, failures 1`), so a dead advertised host is visible in `/health` rather than fatal.

`local/wsprobe/` (gitignored) is the WS client used here. `websocat` closes on stdin EOF, which races the first response and prints nothing — do not conclude the bridge is broken from that.

**Beta TestNet, 2026-07-22 — ALL FIVE TRANSPORTS, live, service `pnf-pocket-beta` (32 suppliers, chain `pocket-lego-testnet`).** Config: `local/beta-config.yaml`. Four of these paths had never completed a real relay before this run.

| transport | request | result |
| --- | --- | --- |
| `json_rpc` | `status` | `pocket-lego-testnet`, height 493116 — 486ms |
| `rest` | `GET /cosmos/bank/v1beta1/params` | `{"params":{"send_enabled":[],"default_send_enabled":true}}` — 495ms |
| `comet_bft` | `GET /block?height=493100` | full block — **path AND query preserved** — 332ms |
| `websocket` | `subscribe tm.event='NewBlock'` | ack + 8.5KB NewBlock event, then a clean 1012 rollover |
| `grpc` | `/cosmos.bank.v1beta1.Query/Params` | `default_send_enabled: true` — same answer as REST — 837ms |

All first-attempt, no failover: health reported `attempts 4, successes 4, failures 0`. Exercised through **both** front doors — `call` one-shot and a running `serve` with five listeners (the gRPC listener answered a `--http2-prior-knowledge` client over h2c with HTTP/2 200). Two live findings came out of it, both recorded above: gRPC needs the **gRPC-Web fallback** on beta, and the miner still forces **binary** WS frames.

### Latency — `call`'s numbers are cold-start, do not read them as the proxy's cost

Measured beta, 2026-07-22, from Europe. **Warm through `serve` (n=40): p50 97ms, p95 178ms, min 60ms.**

The per-attempt latency `call -v` prints (332-928ms above) is **`Send` only, from a one-shot process that has no connection to reuse**. Every `call` invocation pays DNS + TCP + TLS to the relay miner from scratch:

| | |
| --- | --- |
| DNS | ~3ms |
| TCP handshake complete | ~55ms ← **one RTT to beta infra** |
| TLS handshake complete | ~110ms |
| first `serve` request (cold dial) | 2.09s |
| subsequent `serve` requests | p50 97ms |

So ~110ms is gone before a request byte moves, and ~55ms of every relay is pure distance that nothing in this repo can remove. For scale, a plain `curl` straight to `sauron-rpc.beta…` — no Pocket at all — is 166ms per request with a fresh connection each time, of which 112ms is TLS. **A warm relay through Pocket is cheaper than a cold direct request to the same infrastructure.** Pocket's own share (miner hop + ring-signature verification + backend) is roughly 40ms.

Two consequences worth knowing:
- **Do not benchmark with `call`.** It exists to debug one relay and it says so; a fresh session fetch and a cold TLS handshake dominate its numbers.
- On beta every supplier resolves to the same `rm.beta.infra.pocket.network`, so one keepalive pool serves all 32 and the cold dial is paid once. On mainnet, suppliers are distinct hosts and `selector.Random` spreads across them — so the ~110ms connection setup is paid per host, and a low-traffic proxy will pay it often. **If that ever surfaces as a complaint:** the prize is bounded at one RTT (TCP's is irreducible), the crossover is ~1 rps, and the likely-correct answer is TLS resumption + transport tuning, *not* a stateful Selector. Don't build a stateful Selector to chase it.

Beta TestNet, 2026-07-15: `eth_blockNumber` → `0x0`, `eth_chainId` → `0x7a69` (anvil), HTTP 200, 37 endpoints in session, service `pnf-anvil`. Signed with the app's own key under gateway delegation — the core thesis, confirmed live.

Beta TestNet, 2026-07-16 (`call` mode): `eth_blockNumber` → `0x0`, `eth_chainId` → `0x7a69`, `web3_clientVersion` → `anvil/v1.5.1`. 37/37 endpoints support `json_rpc`; first-attempt success at ~260-300ms; `selector.Random` observed picking a different supplier per invocation. All three `--compare` tiers exercised (local echo for `identical`/`equivalent`, cloudflare-eth for `differ`). Race-detector clean.

**Tests** (2026-07-16, 89 cases, `-race` clean): `relay` / `config` / `domain` / `selector` at 100% statement coverage; `pocket` at 9.1% (`sender.go` + `buildTargetURL` — the rest needs a live full node, see below). Every test was **mutation-checked**: 11 deliberate bugs were injected (wrong Rpc-Type enum, no failover, ignored MaxAttempts, `KnownFields(false)`, dropped rpc-type filter…) and all 11 were caught.

**`pocket` coverage and its ceiling.** A `nodeClient` seam (`fullnode.go`) now fakes the three methods that matter — `GetSession`, `GetApp`, `GetCurrentBlockHeight` — so the session cache, rotation, poll state and app cache are covered. `*FullNode` satisfies it, so callers were untouched.

It **stops there deliberately**: `AccountClient()` returns a concrete `*sdk.AccountClient`, and the ring signing runs through concrete SDK calls. Faking those means scaffolding around the one thing this file says not to touch, and the network verifies it on every relay. So `pocket` will not reach 100%, and chasing it is a mistake.

**`cmd/` and `transport/` have no tests.** `transport/http.go` is thin glue and `cmd/` is wiring; both are exercised live by `call`.
