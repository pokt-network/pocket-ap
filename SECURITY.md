# Security Policy

pocket-ap is unusual for a proxy: **it holds an application's private key and every
relay it sends spends that application's staked POKT.** The key *is* the app
identity — it is never a username-and-password that can be rotated behind an
account. That shapes both how vulnerabilities should be reported and how the tool
must be operated.

## Reporting a vulnerability

**Do not open a public issue for anything security-sensitive** — especially not
anything involving key exposure, unauthorized relaying, or stake drain.

Report privately through GitHub's [private vulnerability
reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)
on this repository (**Security → Report a vulnerability**). If you cannot use
that, contact the Pocket Network Foundation security team.

Please include:

- what an attacker can do, and what they need in order to do it;
- a minimal reproduction (config + request), **with all private keys redacted** —
  send a key length, never a key;
- affected version (`pocket-ap version`) and platform.

We aim to acknowledge within a few business days and to coordinate a fix and
disclosure timeline with you.

### Never include a private key in a report

If you believe a key has been exposed, treat it as compromised: **re-stake the app
with a fresh key** rather than sending the old one anywhere. A 64-hex secp256k1
key in an issue, a log paste, or a screenshot is itself the incident.

## Operational security model

Most of the risk here is operational, not a code bug. The design already defends
these; the failure mode is turning the defenses off. The threat is almost always a
**quota drain** — an attacker spending your stake — not data theft, because what
pocket-ap relays is public chain data.

| Rule | Why it matters |
| --- | --- |
| **Bind loopback (`127.0.0.1`), never `:8545` / `0.0.0.0`.** | A wider bind exposes the relay to your whole network; anyone on it can POST and spend your stake with no attack — they send no `Origin` and read as a native client. Loopback also switches on the `Host` check that closes DNS rebinding. |
| **Keep `local/` out of git.** | It holds private keys. It is gitignored; a `config/secrets_test.go` backstop fails the build if a 64-hex key reaches a tracked file — but that is a backstop, not permission to be careless. |
| **Never `allowed_origins: ["*"]` to silence an error.** | It hands every website the user visits the ability to relay with their key. WebSocket is *not* covered by the same-origin policy, so a permissive default is worse there. |
| **Prefer `POCKET_APP_PRIVATE_KEY` over the config file.** | Keeps the key out of any file at all. |
| **Never print or log the key.** | Not in errors, not in issues, not in a paste. Emit a length, never the string. |

The reasoning behind the `Origin`, `Host`, and CORS defaults is documented in the
README's **Security** section and in `CLAUDE.md`. If you find a way around any of
them, that is exactly the kind of report this policy is for.

## Scope

- **In scope:** anything that lets a third party relay with your key, exposes the
  key, bypasses the origin/host/CORS gates, or forges/replays a signed relay or a
  supplier response.
- **Out of scope:** the security of the upstream full node or relay miner you
  point at (report those to their projects), and supplier behavior on the network.
  Bugs in the lifted crypto usually exist upstream in SAGE too.
