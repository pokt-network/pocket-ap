package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// secp256k1 private keys are exactly 32 bytes, so a committed one shows up as a
// 64-character hex run. The word boundaries keep this off longer hex strings —
// commit SHAs are 40, and session IDs, block hashes and pubkeys quoted in docs
// are different lengths or appear inside longer tokens.
var hexKeyPattern = regexp.MustCompile(`\b[0-9a-fA-F]{64}\b`)

// allowedHexKeyMatches are known-benign 64-hex strings. Add an entry with a
// reason rather than loosening the pattern, so the next 64-hex string still has
// to be justified by a human.
var allowedHexKeyMatches = map[string]string{
	"4bd7f2e1a9c3068b5d4f7e2a1c9b8d6e3f5a7c2b4d6e8f0a1c3e5b7d9f2a4c6e": "throwawayKey in pocket/signer_key_test.go — made up, never staked",
	"1111111111111111111111111111111111111111111111111111111111111111": "all-ones fixture proving two keys derive two addresses",
}

// ⚠️ Do NOT try to reproduce this scan with `git grep -E '\b...\b'`. git's ERE is
// POSIX and does not support \b, so the pattern matches NOTHING and the scan
// reports a clean repo no matter what is in it — a security check that fails
// open. This bit us on 2026-07-22: two audits "passed" before a positive control
// showed they were matching zero lines. Go's regexp does support \b, which is
// why this test works; if you need a shell equivalent use
// `git grep -o -E '[0-9a-fA-F]{64,}'` piped through `awk 'length($0)==64'`.

// A key in a tracked file is unrecoverable: git history is forever, and this
// repo is public, so the moment it lands the key must be rotated rather than
// removed. The whole workflow depends on keys living in local/ (gitignored) or
// POCKET_APP_PRIVATE_KEY, and that convention is one careless `git add -A` away
// from being broken.
//
// Scans tracked files only. Untracked and ignored files are exactly where keys
// are SUPPOSED to be, so flagging them would train people to ignore this test.
func TestNoSecretsInTrackedFiles(t *testing.T) {
	files := trackedFiles(t)
	if len(files) < 10 {
		t.Fatalf("only %d tracked files found; the scan is not seeing the repo", len(files))
	}

	// Prove the pattern still matches what it is meant to catch. Without this the
	// test would pass just as happily if the regex were broken, which is the one
	// failure mode a green security check must not have.
	if !hexKeyPattern.MatchString(strings.Repeat("ab", 32)) {
		t.Fatal("hexKeyPattern no longer matches a 64-hex key; the scan is broken")
	}

	// Scans the WORKING TREE copy of each tracked file, not HEAD. Reading HEAD
	// would skip a newly added file until the commit that introduces it has
	// already landed — which is precisely too late, since the point is to stop a
	// key entering history at all. Working-tree content also covers staged edits.
	for _, f := range files {
		content, err := os.ReadFile(filepath.Join("..", f))
		if err != nil {
			continue // tracked but deleted locally; nothing to leak
		}
		checkForKeys(t, f, string(content))
	}
}

func checkForKeys(t *testing.T, file, content string) {
	t.Helper()
	for _, match := range hexKeyPattern.FindAllString(content, -1) {
		if reason, ok := allowedHexKeyMatches[match]; ok {
			t.Logf("%s: allowlisted 64-hex string (%s)", file, reason)
			continue
		}
		// Deliberately does NOT print the match: this test's whole subject is a
		// secret, and echoing it into CI logs would do the leaking itself.
		t.Errorf("%s contains a 64-character hex string, which is the shape of a "+
			"secp256k1 private key. Keys belong in local/ (gitignored) or "+
			"POCKET_APP_PRIVATE_KEY. If it is genuinely not a key, add it to "+
			"allowedHexKeyMatches with a reason.", file)
	}
}

func trackedFiles(t *testing.T) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", "..", "ls-files").Output()
	if err != nil {
		t.Skipf("git not available or not a repo: %v", err)
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files
}

// local/ is where real keys live, so its exclusion from git is a security
// control, not a preference. A .gitignore edit that dropped it would be silent.
func TestLocalDirIsGitIgnored(t *testing.T) {
	// check-ignore exits 0 when the path IS ignored.
	cmd := exec.Command("git", "-C", "..", "check-ignore", "-q", "local/beta-config.yaml")
	if err := cmd.Run(); err != nil {
		t.Error("local/ is not gitignored — configs holding app private keys would be committable")
	}
}
