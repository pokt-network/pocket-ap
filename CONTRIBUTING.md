# Contributing to pocket-ap

Thanks for helping. This is a small, deliberately narrow codebase — a transparent
passthrough relay client for Pocket Network's Shannon protocol. Before you start,
two documents set the boundaries:

- **`README.md`** — what pocket-ap is and how to run it.
- **`CLAUDE.md`** — the design doctrine and the reasoning behind every non-obvious
  choice. **Read the relevant section before changing a file.** Much of what looks
  like a bug is load-bearing, and the file says which.

## The one rule that shapes everything

**pocket-ap is a transparent passthrough. The RPC payload is opaque bytes — no
per-chain parsing, no QoS, no reputation, no heuristics.** Those "gateway smarts"
live in the sibling **SAGE** repo and are intentionally absent here. If a change
wants to make the proxy *smarter about traffic*, it almost certainly belongs in
SAGE, not here. The one carve-out (`jsonEquivalent` in `call.go`, debug-only) is
documented and does not license parsing anywhere else.

## This code is a lift — keep it in sync

Most of `pocket/`, `websockets/`, and `relay/` was lifted near-verbatim from SAGE
`protocol/shannon/`. `CLAUDE.md` has the file-by-file provenance table.

- **Do not reimplement the crypto.** Ring signing is `shannon-sdk` +
  `poktroll/pkg/crypto/rings` + `ring-go`, network-verified. Touch the signer only
  to re-sync with SAGE.
- **If you fix a bug in a lifted file, check whether SAGE has it too** and call it
  out in the PR. These lifted bugs are usually shared, and nobody upstream is
  tracking them.
- **Sync goes both ways.** SAGE has adopted designs from here and vice versa. When
  syncing, diff both directions.

## Development

```sh
make build     # -> bin/pocket-ap (CGO off)
make test      # go test ./... -race -count=1
go vet ./...
make lint      # golangci-lint (see below)
make tidy      # sync go.mod/go.sum
```

The cosmos-sdk / cometbft / go-ethereum dep tree is heavy: first cold build ~40s,
cached after. All deps are in the shared module cache, so offline builds work —
prefix with `GOPROXY=off GOSUMDB=off`.

A single test:

```sh
go test ./pocket/ -run TestFoo -race -count=1 -v
```

`golangci-lint` is required for `make lint`
([install](https://golangci-lint.run/welcome/install/)). CI runs it too.

## Testing expectations

- **Race-clean.** `-race` is on by default in `make test`; keep it green.
- **Cover behavior at the seams.** The core packages (`relay`, `config`,
  `domain`, `selector`) sit at 100% statement coverage. `pocket` has a lower
  ceiling by design — the ring signing runs through concrete SDK calls the network
  verifies on every relay, and faking those would scaffold around the one thing we
  don't touch. Don't chase 100% in `pocket`.
- **Pin the constants that a real peer depends on.** Wire literals (the
  `||POKT_STREAM||` stream delimiter, RPC-type enum values, the miner's gRPC
  service path) are pinned in tests precisely so a rename can't keep the suite
  green while breaking every real relay. Follow that pattern.
- **Mutation-check security-critical changes.** The existing tests were validated
  by injecting deliberate bugs (wrong Rpc-Type, dropped failover, disabled
  origin check…) and confirming each was caught. If you add a defense, add a test
  that fails when the defense is stubbed out.

## Security — non-negotiable

- **Never commit `local/`.** It holds private keys. A `config/secrets_test.go`
  backstop fails the build if a 64-hex key reaches a tracked file — don't rely on
  it, but don't defeat it either.
- **Never print or log a key.** Emit a length, never the string.
- **Don't weaken the origin/host/CORS defaults** to make a test or an error pass.
  Read the README Security section first; those defaults each close a specific
  attack.

See `SECURITY.md` for reporting a vulnerability.

## Pull requests

- Branch off `main`. Keep PRs focused — one concern each.
- Run `make test`, `go vet ./...`, and `make lint` before pushing.
- Explain the **why** in the description, not just the what. If you changed a
  lifted file, say whether SAGE needs the same change.
- Every new config field must land in `config.schema.yaml` too, or
  `TestSchemaMatchesConfigStructs` fails. If you change the example endpoints,
  update all four documented places (`TestExampleEndpointsAreDocumentedEverywhere`
  enforces this).

## Commit messages

Short imperative subject, and a body that explains the reasoning when it isn't
obvious from the diff. The git history here favors *why over what* — match it.

## Code of Conduct

By participating you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).
