#!/usr/bin/env node
// Wrapper shim. The real binary lives in a platform package that npm installed
// as an optional dependency; this finds it and hands over.
"use strict";

const { spawn } = require("child_process");

// Keys are `${process.platform} ${process.arch}` — Node's names, not Go's, which
// is why linux/amd64 appears here as "linux x64". The generator writes the same
// table into each platform package's os/cpu fields, and a test checks both
// against what goreleaser actually builds.
const PACKAGES = {
  "darwin arm64": "@pokt-network/pocket-ap-darwin-arm64",
  "darwin x64": "@pokt-network/pocket-ap-darwin-x64",
  "linux arm64": "@pokt-network/pocket-ap-linux-arm64",
  "linux x64": "@pokt-network/pocket-ap-linux-x64",
  "win32 x64": "@pokt-network/pocket-ap-win32-x64",
};

const key = `${process.platform} ${process.arch}`;
const pkg = PACKAGES[key];
if (!pkg) {
  console.error(
    `pocket-ap: no prebuilt binary for ${key}.\n` +
      `Supported: ${Object.keys(PACKAGES).join(", ")}.\n` +
      `Build from source instead: go install github.com/pokt-network/pocket-ap/cmd/pocket-ap@latest`
  );
  process.exit(1);
}

const exe = process.platform === "win32" ? "pocket-ap.exe" : "pocket-ap";
let binary;
try {
  binary = require.resolve(`${pkg}/bin/${exe}`);
} catch {
  // The platform packages are optionalDependencies, so npm skips them silently
  // when it cannot install one — leaving this shim present and the binary
  // absent. Say which package is missing rather than "ENOENT".
  console.error(
    `pocket-ap: ${pkg} is not installed.\n` +
      `It is an optional dependency, so an install run with --no-optional (or a\n` +
      `lockfile from another platform) leaves this wrapper without its binary.\n` +
      `Reinstall with optional dependencies enabled, or install ${pkg} directly.`
  );
  process.exit(1);
}

// spawn, not spawnSync: pocket-ap serve is a long-lived process, and a blocked
// parent cannot forward anything. Under Docker or systemd the stop signal
// arrives at node — if it is not passed on, shutdown waits for the timeout and
// then becomes a kill, which skips the proxy's own shutdown path.
const child = spawn(binary, process.argv.slice(2), { stdio: "inherit" });

for (const signal of ["SIGINT", "SIGTERM", "SIGHUP", "SIGQUIT"]) {
  process.on(signal, () => {
    if (child.exitCode === null && child.signalCode === null) {
      child.kill(signal);
    }
  });
}

child.on("error", (err) => {
  console.error(`pocket-ap: cannot run ${binary}: ${err.message}`);
  process.exit(1);
});

child.on("exit", (code, signal) => {
  if (signal) {
    // Report a signal death the way a shell does, so `$?` means what a caller
    // expects rather than collapsing every signal into exit 1.
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code === null ? 1 : code);
});
