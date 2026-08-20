// Builds the npm packages for a release out of the binaries goreleaser already
// produced. Run after `goreleaser release` (or `--snapshot`), from the repo root:
//
//   node npm/generate.mjs --version 0.1.2 --dist dist --out npm/build
//
// Shape: one wrapper package (`pocket-ap`) that people install, plus one package
// per platform holding nothing but the binary. The wrapper lists the platform
// packages as optionalDependencies with `os`/`cpu` set, so npm installs exactly
// the one that matches and silently skips the rest.
//
// Why not a postinstall that downloads from the GitHub release: that is the
// other common shape, and it breaks under `--ignore-scripts` — which pnpm
// defaults to and plenty of companies enforce — as well as offline and behind
// proxies. This shape has no install-time network and no scripts at all; the
// tarball npm already cached IS the artifact. It is what esbuild, swc, biome and
// turbo all do.
//
// Node only, no dependencies: this runs in a release job where adding an npm
// install step to publish an npm package is a circularity worth avoiding.

import { execFileSync } from "node:child_process";
import { copyFileSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { basename, join } from "node:path";

const SCOPE = "@pokt-network";
const WRAPPER = "pocket-ap";
const REPO = "https://github.com/pokt-network/pocket-ap";
const DESCRIPTION =
  "Self-hosted, drop-in RPC proxy for Pocket Network's Shannon protocol";

// Go's GOOS/GOARCH on the left, Node's process.platform/process.arch on the
// right. They disagree on two of the five (amd64 is x64, windows is win32),
// which is the entire reason this table is written down once and checked by a
// test rather than spelled out in each package.
const TARGETS = [
  { goos: "darwin", goarch: "arm64", os: "darwin", cpu: "arm64" },
  { goos: "darwin", goarch: "amd64", os: "darwin", cpu: "x64" },
  { goos: "linux", goarch: "arm64", os: "linux", cpu: "arm64" },
  { goos: "linux", goarch: "amd64", os: "linux", cpu: "x64" },
  { goos: "windows", goarch: "amd64", os: "win32", cpu: "x64" },
];

function arg(name, fallback) {
  const i = process.argv.indexOf(`--${name}`);
  if (i !== -1 && process.argv[i + 1]) return process.argv[i + 1];
  if (fallback !== undefined) return fallback;
  throw new Error(`missing required --${name}`);
}

// The version comes from the tag, never from a file: a package.json version that
// disagreed with the binary's `pocket-ap version` would make a bug report point
// at the wrong source.
const version = arg("version").replace(/^v/, "");
const distDir = arg("dist", "dist");
const outDir = arg("out", "npm/build");

// goreleaser writes artifacts.json describing everything it produced. Reading it
// beats globbing dist/: the directory names carry GOAMD64/GOARM64 suffixes
// (pocket-ap_linux_arm64_v8.0) that change with build settings, and a glob that
// silently matched nothing would publish an empty package.
const artifacts = JSON.parse(
  readFileSync(join(distDir, "artifacts.json"), "utf8")
);

function binaryFor(target) {
  const found = artifacts.filter(
    (a) =>
      a.type === "Binary" &&
      a.goos === target.goos &&
      a.goarch === target.goarch
  );
  if (found.length !== 1) {
    throw new Error(
      `expected exactly 1 binary for ${target.goos}/${target.goarch} in ${distDir}/artifacts.json, found ${found.length}`
    );
  }
  return found[0].path;
}

const common = {
  version,
  description: DESCRIPTION,
  license: "MIT",
  homepage: REPO,
  repository: { type: "git", url: `git+${REPO}.git` },
  bugs: { url: `${REPO}/issues` },
};

const optionalDependencies = {};

for (const target of TARGETS) {
  const name = `${SCOPE}/${WRAPPER}-${target.os}-${target.cpu}`;
  const dir = join(outDir, `${WRAPPER}-${target.os}-${target.cpu}`);
  const exe = target.goos === "windows" ? "pocket-ap.exe" : "pocket-ap";

  mkdirSync(join(dir, "bin"), { recursive: true });
  const src = binaryFor(target);
  copyFileSync(src, join(dir, "bin", exe));
  // The mode has to be set explicitly: npm records the executable bit from the
  // file on disk, and copyFileSync from a checkout or an artifact download does
  // not reliably carry it.
  execFileSync("chmod", ["755", join(dir, "bin", exe)]);

  writeFileSync(
    join(dir, "package.json"),
    JSON.stringify(
      {
        name,
        ...common,
        // os/cpu are what let npm skip the four packages that do not apply. They
        // are advisory for a manual install and decisive for an optional
        // dependency, which is how the wrapper depends on them.
        os: [target.os],
        cpu: [target.cpu],
        files: ["bin"],
        // No "bin": these packages are payload. Exposing five identically named
        // binaries would have npm link whichever it installed last.
        preferUnplugged: true,
      },
      null,
      2
    ) + "\n"
  );
  writeFileSync(
    join(dir, "README.md"),
    `# ${name}\n\nThe ${target.os}/${target.cpu} binary for [\`${WRAPPER}\`](https://www.npmjs.com/package/${WRAPPER}).\n\nInstall \`${WRAPPER}\` instead — npm picks the right one of these for you.\n`
  );

  optionalDependencies[name] = version;
  console.log(`${name} <- ${basename(src)}`);
}

const wrapperDir = join(outDir, WRAPPER);
mkdirSync(join(wrapperDir, "bin"), { recursive: true });
copyFileSync("npm/bin/pocket-ap.js", join(wrapperDir, "bin", "pocket-ap.js"));
execFileSync("chmod", ["755", join(wrapperDir, "bin", "pocket-ap.js")]);
copyFileSync("README.md", join(wrapperDir, "README.md"));
copyFileSync("LICENSE", join(wrapperDir, "LICENSE"));

writeFileSync(
  join(wrapperDir, "package.json"),
  JSON.stringify(
    {
      name: WRAPPER,
      ...common,
      keywords: ["pocket", "pokt", "rpc", "proxy", "web3", "shannon", "relay"],
      bin: { [WRAPPER]: "bin/pocket-ap.js" },
      files: ["bin", "README.md", "LICENSE"],
      // Node 16 is where the shim's syntax and child_process behaviour are safe.
      engines: { node: ">=16" },
      optionalDependencies,
    },
    null,
    2
  ) + "\n"
);
console.log(`${WRAPPER} (wrapper) -> ${wrapperDir}`);
