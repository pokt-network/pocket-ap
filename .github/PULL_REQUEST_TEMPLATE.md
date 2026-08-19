<!-- Keep PRs focused: one concern each. See CONTRIBUTING.md. -->

## What & why

<!-- What does this change, and why? Explain the reasoning, not just the diff. -->

## Checklist

- [ ] `make test` passes (`-race` clean)
- [ ] `go vet ./...` and `make lint` pass
- [ ] No secrets or `local/` contents are committed; no key value is printed or logged
- [ ] New config fields are also in `config.schema.yaml` (and endpoint changes updated in all four documented places)
- [ ] If a **lifted** file changed, I noted in this PR whether SAGE needs the same change
- [ ] This does **not** add gateway smarts (QoS, reputation, per-chain parsing) — those belong in SAGE

## Notes for reviewers

<!-- Anything load-bearing or non-obvious a reviewer should know. -->
