# npm packaging

Scaffolding for publishing pocket-ap to npm. **Nothing here is published yet** —
it is waiting on an npm organisation and a token, listed under "What a human
still has to do" below. Everything else is built and tested.

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

## What a human still has to do

1. **Create the `@pokt-network` organisation on npmjs.com.** Free for public
   packages. The scope is what appears in every package name; a personal username
   only shows as publisher metadata.
2. **Create a granular access token** with write access to `pocket-ap` and
   `@pokt-network/pocket-ap-*`, and add it as the `NPM_TOKEN` repository secret.
   A classic token with 2FA-required-on-publish cannot be used from CI.
3. **Run `npm-token-check.yml`** (`gh workflow run npm-token-check.yml`) to
   confirm the token works — before a tag depends on it. This exists because the
   Homebrew tap credential was first exercised by tagging, and that failure cost a
   version number that can never be reused.

Until then the release workflow's `npm` job publishes nothing and says so; it
cannot fail a release.

Both names are currently unregistered: `pocket-ap` and `@pokt-network/pocket-ap`.
The wrapper deliberately takes the unscoped name — it is what people type — while
the payload packages stay scoped.
