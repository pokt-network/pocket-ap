# 🔌 pocket-ap — Pocket Access Point

> **Your key. Your stake. No gateway.**

[![CI](https://github.com/pokt-network/pocket-ap/actions/workflows/ci.yml/badge.svg)](https://github.com/pokt-network/pocket-ap/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/pokt-network/pocket-ap.svg)](https://pkg.go.dev/github.com/pokt-network/pocket-ap)
![Go](https://img.shields.io/github/go-mod/go-version/pokt-network/pocket-ap)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

**Jump to:** [Install](#installation) · [Quickstart](#quickstart) · [`call`](#one-shot-pocket-ap-call) · [Several apps](#several-apps-one-process) · [Security](#security) · [Status](#status) · [Architecture](#architecture) · [Roadmap](#roadmap) · [Contributing](#contributing)

A self-hosted, drop-in RPC proxy for [Pocket Network](https://pokt.network)'s
Shannon protocol. Stake an app, run the binary, point your `RPC_URL` at a local
listener — your traffic is signed and relayed straight to network suppliers.
**No gateway middleman.**

```
your app  ──HTTP──▶  pocket-ap (localhost)  ──signed relay──▶  Pocket supplier
                     │
                     ├─ fetch + rotate session (per network params)
                     ├─ ring-sign the relay off-chain (your app key)
                     ├─ pick a supplier endpoint
                     └─ verify the supplier's response signature
```

Unlike a hosted RPC service or a shared gateway, nothing sits between your app
and the network: your key, your staked app, direct to suppliers, running on your
box.

## Prerequisites

**You do not need to run a full node.** pocket-ap talks to one over the network,
and the Pocket Network Foundation operates public ones.

### `pocketd` — to stake the app

pocket-ap relays for an app that is already staked; `pocketd` is the CLI that
stakes it and the tool you check with when relays misbehave.

```sh
curl -sSL https://raw.githubusercontent.com/pokt-network/poktroll/main/tools/scripts/pocketd-install.sh | bash
pocketd version
```

It installs one binary to `/usr/local/bin`, verifies the release checksum, and
sets up **no node, no systemd, nothing else**. Add `-s -- --upgrade` to update.
Piping a script from the internet into a shell deserves a read first:

```sh
curl -sSL https://raw.githubusercontent.com/pokt-network/poktroll/main/tools/scripts/pocketd-install.sh -o pocketd-install.sh
less pocketd-install.sh && bash pocketd-install.sh
```

### From nothing to a working relay

pocket-ap relays for an app that is **already staked**. If you have one, skip to
[Quickstart](#quickstart). If you do not, this is the whole path — it is
`pocketd` work, not pocket-ap work, and it is the part neither tool's docs
previously owned.

Shown on **Beta**, whose tokens are worthless; MainNet is the same five commands
with `--network=main` and real POKT.

```sh
# 1. Create the app key. Use a real keyring backend for anything you care about;
#    --keyring-backend test stores it unencrypted on disk.
pocketd keys add my-app --keyring-backend test
APP=$(pocketd keys show my-app -a --keyring-backend test)

# 2. Fund it. The stake has to come from somewhere, and the minimum is a chain
#    parameter — read it rather than guessing, it changes:
pocketd query application params --network=beta -o json   # min_stake, in upokt

# 3. Describe the stake. stake-application takes a --config FILE, not flags.
cat > stake.yaml <<'EOF'
stake_amount: 1000000000upokt   # >= min_stake from step 2 (1000000000upokt = 1000 POKT)
service_ids:
  - pnf-pocket-beta             # EXACTLY ONE — see below
EOF

# 4. Stake. --fees is REQUIRED; without it the tx is rejected.
pocketd tx application stake-application \
  --config stake.yaml --from my-app --keyring-backend test \
  --network=beta --fees 200000upokt --yes

# 5. Export the key as hex. THIS is the step that joins the two tools: pocket-ap
#    wants a raw hex key, and the keyring stores an armored one.
pocketd keys export my-app --unarmored-hex --unsafe --keyring-backend test --yes
```

Put that hex string in `POCKET_APP_PRIVATE_KEY` (not in a file), point
`config.example.yaml`'s `fullnode` block at Beta, and verify with `call` —
`service_id` is derived from the key, so there is nothing else to configure:

```sh
POCKET_APP_PRIVATE_KEY=<hex> ./bin/pocket-ap call --config config.yaml -v \
  -X GET --path /status
```

Four things that bite, in the order they bite:

- **`service_ids` is a list that must have exactly one entry.** poktroll rejects
  anything else (`application must have exactly one service`). This is why
  "several services" means several apps, and why pocket-ap can derive the
  service from the key at all.
- **`stake-application` takes `--config <file>`.** There is no `--stake-amount`
  or `--service-id` flag to reach for.
- **`--network=beta` sets `--chain-id`, `--node` and `--grpc-addr` together.**
  Omit it and `pocketd` quietly queries `tcp://localhost:26657` and fails as
  though the network were down.
- **`--unarmored-hex` requires `--unsafe`,** and it prints a live key to your
  terminal. Pipe it somewhere sensible; do not paste it into a config you might
  commit. Add `--yes` or a script hangs on the confirmation prompt.
- **Omitting `--fees` fails, and the message is buried.** The transaction returns
  roughly forty lines of Go stack trace whose last six words are the only ones
  that matter: `insufficient fees; got:  required: 1upokt`. Trivial to satisfy,
  easy to lose ten minutes to.
- **You do NOT need to delegate to a gateway.** An application is always a member
  of its own ring, so a bare stake is enough: the walkthrough above was run end to
  end with `delegatee_gateway_addresses: []`. Delegation exists so a gateway can
  sign on your behalf, which is the arrangement pocket-ap exists to avoid.

> ⚠️ **Step 2: use the web faucet, not `pocketd faucet fund`.** For Beta the
> faucet is <https://faucet.beta.testnet.pokt.network/pokt/> — a browser page.
> Paste the address from step 1.
>
> `pocketd faucet fund` will not get you there, and it is worth knowing why
> before you lose time on it. It is built with faucet URLs that no longer
> resolve — `shannon-testnet-grove-faucet.{alpha,beta}.poktroll.com` and
> `shannon-grove-faucet.mainnet.poktroll.com`, all three NXDOMAIN as of
> 2026-08-19 — so it fails with `no such host` on every network. The command
> also has no `--faucet-base-url` flag despite its own `--help` describing one,
> so you cannot point it at the working faucet either. And the working faucet is
> a UI rather than the `POST /{denom}/{address}` REST service that command
> speaks, so the two would not fit together even if it could be aimed.
>
> Everything else in this section was verified against `pocketd` and the poktroll
> source. This step is the one that needs a browser.

### Public full-node endpoints

pocket-ap needs **both** transports — gRPC for sessions/apps/accounts, CometBFT
RPC for block height. They are different hosts, and both are required.

| | Beta TestNet (`pocket-lego-testnet`) | MainNet (`pocket`) |
| --- | --- | --- |
| **gRPC** (`fullnode.grpc_host_port`) | `sauron-grpc.beta.infra.pocket.network:443` | `sauron-grpc.infra.pocket.network:443` |
| **CometBFT RPC** (`fullnode.rpc_url`) | `https://sauron-rpc.beta.infra.pocket.network` | `https://sauron-rpc.infra.pocket.network` |
| **REST / LCD** | `https://sauron-api.beta.infra.pocket.network` | `https://sauron-api.infra.pocket.network` |

Both gRPC endpoints are TLS on `:443`, so leave `grpc_insecure: false`. REST/LCD
is listed for completeness — pocket-ap does not use it (`pocketd` and block
explorers do); a full node's own REST is unrelated to a `rest` **listener**,
which relays to suppliers.

**You do not normally write these two hostnames.** `network: beta` or
`network: main` sets both at once — they have to name the same chain, and setting
them separately is the one edit that must not be done by halves. `--network`
overrides the file for a single run:

```sh
pocket-ap serve --config local/config.yaml --network main
```

`config.example.yaml` ships `network: "beta"`, where the tokens are worthless and
the code path is identical. ⚠️ **On `main`, every relay spends real POKT** from
your app's stake — no dry-run, nothing to refund; the flag warns at startup.
These are shared infrastructure — fine to build on, worth swapping for your own
node if you come to depend on it, which is what setting `fullnode` directly is
for.

`pocketd` reaches them with `--network=beta` (or `main`), which sets `--chain-id`,
`--node` and `--grpc-addr` in one go. **Without it, `pocketd` defaults to a node
on your own machine** (`tcp://localhost:26657`) and fails looking like the
network is down.

## Installation

Pick one. Homebrew, the prebuilt archives and the Linux packages all come out of
the same tagged release, built by goreleaser; building from source works too.

### From source (Go 1.26+)

```sh
git clone https://github.com/pokt-network/pocket-ap && cd pocket-ap
make build              # -> bin/pocket-ap (CGO off, single static binary)
./bin/pocket-ap version
```

Or straight into `$GOBIN`:

```sh
go install github.com/pokt-network/pocket-ap/cmd/pocket-ap@latest
```

### Docker

The image is `FROM scratch` — no shell, no package manager — because it holds a
staked key and there should be nothing to pivot to if the process is ever
compromised. Published for `linux/amd64` and `linux/arm64`:

```sh
docker pull ghcr.io/pokt-network/pocket-ap:v0.1.2   # or :latest
docker run --rm -p 8545:8545 -p 9090:9090 \
    -v "$PWD/local/config.yaml:/etc/pocket-ap/config.yaml:ro" \
    -e POCKET_APP_PRIVATE_KEY \
    ghcr.io/pokt-network/pocket-ap:v0.1.2
```

Or build it yourself — `docker build -t pocket-ap:local .`, or `make docker`,
which stamps the version and commit into `pocket-ap version`.

**Inside a container, bind `0.0.0.0`, not loopback.** A loopback listener does not
reach the host; Docker's port mapping is the boundary instead. This is the one
place the "always bind loopback" rule flips — the Dockerfile explains why.

### Homebrew (macOS and Linux)

```sh
brew tap pokt-network/tap
brew trust --formula pokt-network/tap/pocket-ap
brew install pocket-ap
```

**The `brew trust` line is not optional on Homebrew 6 or newer**, which refuses to
load a formula from a third-party tap until told to:

```
Error: Refusing to load formula pokt-network/tap/pocket-ap from untrusted tap pokt-network/tap.
```

`brew trust pokt-network/tap` trusts the tap as a whole — every formula it carries
now and every one added later. Trusting the single formula is the narrower grant,
so that is what is written above.

### Prebuilt binaries & packages

Every tagged release carries stripped binaries for darwin (arm64/amd64), linux
(amd64/arm64) and windows (amd64), `.deb` / `.rpm` / `.apk` packages for both
linux architectures, and a `checksums.txt` — all from
[goreleaser](.goreleaser.yaml).

```sh
VERSION=v0.1.2
OS=darwin ARCH=arm64      # also built: darwin/amd64, linux/amd64, linux/arm64
BASE=https://github.com/pokt-network/pocket-ap/releases/download/$VERSION

curl -sSLO $BASE/pocket-ap_${OS}_${ARCH}.tar.gz
curl -sSLO $BASE/checksums.txt
shasum -a 256 -c checksums.txt --ignore-missing   # sha256sum -c on linux
tar xzf pocket-ap_${OS}_${ARCH}.tar.gz && ./pocket-ap version
```

**Check the checksum rather than skipping it.** What you are downloading is the
process that will hold your staked app's private key. `--ignore-missing` is what
lets one checksums file verify the single archive you fetched instead of failing
over the eleven you did not.

The binaries are stripped (`-s -w`); that is what keeps them near 100 MB instead
of 150 MB, since the cosmos-sdk / cometbft / go-ethereum tree is large. Panic
stack traces are unaffected — stripping drops the symbol table and DWARF, not the
pclntab — but `dlv` cannot debug a stripped binary, so build from source without
`-ldflags` for that.

An **npm launcher** is still planned (see Roadmap).

## Quickstart

Three things, whichever way you installed: a config, the right network, a key.

**1. Get a config.** The source tree and the release archives both carry
`config.example.yaml`. Homebrew and Docker do not put one in front of you.

```sh
mkdir -p local     # gitignored — the mkdir is needed on a fresh clone, which
                   # has no local/ at all

# source checkout, or an unpacked release archive
cp config.example.yaml local/config.yaml

# anything else — pinned to the version you installed, not to main
curl -sSL -o local/config.yaml \
  https://raw.githubusercontent.com/pokt-network/pocket-ap/v0.1.2/config.example.yaml
```

The image ships its own copy at `/etc/pocket-ap/config.example.yaml`. It is `FROM
scratch`, so there is no shell and no `cat` to pipe it out with — copy it off a
container that never runs:

```sh
id=$(docker create ghcr.io/pokt-network/pocket-ap:v0.1.2)
docker cp "$id:/etc/pocket-ap/config.example.yaml" local/config.yaml && docker rm "$id"
```

**2. Pick a network.** The example ships `network: "beta"` — the Beta TestNet the
staking walkthrough above uses, where relays cost nothing. One key sets both
full-node transports, so they cannot end up naming different chains.

Switch for a single run without editing anything:

```sh
pocket-ap serve --config local/config.yaml --network main   # ⚠️ spends real POKT
```

⚠️ **On `main`, every relay spends real POKT from your app's stake — no dry run,
no refund.** The flag says so at startup, at WARN.

`network` and `fullnode.*` are mutually exclusive in a config file: naming a
network and also spelling out a hostname means one of them is wrong with no way
to say which, and the failure that produces — a full node that has never heard of
your app — reads as "the network is broken" rather than "wrong network". Delete
the `network` line and set `fullnode` yourself to point at your own node:

```yaml
fullnode:
  grpc_host_port: "localhost:9090"
  grpc_insecure: true          # only for a local node with no TLS
  rpc_url: "http://localhost:26657"
```

**3. Give it the key and run.** `service_id` stays optional — an app stakes for
exactly one service, so pocket-ap reads it off the chain at startup.

```sh
export POCKET_APP_PRIVATE_KEY=<64 hex chars>   # keeps the key out of any file

./bin/pocket-ap serve --config local/config.yaml   # from source
pocket-ap serve --config local/config.yaml         # brew, or any binary on PATH
```

Docker needs two more things, both covered in [Docker](#docker): mount the config
in, and bind `0.0.0.0` inside the container rather than loopback.

Then point your client at `http://127.0.0.1:8545`.

Whatever service your app is staked for is what it can relay. Confirm it before
blaming the proxy (this is also what pocket-ap discovers for you at startup):

```sh
pocketd query application show-application <your-app-addr> --network=beta -o json \
  | jq '.application.service_configs'
```

Check it works before wiring anything up — one relay, no daemon. **Send
something the service you staked for can actually answer:**

```sh
# a Cosmos service — pnf-pocket-beta, which is what the walkthrough above stakes for
pocket-ap call --config local/config.yaml --rpc-type comet_bft -X GET --path /status

# an EVM service — pnf-anvil and the like
pocket-ap call --config local/config.yaml \
    -d '{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}'
```

Sending the EVM one to a Cosmos service answers
`{"error":{"code":-32601,"message":"Method not found"}}`, and **that is a working
relay** — it travelled to a supplier and the chain replied. Add `-v` to see the
session, the supplier and each attempt, which is what distinguishes this from
anything the proxy got wrong.

**The key must be the APP's own key, not a gateway key.** The app address is
derived from it, so a wrong key does not error — it makes you a different app.

`config.schema.yaml` documents every option; point your editor at it with
`# yaml-language-server: $schema=../config.schema.yaml`.

## One-shot: `pocket-ap call`

`call` sends a single relay and prints the response — no listener, no daemon. It
is the fastest way to answer "is my staked app working?", and the tool to reach
for when debugging a service.

```sh
pocket-ap call --config local/config.yaml \
    -d '{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}'
# {"jsonrpc":"2.0","id":1,"result":"0x0"}
```

The body goes to stdout verbatim, so it pipes into `jq`; everything else goes to
stderr. `--service` and `--rpc-type` are inferred from the config when its
listeners make them unambiguous. Flags mirror curl: `-X`, `-H`, `-d` (`@file`
and `-` for stdin), `--path`.

`-v` reports what the relay actually did — the session, which supplier answered,
and each failover:

```
--- relay diagnostics ---
service:   pnf-anvil (json_rpc)
app:       pokt1e3scnf3tfs9t44pawlvpemm6r0ggy3un4avmdk
session:   3d2c1d9ac0c6... (ends at block 476660)
endpoints: 37 in session, 37 support json_rpc
attempt 1: pokt18na0p7t37du6s5yufvajfatwhkv362qyjytxvz in 304ms via https://rm.beta.infra.pocket.network -> ok
total:     1.605s
```

`--compare <url>` sends the same request straight to a URL as well and diffs the
two responses — the quickest way to tell a relay problem from a backend one:

```sh
pocket-ap call --config local/config.yaml --compare https://cloudflare-eth.com \
    -d '{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}'
```

The verdict has three tiers: `identical` (byte-for-byte), `equivalent` (same JSON
value, only key order or whitespace differs), or `differ` (with both bodies
shown). Byte-identity stays the top tier because a real passthrough serving the
same backend *should* be byte-identical. A `differ` is a prompt to look, not a
verdict — non-deterministic methods move between calls, and the two sides are
usually different nodes anyway.

The JSON tier is the only place anything looks inside a payload, and it is
confined to this debug path: it labels a diff for a human and feeds nothing back
into routing or selection. The relay itself never parses what it carries.

Both are debug and verification tools: every invocation fetches a fresh session,
so neither is a path for production traffic — that is what `serve` is for.

## Health

Set `admin.addr` to expose a health endpoint on its own port (leave it out and no
admin listener starts):

```sh
curl localhost:9090/pocket-ap/health
```
```json
{
  "status": "ok",
  "apps": [ { "address": "pokt1e3scnf3tfs9t44pawlvpemm6r0ggy3un4avmdk", "service_id": "pnf-anvil" } ],
  "chain": { "block_height": 476830, "height_age_seconds": 0.2, "poll_interval_seconds": 10 },
  "services": [
    { "service_id": "pnf-anvil", "session_id": "87c4421…", "endpoints": 37,
      "session_cached": true, "attempts": 3, "successes": 3, "failures": 0,
      "mean_latency_ms": 156.2 }
  ]
}
```

A `recovered_panics` field appears if the count is non-zero. Nothing here is
expected to panic, so any value means a frame, a relay or a background task was
abandoned partway — worth investigating, though it does not by itself make the
proxy unhealthy.

`200` when it can relay, `503` when it cannot — **use it for readiness, not
liveness.** The signal is block-height staleness: if the poller stops tracking
the chain, sessions stop rotating and relays break. Restarting the process will
not fix an unreachable full node, so a liveness probe would only produce a
restart loop during an upstream outage.

It has its own port because the relay listeners proxy every path verbatim —
`/health` there would collide with the proxied service's own routes. The endpoint
never calls the full node, so probing it costs the network nothing.

The counters are in-memory and per-process: they reset on restart, and nothing is
written to disk or sent anywhere. Storing or shipping telemetry is not this
proxy's job.

## Several apps, one process

An application stakes for **exactly one service** — poktroll enforces it
(`application must have exactly one service`). So "serve several services" means
"several apps", and the way to do that is one key each, not one key with a list:

```yaml
apps:
  - private_key_hex: ""          # staked for pnf-pocket-beta
  - private_key_hex: ""          # staked for eth
    suppliers:
      deny: ["pokt1…"]           # never route this app's relays here
listeners:
  - { addr: "127.0.0.1:8545", service_id: "pnf-pocket-beta", rpc_type: "json_rpc" }
  - { addr: "127.0.0.1:8546", service_id: "eth",             rpc_type: "json_rpc" }
```

`POCKET_APP_PRIVATE_KEYS` (comma-separated) fills this from the environment, so
no key has to touch a file. Each app keeps its own sessions, its own signing key
and its own supplier policy; one block poller and one listener set serve them all.

**`service_id` is optional.** The key derives the app address, the app address
has one service onchain, so pocket-ap looks it up at startup and tells you what
it found. Set it anyway and it becomes a check: a mismatch is a startup error
instead of every relay failing with "session not found". A listener may omit it
too when there is a single app; with several, each listener says which one it
fronts.

### Choosing suppliers

Static lists per app, applied before selection, on **two independent axes**:

```yaml
apps:
  - private_key_hex: ""
    suppliers:
      allow: ["pokt1…", "pokt1…"]        # exhaustive: nothing else is used
      deny:  ["pokt1…"]                  # removed; deny wins over allow
      allow_hosts: ["rm.example.com"]    # by relay-miner host, not by supplier
      deny_hosts:  ["*.slow.example"]
```

`deny` is how you drop one bad supplier out of a random pick. **A one-entry
allowlist removes failover**: there is no second supplier left to try when it is
down.

**Why the host axis exists, and why it is not just a coarser address list.** One
relay-miner operator routinely runs many supplier stakes behind a single host —
on beta, all 32 `pnf-pocket-beta` suppliers answer on
`rm.beta.infra.pocket.network`. So "route away from this operator" is expressible
*only* by host: the set of addresses behind a host is session-scoped and cannot
be listed in advance. Conversely "drop this one supplier" is expressible only by
address. Both axes apply, and both must permit a supplier.

Host entries are bare hosts, never URLs:

| entry | matches |
| --- | --- |
| `rm.example.com` | that host on any port |
| `rm.example.com:443` | that host on that port only |
| `*.example.com` | any subdomain, **not** the apex |

The scheme's default port is filled in, so `rm.example.com:443` matches
`https://rm.example.com`. Matching is on the **parsed host**, never a substring
of the URL — `https://evil.example/rm.example.com` is not `rm.example.com`, and a
substring denylist would fail open on exactly that. A URL in a host list is
rejected at startup, and an operator address in a host list (or a host in an
address list) is rejected too: each list refuses the other's content rather than
quietly matching nothing.

#### Per request, from your own process

The config lists are fixed until you restart, which is no use to anything that
changes its mind. Send the same lists as headers and they apply to that relay
alone:

```
X-Pocket-Allow-Suppliers: pokt1…,pokt1…
X-Pocket-Deny-Suppliers:  pokt1…
X-Pocket-Allow-Hosts:     rm.example.com
X-Pocket-Deny-Hosts:      *.slow.example
```

That is what lets an external scoring process sit in front of a long-running
pocket-ap:

```
user --(request)--> your QoS --(request + allowed suppliers)--> pocket-ap
```

All four are repeatable and accept a comma-separated list. They work on every
listener — HTTP and WebSocket alike (on a WebSocket they apply at the handshake,
since a bridge is pinned to one supplier for its life) — and on
`pocket-ap call -H`, so you can reproduce a routing decision by hand.

Three things to know:

- **A request can only narrow what the config allows, never widen it.** Every
  list in force applies and all must permit a supplier. The listener is
  unauthenticated, so a header that could *add* a supplier you excluded would
  hand your routing to anything that can reach the port. Leave the app's
  `suppliers` empty and the header decides alone; set it and it becomes a ceiling.
- **They are stripped before the relay is signed.** They address this proxy, not
  the backend, so they never reach the supplier — which would otherwise be told
  how you ranked its competitors. Every list is taken out even when another one
  is malformed, so a rejected request cannot leave one behind.
- **A value in the wrong list is a 400**, and costs no relay — an address that is
  not `pokt1…`, or a URL where a host belongs. A value that never matches is
  silent and expensive: an allowlist would drop every supplier and a denylist
  would deny nobody, and both read as "the network is broken".

This is not QoS. Nothing here measures latency, scores suppliers or changes its
mind — it is the caller naming who they are willing to pay, and pocket-ap
obeying. Supplier *quality* remains SAGE's job (see the note under Roadmap).

## Security

Three defaults worth knowing, because each one exists for a reason that is not obvious:

Every listener rejects browser `Origin` headers by default, and leaves native
clients — node, curl, go, which send no `Origin` — alone. Allowlist per listener:

```yaml
listeners:
  - addr: "127.0.0.1:8545"
    service_id: "eth"
    rpc_type: "json_rpc"
    allowed_origins:
      - "http://localhost:3000"
```

This is not the usual CORS boilerplate. CORS stops a malicious page **reading** a
cross-origin response; it does not stop the request being **sent**. A page can
POST here with `Content-Type: text/plain` — a CORS "simple request", so no
preflight — and the relay is signed with your key and billed to your stake before
CORS is consulted. The attacker learns nothing and you still pay for it.

Allowlisted origins also get real CORS response headers, so a browser dapp can
actually read what it asked for, and preflights are answered locally rather than
costing a relay.

### Bind loopback

The example binds `127.0.0.1:8545`, and you should keep it that way. `:8545` is
every interface — anyone on your wifi can POST to your machine and spend your
stake. No attack, just your IP. They send no `Origin`, so they look like any
other native client, which is correct behaviour and exactly the problem.

Binding loopback also enables the `Host` check that closes DNS rebinding — a page
whose DNS re-answers `127.0.0.1` is *same-origin* to the browser, so CORS never
runs, but it still sends `Host: evil.com` and gets rejected. Bind wider and that
check turns off, because a LAN IP or Docker service name is a legitimate `Host`
there and guessing would break more than it protects. Set `allowed_hosts`
explicitly if you need a specific name (behind a reverse proxy, say).

## Status

**All five Shannon RPC types relay live.** Verified against Pocket beta TestNet
on 2026-07-22 (service `pnf-pocket-beta`, 32 suppliers): JSON-RPC, REST,
CometBFT, WebSocket and gRPC each completed a real signed relay on the first
attempt, through both the `serve` listeners and the `call` one-shot.

| Piece | File | Status |
| --- | --- | --- |
| full-node clients / response validation | `pocket/fullnode.go`, `pocket/validate.go` | ✅ live |
| session fetch / rotation / endpoints | `pocket/session.go`, `pocket/endpoint.go` | ✅ live |
| ring signing / key→addr | `pocket/signer.go` | ✅ live |
| HTTP front, sender, selector, config, wiring | `transport/http.go`, `pocket/sender.go`, `selector/`, `config/`, `cmd/` | ✅ live |
| WebSocket | `transport/ws.go`, `websockets/`, `relay/bridge.go` | ✅ live |
| gRPC (relaying out, native + gRPC-Web) | `pocket/grpc.go` | ✅ live |
| gRPC (listener: h2c + trailers) | `transport/grpc.go` | ✅ live |
| SSE / NDJSON streaming | `relay/stream.go`, `transport/http.go` | ✅ live (team-confirmed, no recorded run) |
| Multi-app + service discovery | `pocket/apps.go`, `config/`, `cmd/` | ✅ live |
| Supplier allow/deny — by address **and** by host, from config **and** per-request headers | `selector/filter.go`, `domain/supplier.go`, `transport/suppliers.go` | ✅ live |

SSE is the one path with no run written down. It was confirmed working against a
real supplier by a team member in 2026-07-27, so it is not in doubt — but every
other row above has a request and a response recorded, and this one does not,
because no inference service our beta app can reach is staked yet. The wire format it
implements (the relay miner's `||POKT_STREAM||`-delimited signed batches) is
covered by unit tests against the miner's own constant.

Lifted from SAGE `protocol/shannon/` (`fullnode.go`, `sessions.go`, `endpoint.go`,
`signer.go`, `relayer.go`, `apps.go`).

## Architecture

Core relay flow is transport-agnostic; the payload is opaque bytes (no per-chain
parsing, no QoS). Four seams (`relay/relay.go`) keep it extensible:

- **SessionSource** — fetch/cache/rotate sessions (`shannon.SessionManager`)
- **Signer** — build + ring-sign relays (`shannon.Signer`) — *the crown jewel; do not reimplement the crypto, it lives in shannon-sdk*
- **Selector** — pick supplier endpoints (`selector.Random`, filtered by RPC type)
- **Transport** — the front listeners (`transport/`)

### RPC types

All five Shannon types are native. They split by **lifecycle**, not name:

- **Stateless** (JSON-RPC, REST, CometBFT, unary gRPC) → one transparent
  reverse-proxy adapter, `transport/http.go`. **v0.**
- **Stateful streaming** (WebSocket) → `transport/ws.go`. A bridge is pinned to
  one supplier and one session; at a session boundary it closes with a reconnect
  hint rather than re-signing a live socket.

gRPC support depends on which relay miner a supplier runs. The released miner
(poktroll v0.1.34) bridges only WebSocket and routes gRPC through a
one-request/one-response path with no way to return `grpc-status`, which gRPC
carries in HTTP/2 trailers. The newer ha-relayminer folds those trailers into the
response headers and supports unary and buffered server-streaming (not
full-duplex), so pocket-ap sends gRPC relays over a **gRPC** transport to the
miner rather than the HTTP POST it uses for everything else — the miner only
folds trailers on that path.

Two framings are supported, selected by `grpc_mode` (default: auto). Native gRPC
is right when nothing terminates HTTP/2 between you and the miner. Behind an
ingress that terminates HTTP/2 and forwards HTTP/1.1 — which is what Pocket beta
runs — the miner answers native calls `505 gRPC requires HTTP/2`, and gRPC-Web
gets through instead because it carries its trailers as a frame inside the body.
Auto tries native once per supplier host and remembers the answer.

## Docs

- **`config.schema.yaml`** — every config option, machine-readable (JSON Schema).
- **`AGENTS.md`** — for an agent operating this: verification, and an error →
  what-it-actually-means table.
- **`CLAUDE.md`** — for changing pocket-ap's own code.

## Roadmap

`v0.1.2` (2026-08-20) is the current release; `v0.1.1` the same day was the first
one with artifacts. Everything under **shipped** below is in them.

- **shipped** — all five Shannon RPC types, each live-verified against Beta:
  JSON-RPC, REST and CometBFT over one HTTP adapter, WebSocket (2026-07-22), gRPC
  relaying and a gRPC listener; failover to the next supplier; `call` one-shot;
  `/health`; multi-app with service discovery from the key (2026-08-04); supplier
  allow/deny by address **and** by relay-miner host, from config **and** from
  per-request headers, so an external QoS process can steer without a restart
  (2026-08-06); Homebrew, ghcr images, signed checksums and Linux packages
  (2026-08-20)
- **`v0.1.2`** — `network: beta|main` plus a `--network` flag, `--help` reaching
  the real usage, long flags spelled `--`, and a container image that
  cross-compiles rather than emulating
- **next** — app rotation (several apps on ONE service); a recorded SSE/NDJSON run
  — it works, it needs a reachable inference service to write down
- **deferred** — an npm launcher. The scaffolding is written (`npm/`) and
  deliberately not published: pocket-ap is a daemon rather than a library, and
  Homebrew, the archives and the container image already serve that use.
  `npm/README.md` says what would change that.
- **later** — wasm SDK for edge/serverless (swaps gRPC→cosmos REST),
  delegated-gateway signer mode

Supplier quality (QoS, reputation, height-awareness) is deliberately **not** on
this list. It is gateway work, and it lives in SAGE — pocket-ap fails over to the
next supplier and otherwise stays out of the way.

## Contributing

Contributions welcome — read [`CONTRIBUTING.md`](CONTRIBUTING.md) first. It covers
the build/test/lint flow, the SAGE-sync doctrine (this is a near-verbatim lift),
and the one rule that shapes everything: pocket-ap is a transparent passthrough,
so gateway smarts belong upstream. By participating you agree to the
[Code of Conduct](CODE_OF_CONDUCT.md).

**Found a security issue?** This tool holds an app key and spends stake — report
privately, never in a public issue. See [`SECURITY.md`](SECURITY.md).

Changes are tracked in [`CHANGELOG.md`](CHANGELOG.md).

## License

MIT — same as [poktroll](https://github.com/pokt-network/poktroll), [PATH](https://github.com/pokt-network/path) and the relay miner. See `LICENSE`.
