# npm packaging

Scaffolding for publishing pocket-ap to npm. **Nothing here is published, and
publishing is DEFERRED — decided 2026-08-20.** The code is written and verified
against a real build; what is missing is a reason and a home, not work.

## Why it is deferred

Read this before picking it up again, because the obvious reasoning ("we target
JS/TS developers, so we should be on npm") is what put it on the roadmap in the
first place.

- **pocket-ap is a daemon, not a library.** Nobody adds a long-running proxy to
  their `dependencies`. The npm-shaped use case is a dev tool — and `brew
  install pocket-ap`, the tarballs and `docker run` already serve it. What npm
  would genuinely add is `npx pocket-ap call …`, a zero-install one-shot.
- **~104 MB per platform in `node_modules`**, which lands in every CI install of
  any project that depends on it.
- **Six packages per release, version-locked**, plus a per-package trusted
  publisher configuration.
- **The credential has no good home, and that is the deciding reason.** Both
  `@pokt-network` and `@pokt-foundation` exist on npm, both were this project's,
  and access to neither survived the people who created them — their packages are
  maintained by individual accounts from earlier eras. Publishing from someone's
  personal account would recreate exactly that: one person holding a
  project-critical name, stranded the moment they move on.

## When to revisit

Either of these makes it worth finishing — until then, nothing here needs doing:

1. **The `@pokt-network` org is recovered** (npm support handles abandoned-org
   disputes; the strong evidence is control of the domain, the GitHub org, and
   the repository the packages point at). Then it publishes into the right home
   with no personal account involved, and the package names in `generate.mjs`
   already match.
2. **A user actually asks for npm.** Today the demand is inferred, not reported.

The release workflow's `npm` job already skips and says so when no credential is
present, so leaving this dormant breaks nothing.

## Shape

One wrapper package plus one package per platform, the layout esbuild, swc, biome
and turbo all use:

```
pocket-ap                                  the package people install
  └─ optionalDependencies
       @pokt-network/pocket-ap-darwin-arm64   os: darwin, cpu: arm64
       @pokt-network/pocket-ap-darwin-x64     os: darwin, cpu: x64
       @pokt-network/pocket-ap-linux-arm64    os: linux,  cpu: arm64
       @pokt-network/pocket-ap-linux-x64      os: linux,  cpu: x64
       @pokt-network/pocket-ap-win32-x64      os: win32,  cpu: x64
```

Each platform package contains the binary and nothing else. npm reads `os`/`cpu`
and installs only the one that matches, silently skipping the rest — so a user
downloads roughly 30 MB, not five platforms' worth.

**Why not a `postinstall` that downloads from the GitHub release.** That is the
other common shape and it has three failure modes this one does not: it breaks
under `--ignore-scripts`, which pnpm defaults to and many companies enforce; it
needs network at install time, so it fails offline and behind proxies; and it has
to verify checksums itself, because a download step that skips that trusts the
network completely. Here the tarball npm already cached *is* the artifact.

## Building

Requires binaries from goreleaser, so run it after a build:

```sh
goreleaser build --clean --snapshot
node npm/generate.mjs --version 0.1.2 --dist dist --out npm/build
```

The generator reads `dist/artifacts.json` rather than globbing `dist/`, because
the directory names carry `GOAMD64`/`GOARM64` suffixes (`pocket-ap_linux_arm64_v8.0`)
that move with build settings — and a glob that silently matched nothing would
publish an empty package.

To try the result before publishing anything, arrange it the way npm would:

```sh
mkdir -p /tmp/sim/node_modules/@pokt-network
cp -R npm/build/pocket-ap                /tmp/sim/node_modules/pocket-ap
cp -R npm/build/pocket-ap-darwin-arm64   /tmp/sim/node_modules/@pokt-network/pocket-ap-darwin-arm64
node /tmp/sim/node_modules/pocket-ap/bin/pocket-ap.js version
```

## The wrapper shim

`bin/pocket-ap.js` resolves the platform package and hands over. Two details that
are not cosmetic:

- **It uses `spawn`, not `spawnSync`.** `pocket-ap serve` is long-lived, and a
  blocked parent cannot forward anything. Under Docker or systemd the stop signal
  arrives at node; if node does not pass it on, shutdown waits for the timeout and
  then becomes a kill, which skips the proxy's own shutdown path.
- **A missing platform package is reported by name.** They are optional
  dependencies, so npm skips them silently when it cannot install one — leaving
  the wrapper present and the binary absent. Without this the user sees `ENOENT`
  on a path they have never heard of.

`TestNPMTargetsMatchGoreleaser` checks the platform list here against the targets
in `.goreleaser.yaml`, in both directions. The mapping is written down three times
— goreleaser's `goos`/`goarch`, the generator's `TARGETS`, the shim's `PACKAGES`
— and Node disagrees with Go on two of the five names (`amd64` is `x64`,
`windows` is `win32`), so the copies cannot be diffed by eye.

## What publishing would take, if it is picked up

1. **Somewhere to publish.** `@pokt-network` cannot simply be created — it already
   exists and is not ours to use any more (see above). Recovering it is the good
   outcome; publishing everything unscoped (`pocket-ap`, `pocket-ap-<os>-<cpu>`,
   all of which were free as of 2026-08-20) is the workaround, and needs one
   constant changed in `generate.mjs` and the shim's `PACKAGES` table.
2. **Trusted Publishing rather than a token.** GitHub Actions authenticates over
   OIDC, so there is no secret to store, rotate or leak, and provenance is
   automatic. It needs Node ≥ 22.14 and npm ≥ 11.5.1 — the workflow currently
   requests Node 20 — and is configured **per package** on npmjs.com (org, repo,
   workflow filename, allowed action). npm's docs do not say whether a package
   that has never been published can be configured, and the setting lives on a
   package's settings page, so plan on one manual publish first.
3. **The publish step keys off `NPM_TOKEN`**, which does not exist under OIDC. It
   would need a different switch — a repository *variable* rather than a secret,
   so the job stays skipped until the setup is deliberately turned on instead of
   failing every release until then.

`npm-token-check.yml` is the dispatchable credential probe, for the same reason
the Homebrew tap has one: that credential was first exercised by tagging, on a
public tag, and it cost a version number that can never be reused.
