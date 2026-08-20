# AGENTS.md — operating pocket-ap

For an agent **using** pocket-ap (running it, configuring it, diagnosing it).
If you are changing pocket-ap's own code, read `CLAUDE.md` instead.

## What it is, in one paragraph

A self-hosted RPC proxy for Pocket Network. You stake an application, run this
binary, and point a client's `RPC_URL` at a local listener. It fetches a session,
ring-signs each request with the app's key, relays it straight to a supplier, and
verifies the signed response. No gateway sits in between. **Every relay spends the
operator's staked POKT.**

## The one thing to understand first

**The private key IS the app identity.** The app address is derived from the key,
never configured. So a wrong key does not error — it makes you a *different app*,
and every relay then fails with something that looks like a network problem. If
relays fail in a confusing way, suspect the key before you suspect anything else.

## Dependencies

**No full node required.** pocket-ap dials a public one over the network.

`pocketd` is a separate binary — the Pocket CLI. pocket-ap does **not** need it to
run; you need it to stake the app and to answer "what does the network actually
offer?" (below). Install:

```sh
curl -sSL https://raw.githubusercontent.com/pokt-network/poktroll/main/tools/scripts/pocketd-install.sh | bash
```

One binary into `/usr/local/bin`, release checksum verified, no node or service
configured. `-s -- --upgrade` to update.

### Getting a staked app — the prerequisite this repo cannot do for you

pocket-ap **cannot relay without a staked application's private key.** There is
no demo key, no anonymous mode, and there should not be: a shared key is a public
credential that spends real stake. If you have no app, nothing below this line
works, and this is `pocketd` work rather than pocket-ap work.

Beta, whose tokens are worthless. MainNet is identical with `--network=main`.

```sh
pocketd keys add my-app --keyring-backend test
APP=$(pocketd keys show my-app -a --keyring-backend test)

# Fund $APP: https://faucet.beta.testnet.pokt.network/pokt/  (a BROWSER page)
# Minimum is a chain parameter — read it, do not assume:
pocketd query application params --network=beta -o json    # -> min_stake

cat > stake.yaml <<'EOF'
stake_amount: 1000000000upokt
service_ids:
  - pnf-pocket-beta
EOF

pocketd tx application stake-application \
  --config stake.yaml --from my-app --keyring-backend test --network=beta \
  --fees 200000upokt --yes

# The step that joins the two tools: pocket-ap wants raw hex, the keyring
# stores armored. Nothing else bridges them.
pocketd keys export my-app --unarmored-hex --unsafe --keyring-backend test --yes
```

Then `POCKET_APP_PRIVATE_KEY=<hex>`, and `service_id` needs no configuring — it
is derived from the key.

Failure modes, in the order you will hit them:

| symptom | cause |
| --- | --- |
| `pocketd` hangs or reports the network down | `--network` omitted; it defaults to `tcp://localhost:26657` |
| `unknown flag: --stake-amount` | `stake-application` takes `--config <file>`, not flags |
| `application must have exactly one service` | `service_ids` accepts a list; the chain accepts exactly one entry |
| ~40 lines of stack trace ending `insufficient fees` | `--fees` omitted on the stake tx |
| `no such host` from `pocketd faucet fund` | its built-in faucet URLs are dead on all three networks; use the browser faucet above |
| pocket-ap: `no configured app is staked for service X` | listener names a service the key is not staked for — omit `service_id` and let it derive |

### Public full-node endpoints

Both transports are required and they are **different hosts**.

| Config field | Beta TestNet (`pocket-lego-testnet`) | MainNet (`pocket`) |
| --- | --- | --- |
| `fullnode.grpc_host_port` | `sauron-grpc.beta.infra.pocket.network:443` | `sauron-grpc.infra.pocket.network:443` |
| `fullnode.rpc_url` | `https://sauron-rpc.beta.infra.pocket.network` | `https://sauron-rpc.infra.pocket.network` |

TLS on `:443` both — keep `grpc_insecure: false`, which `network:` enforces for
you. Prefer `network: beta|main` over writing these: it sets both transports at
once so they cannot name different chains, and spelling one out alongside it is a
config error. Default to **Beta**; MainNet spends real POKT. (REST/LCD exists too — `sauron-api[.beta].infra.pocket.network`
— but pocket-ap never calls it. It is for `pocketd` and explorers, and is
unrelated to a `rest` listener, which relays to suppliers.)

For `pocketd`, pass `--network=beta` (or `main`): it sets `--chain-id`, `--node`
and `--grpc-addr` together. **Omit it and `pocketd` queries `localhost:26657`**,
failing as if the network were down.

## Run it

```sh
make build                                   # -> bin/pocket-ap
mkdir -p local                               # gitignored; absent in a fresh clone
cp config.example.yaml local/config.yaml     # ships network: "beta"
export POCKET_APP_PRIVATE_KEY=<64 hex chars> # preferred over putting it in the file
./bin/pocket-ap serve --config local/config.yaml
```

`network` sets both full-node transports at once, so they cannot name different
chains: `beta` is `sauron-grpc.beta.infra.pocket.network:443` +
`https://sauron-rpc.beta.infra.pocket.network`, `main` is the same two without
`.beta`. `--network main` overrides the file for one run and ⚠️ spends real POKT.
Naming `network` and `fullnode.*` in the same file is an error — set `fullnode`
alone for your own node.

Minimum viable config (`config.schema.yaml` is the machine-readable contract):

```yaml
network: "beta"                     # sets BOTH fullnode transports; "main" spends real POKT
listeners:
  - addr: "127.0.0.1:8545"          # loopback. see "Hard rules".
    rpc_type: "json_rpc"
    # service_id optional with one app (it stakes for exactly one service, read
    # from the chain at startup); required once several apps are configured.
```

Several staked apps in one process — `apps:` instead of `app:` (both is an
error). One app per service; poktroll allows an application exactly one service,
so multi-service **is** multi-app. `POCKET_APP_PRIVATE_KEYS` (comma-separated)
fills it from the environment.

```yaml
apps:
  - private_key_hex: ""            # service discovered from the key
  - private_key_hex: ""
    service_id: "eth"              # optional; startup FAILS if the chain disagrees
    suppliers:
      allow: ["pokt1…"]            # exhaustive — kills failover if it has one entry
      deny:  ["pokt1…"]            # deny wins over allow
      allow_hosts: ["rm.x.com"]    # by relay-miner host: covers every supplier behind it
      deny_hosts:  ["*.slow.io"]   # bare hosts, never URLs
```

Both axes apply, and both must permit a supplier. The host axis exists because
one operator runs many supplier stakes behind one host — on beta all 32
`pnf-pocket-beta` suppliers answer on `rm.beta.infra.pocket.network` — so "route
away from this operator" cannot be written as an address list: that set changes
every session.

### Steering per request, from your own process

The config lists need a restart. The same four lists go per request as headers,
which is how an external QoS process drives selection on a running proxy:

```sh
curl localhost:8545 -H 'X-Pocket-Allow-Suppliers: pokt1…,pokt1…' \
                    -H 'X-Pocket-Deny-Hosts: *.slow.io' -d '{…}'
```

`X-Pocket-Allow-Suppliers`, `X-Pocket-Deny-Suppliers`, `X-Pocket-Allow-Hosts`,
`X-Pocket-Deny-Hosts` — comma-separated, repeatable, on every listener (on a
WebSocket they apply at the handshake) and on `call -H`.

**A request can only narrow the config, never widen it.** Every list in force
must permit a supplier, so the config lists are a ceiling. The headers are
stripped before the relay is signed, so a supplier never learns how you ranked
its competitors. A value in the wrong list — an address that is not `pokt1…`, a
URL where a host belongs — is a **400** and costs no relay.

## Verify it works — use `call`, not `serve`

`call` sends one relay and prints the response. It is the fastest way to answer
"is this working?", and the right first move when anything is wrong.

```sh
pocket-ap call --config local/config.yaml \
    -d '{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}'
# {"jsonrpc":"2.0","id":1,"result":"0x0"}
```

Add `-v` and it tells you what actually happened — the app address it derived,
the session, how many endpoints, which supplier answered, and each failover:

```
app:       pokt1e3scnf3t...          <- is this the app you think you are?
session:   3d2c1d9a... (ends at block 476660)
endpoints: 37 in session, 37 support json_rpc
attempt 1: pokt18na0p7t... in 304ms via https://rm.beta.infra.pocket.network -> ok
```

Body goes to stdout verbatim (pipe to `jq`); everything else to stderr.
`--compare <url>` sends the same request straight to a URL too and diffs them —
that is how you tell a relay problem from a backend problem.

## Error → what it actually means

The single highest-value table here. Most of these do not say what they mean.

| What you see | What it actually is |
| --- | --- |
| `session fetch failed … rpc error: code = Unavailable … connection refused` | The **full node** is unreachable. Check `fullnode.grpc_host_port`. Nothing to do with suppliers. |
| `session … not found` / relays fail for one service | The app is **not staked for that `service_id`**. Check with `pocketd query application show-application <addr> --network=beta` — `service_configs` lists what it can relay for. |
| `deriveAppAddr: key is N bytes, want 32` | Truncated or pasted-wrong key. secp256k1 keys are exactly 64 hex chars. |
| `-v` shows an `app:` address you do not recognise | Wrong key. The address is derived from it. |
| `no endpoint supports the requested rpc type` | No supplier in the session advertises that `rpc_type`. Not a bug — check what they advertise (see below). |
| `{"error":{"code":-32600,"message":"Invalid request"}}` but the relay **succeeded** | Almost always **`Content-Type`**. `curl -d` defaults to form encoding; the backend rejects it. Add `-H 'Content-Type: application/json'`. The relay was spent — passthrough is faithful, the backend simply did not like it. |
| `403 origin not allowed` | A browser `Origin` that is not allowlisted. Add it to that listener's `allowed_origins`. Native clients (node/curl/go) send no Origin and are unaffected. |
| `403 host not allowed` | The `Host` header is not one this loopback listener answers to. Expected under DNS rebinding; otherwise set `allowed_hosts`. |
| `config: … unknown field` | Config is strict on purpose. A typo'd key fails at load rather than silently defaulting. |
| `config: no app key — set app.private_key_hex, apps[].private_key_hex, POCKET_APP_PRIVATE_KEY or POCKET_APP_PRIVATE_KEYS` | No source has a key. |
| `config: set either app or apps, not both` | Pick one form. Both would make "which key signs" depend on load order. |
| `app … is staked for service "X", but the config says "Y"` | The key is not the app you think it is (or `service_id` is stale). The chain wins; fix one of them. |
| `app … is staked for no service` | Unstaked app, wrong network, or wrong key. This used to surface as `session not found` on every relay. |
| `listener …: service_id is required — N apps are configured` | With several apps, each port must say which one it fronts. |
| `listener …: no configured app is staked for service "X"` | Typo, or the app for that service is not in the config. |
| `selector: every supplier for X was filtered out (… config allow/deny a/b, hosts c/d; request allow/deny e/f, hosts g/h)` | A supplier policy left nothing. **The counts say which one**: `config` is the app's `suppliers:` block, `request` is this call's `X-Pocket-*` headers, and `hosts` is the host axis rather than the address one. Check the entries actually appear in the session. |
| `X-Pocket-Allow-Hosts: "https://…" is a URL, not a host` | Host lists take a bare host, optionally with a port, optionally `*.`-prefixed. A URL can never match a parsed host — and as a denylist entry it would fail open, so it is refused rather than ignored. |
| `X-Pocket-Allow-Hosts: "pokt1…" is a supplier operator address` | Wrong list. Addresses go in `X-Pocket-Allow-Suppliers`/`-Deny-Suppliers`; each list refuses the other's content rather than quietly matching nothing. |
| `config: serve needs at least one listener` | `serve` needs listeners; `call` does not. |
| `/pocket-ap/health` returns 503 | The block poller has lost the chain. Sessions stop rotating and relays break. Check the full node — **restarting pocket-ap will not help**. |

## Checking what the network actually offers

Before assuming pocket-ap is broken, check whether any supplier serves what you
asked for. Suppliers advertise per RPC type:

```sh
pocketd query supplier list-suppliers --network=beta -o json \
  | jq '[.supplier[]?|.services[]?|.endpoints[]?.rpc_type] | group_by(.) | map({type:.[0],count:length})'
```

Reality as of 2026-07-22: service **`pnf-pocket-beta`** advertises all five types
across **32 suppliers**, and all five relay paths were verified live against it
(see the README status table). The older `pnf-anvil` service is still
`JSON_RPC`-only — so a `rest`/`websocket`/`grpc` listener pointed at `pnf-anvil`
will correctly report "no endpoint supports the requested rpc type". That is the
service you picked, not the proxy. Use `pnf-pocket-beta` for anything non-EVM.

## Health, for machines

`GET http://<admin.addr>/pocket-ap/health` → JSON. `200` = can relay, `503` =
cannot. **Readiness, not liveness.** Counters are in-memory and reset on restart;
nothing is persisted or exported anywhere.

```json
{ "status": "ok",
  "apps": [ { "address": "pokt1e3s…", "service_id": "pnf-anvil" } ],
  "uptime_seconds": 128.4,
  "chain": { "block_height": 478443, "height_age_seconds": 1.9,
             "poll_interval_seconds": 10 },
  "services": [ { "service_id": "pnf-anvil", "session_id": "87c4421…",
                  "session_end_height": 478450, "endpoints": 37,
                  "session_cached": true, "attempts": 3, "successes": 3,
                  "failures": 0, "mean_latency_ms": 156.2 } ] }
```

Per-service `session_*`, `endpoints` and `mean_latency_ms` appear once that
service has fetched a session and served a relay; everything else is always
present.

`height_age_seconds` climbing is the leading indicator of breakage. Note
`attempts > successes` is normal — it means failover worked.

A `recovered_panics` field appears only when it is non-zero. Nothing here is
expected to panic, so any value means a frame, a relay or a background task was
abandoned partway and is worth investigating — but it does not by itself change
`status`, and it is not a reason to restart.

## Hard rules

- **Never commit `local/`.** It holds private keys. It is gitignored; keep it that way.
- **Never print or log the key.** Not in errors, not in issues, not in a paste.
- **Never bind `:8545`** (or any `0.0.0.0`). That exposes the relay to your whole
  network, and anyone on it can spend your stake — no attack needed, just your IP.
  Loopback also enables the Host check that closes DNS rebinding.
- **Never set `allowed_origins: ["*"]` to make an error go away.** It hands every
  site the user visits the ability to relay with their key.
- **Do not treat `call` or `--compare` as a traffic path.** Every invocation fetches
  a fresh session. They are debug tools; `serve` is the path.

## Known limits — do not report these as bugs

- **SSE/NDJSON streaming works** — confirmed against a real supplier by a team
  member (2026-07-27). It is **not** covered by any live test here, and no run is
  recorded, because no inference service reachable from our beta app is staked.
  So: do not report "streaming is unproven" as a limitation — it is proven. Do
  expect that a regression in it would go unnoticed by this repo's tests.
- **No QoS.** Supplier selection is random, with failover on hard failure. Quality
  ranking is deliberately SAGE's job, not this proxy's.
- **Full-duplex gRPC streaming is impossible**, not unbuilt — the relay miner
  bridges WebSocket only. Unary and buffered server-streaming work.
- `pocket-ap` relays **opaque bytes**. It does not parse chain payloads and never
  will, so it cannot "fix" a malformed request for you.
