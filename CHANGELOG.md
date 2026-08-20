# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project aims to
follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- **`network: beta|main` and a `--network` flag** on both `serve` and `call`. One
  key sets both full-node transports, so they cannot end up naming different
  chains — a config that fetches sessions from one chain and block height from
  another fails like an outage rather than like a typo. The flag overrides the
  file for a single run; `main` warns at startup that relays spend real POKT.
  `config.example.yaml` now ships `network: "beta"`, so the default costs
  nothing.
- The Homebrew formula installs `config.example.yaml` and `config.schema.yaml`
  into `pkgshare`. Brew was the only install path that discarded them, leaving
  its users with a binary and nothing to point `--config` at.

### Changed

- The container image **cross-compiles instead of emulating**. The build stage
  had no platform pin, so buildx compiled the whole dependency tree under QEMU
  for the non-native architecture: 39 minutes for `v0.1.1`'s `linux/arm64` leg.
  Image layers are also cached to the Actions cache now.

### Changed

- **Long flags are written `--config`, `--network`, `--service` and so on**, in
  the docs and in what `--help` prints. Go's `flag` package has always accepted
  one dash or two interchangeably, so **every existing single-dash invocation
  keeps working** — this is spelling, not parsing. One-letter flags (`-v`, `-H`,
  `-X`, `-d`) stay short, as convention expects.

### Fixed

- `pocket-ap --help`, `-h` and a bare `pocket-ap` now reach the top-level usage.
  `--help` used to hit the leading-flag rule and print `Usage of serve:`, hiding
  that `call` and `version` exist; a bare invocation failed with `config: read :
  open : no such file or directory`, an empty path in a message about a file.
- The Quickstart assumed a source checkout throughout, so a Homebrew or Docker
  user had no `config.example.yaml` to copy; `cp config.example.yaml
  local/config.yaml` also failed outright on a fresh clone, since `local/` is
  gitignored and therefore absent.
- The Quickstart's own verification call sent `eth_blockNumber` to
  `pnf-pocket-beta`, a Cosmos chain, so the first command a new user ran replied
  `Method not found` from a relay that had in fact worked.
- The docs said `config.example.yaml` pointed at Beta when it pointed at MainNet
  — the wrong direction to be wrong in, since MainNet relays spend real POKT.
- A `[Configuration](#configuration)` link that matched no heading.

## [0.1.1] - 2026-08-20

**This is the first release with artifacts.** It is `0.1.0` plus a release-workflow
fix; no relay, transport or configuration behaviour changed between the two.

`v0.1.0` was tagged and published nothing. Its release workflow checks that the
Homebrew tap credential can push *before* anything ships — goreleaser publishes
the GitHub release and only then pushes the formula, so a bad token would
otherwise fail at the one moment nothing can be undone. That check was itself
broken: it exported the tap token as `HOMEBREW_TAP_GITHUB_TOKEN` but `gh` reads
its credential from `GH_TOKEN`, so `gh api` ran with no credential at all, and
`2>/dev/null` discarded the reason. The check reported that the token could not
see the tap. The token could see the tap the whole time.

The `v0.1.0` tag is left in place rather than moved. The Go module proxy caches a
tag's content permanently, so re-pointing it would hand anyone who had already
fetched it a checksum mismatch.

### Fixed

- The release preflight authenticates `gh` with `GH_TOKEN` and keeps stderr, so a
  future failure reports what `gh` actually said instead of the script's guess.

### Added

- `tap-token-check.yml`, dispatchable on its own and called by the release
  workflow as its preflight. Pushing a tag was previously the only way to
  exercise the tap credential, and that is an expensive test: a version number
  spent on a failed release cannot be reused. One copy, so the dispatchable check
  and the one gating a real release cannot drift.

## [0.1.0] - 2026-08-19

First public release. Everything below is what ships in it.

**Status:** every relay path has completed a real signed relay against a live
network — not only a test suite. What is *not* yet proven is scale: this has
been exercised at debugging volumes, not production ones. Treat 0.1.0 as
"working and verified", not "battle-tested".

### Added — relay transports (all five Shannon RPC types)

- **JSON-RPC, REST, and CometBFT** over one transparent HTTP reverse-proxy adapter
  (`transport/http.go`). The payload is opaque bytes — no per-chain parsing.
- **WebSocket** streaming via a protocol-agnostic bridge (`websockets/`,
  `relay/bridge.go`, `transport/ws.go`): origin-checked, connection-capped, with
  close-code handling and per-bridge session-rollover handling. Close codes are
  both **sanitized** (RFC 6455 §7.4.1 reserves 1005/1006/1015 for local inference
  and forbids sending them) and **role-correct per peer**: the proxy is a server
  to its client but a client to the relay miner, so 1011/1012/1013 become 1001
  Going Away upstream rather than asking the miner to reconnect to us.
- **gRPC** relaying out (`pocket/grpc.go`), native with a **gRPC-Web fallback**
  (auto-detected per supplier host), plus a gRPC **listener** with h2c and
  trailer folding/unfolding (`transport/grpc.go`).
- **SSE / NDJSON streaming** relay path (`relay/stream.go`) for the inference use
  case, reading the relay miner's `||POKT_STREAM||`-delimited signed batches.

### Added — operation

- **Sovereign app-key signing.** The app address is derived from the configured
  key; relays are ring-signed with the app's own key, working even under gateway
  delegation.
- **Multi-app**: several staked apps in one process via `apps:` (or
  `POCKET_APP_PRIVATE_KEYS`), each with its own key, service, sessions and
  supplier policy. An application stakes for exactly one service (poktroll
  enforces it), so serving several services means several apps — one process
  now does it, where it previously took one process per service.
- **Service discovery from the key**: `service_id` is optional in `app`/`apps`
  and on listeners. It is read from the chain at startup; when configured, it is
  verified and a mismatch is a startup error instead of every relay failing with
  "session not found".
- **Supplier allow/deny lists** per app (`suppliers.allow` / `suppliers.deny`,
  `selector.Filter`). Static routing policy, not QoS — nothing measures or scores.
  `allow` is exhaustive (and therefore removes failover if it names one supplier);
  `deny` wins over `allow`.
- **`serve`** — the daemon, one listener per RPC type.
- **`call`** — one-shot relay for debugging, with `-v` diagnostics and `--compare`
  against a reference URL.
- **`version`** — ldflags-stamped build info.
- **Health endpoint** (`health/`) on its own opt-in admin port: `GET
  /pocket-ap/health`, block-height-staleness readiness signal, in-memory counters.
  Reports `apps: [{address, service_id}]` — there is no single "the" app.
- **Panic containment on detached goroutines** (`internal/safego`). `net/http`
  recovers a panic in the goroutine serving a request, but that stops at the
  goroutine boundary — and the WebSocket bridge processes every frame on its own
  goroutine, as do the block poller and each listener. One bad frame from one
  supplier could take down every listener and every app. Every `go` statement now
  runs its body under a recovery, and in the bridge the recovery is paired with a
  shutdown so a contained panic closes the connection instead of wedging it. The
  count is reported as `recovered_panics` in `/health` (absent when zero).
- **Selector outcome-feedback seam** (`relay.Observer`) — reports per-attempt
  results without deciding anything; `selector.Random` pays nothing for it.
- **Security defaults**: per-listener `allowed_origins` (browser origins rejected
  by default), `Host`-header check for DNS-rebinding (auto-enabled on loopback
  binds), local CORS preflight handling, and WebSocket `max_connections`.
- **Config** (`config/`): strict YAML (unknown fields error), value types, keys
  from `app.private_key_hex` / `apps[].private_key_hex` or
  `POCKET_APP_PRIVATE_KEY` / `POCKET_APP_PRIVATE_KEYS`, with a JSON Schema
  (`config.schema.yaml`) kept in sync by tests.

### Added — distribution

- `make dist` cross-compiles all five targets (darwin arm64/amd64, linux
  amd64/arm64, windows amd64), CGO-free and stripped.
- `.goreleaser.yaml`, `Dockerfile`, and `.deb`/`.rpm`/`.apk` packaging.

### Known limits

- **npm is not shipped.** Homebrew, the raw archives, the Linux packages and the
  container image are all published; the npm wrapper is not written yet.
- **Homebrew coverage depends on a formula, not a cask,** which is what keeps
  `brew install` working on Linux as well as macOS.
- **No load testing.** Latency was measured (p50 97ms warm, of which ~55ms is
  network distance to the test network) but throughput was not.
- **Streaming has no recorded live run.** SSE/NDJSON was confirmed working
  against a real supplier by a team member, and is unit-tested here, but no
  inference service reachable from the test app is staked, so there is no
  transcript and no regression test against a live stream.

### Validated

- **Beta TestNet, 2026-07-22** — all five transports completed a real signed relay
  on the first attempt against service `pnf-pocket-beta` (32 suppliers, chain
  `pocket-lego-testnet`), through both the `serve` listeners and the `call`
  one-shot. SSE/NDJSON was confirmed working against a real supplier separately
  (2026-07-27); it is unit-tested here but has no recorded live run, as no
  inference service reachable from the beta test app is staked.
- **Beta TestNet, 2026-08-04** — multi-app, service discovery and supplier
  allow/deny verified live: two staked apps in one process, both services
  discovered from their keys, relays through each, separate sessions, the
  WebSocket bridge handshaking with the session's app, and allow/deny narrowing
  32 suppliers to 1 and 31 without leaking across apps.

- **Beta TestNet, 2026-08-19** — re-verified end to end after the `poktroll
  v0.1.35` / `go 1.26.6` dependency bump, since a Shannon SDK bump has broken
  signing before: a signed CometBFT relay succeeded on the first attempt, a
  WebSocket subscription returned live block events as text frames, and a session
  rollover closed the bridge with the expected client-facing code and no protocol
  error from the relay miner. No panics were recovered during the run.

[Unreleased]: https://github.com/pokt-network/pocket-ap/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/pokt-network/pocket-ap/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/pokt-network/pocket-ap/releases/tag/v0.1.0
