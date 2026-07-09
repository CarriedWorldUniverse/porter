package gitreplica_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	casket "github.com/CarriedWorldUniverse/casket-go"

	"github.com/CarriedWorldUniverse/porter/internal/gitreplica"
	"github.com/CarriedWorldUniverse/porter/internal/packstore"
	"github.com/CarriedWorldUniverse/porter/internal/packstore/localdir"
)

// TestMain forces hermetic git config for every git invocation in this
// package's tests (helper repos and gitreplica's own shell-outs).
func TestMain(m *testing.M) {
	os.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	os.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	os.Exit(m.Run())
}

// keypair mints a fresh X25519 recipient keypair for a test.
func keypair(t *testing.T) (priv, pub []byte) {
	t.Helper()
	priv, pub, err := casket.GenerateRecipientKey()
	if err != nil {
		t.Fatalf("GenerateRecipientKey: %v", err)
	}
	return priv, pub
}

// newBackend opens a fresh localdir backend rooted at a fresh temp dir.
func newBackend(t *testing.T) *localdir.Dir {
	t.Helper()
	b, err := localdir.New(t.TempDir())
	if err != nil {
		t.Fatalf("localdir.New: %v", err)
	}
	return b
}

// runGit runs git -C dir args... and fails the test on error.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", fullArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(fullArgs, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// newRepo creates a fresh git repo at a temp dir with an initial commit on
// main, configured for hermetic, deterministic commits.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "test")
	return dir
}

// commitFile writes content to name in dir and commits it.
func commitFile(t *testing.T, dir, name, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	runGit(t, dir, "add", name)
	runGit(t, dir, "commit", "-q", "-m", msg)
}

// allRefs returns every refs/heads and refs/tags refname->sha in dir.
func allRefs(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := runGit(t, dir, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads", "refs/tags")
	refs := map[string]string{}
	if out == "" {
		return refs
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("unexpected for-each-ref line %q", line)
		}
		refs[fields[0]] = fields[1]
	}
	return refs
}

// artifactSize returns the plaintext size (index-recorded) of a store
// artifact, using the reader's List/Get is not enough to get the ORIGINAL
// bundle byte size without decrypting — Get already returns plaintext, so
// len(Get(name)) IS the bundle size.
func artifactSize(t *testing.T, r *packstore.Reader, name string) int {
	t.Helper()
	data, err := r.Get(name)
	if err != nil {
		t.Fatalf("Get(%s): %v", name, err)
	}
	return len(data)
}

func TestFullIncrementalRestore(t *testing.T) {
	src := newRepo(t)
	commitFile(t, src, "f", "1", "c1")
	commitFile(t, src, "f", "2", "c2")
	commitFile(t, src, "f", "3", "c3")
	runGit(t, src, "tag", "v1")

	storeDir := t.TempDir()
	b, err := localdir.New(storeDir)
	if err != nil {
		t.Fatalf("localdir.New: %v", err)
	}
	privA, pubA := keypair(t)
	privB, pubB := keypair(t)
	recipients := [][]byte{pubA, pubB}

	if _, err := packstore.Init(b, recipients, 2*1024*1024, 512*1024); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Re-open with a privKey (Init's own Writer has none — it can't read
	// back what it just wrote, matching the real CLI flow of init then
	// separately-keyed writes).
	w, err := packstore.OpenWriter(b, privA, recipients)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}

	seq0, refsCount0, noChange0, err := gitreplica.Snapshot(w, "myrepo", src)
	if err != nil {
		t.Fatalf("Snapshot (full): %v", err)
	}
	if seq0 != 0 {
		t.Fatalf("Snapshot (full): seq = %d, want 0", seq0)
	}
	if refsCount0 != 2 { // main + v1
		t.Fatalf("Snapshot (full): refsCount = %d, want 2", refsCount0)
	}
	if noChange0 {
		t.Fatalf("Snapshot (full): noChange = true, want false")
	}

	commitFile(t, src, "f", "4", "c4")
	commitFile(t, src, "f", "5", "c5")
	runGit(t, src, "checkout", "-q", "-b", "feature")
	runGit(t, src, "checkout", "-q", "main")

	seq1, refsCount1, noChange1, err := gitreplica.Snapshot(w, "myrepo", src)
	if err != nil {
		t.Fatalf("Snapshot (incremental): %v", err)
	}
	if seq1 != 1 {
		t.Fatalf("Snapshot (incremental): seq = %d, want 1", seq1)
	}
	if refsCount1 != 3 { // main + v1 + feature
		t.Fatalf("Snapshot (incremental): refsCount = %d, want 3", refsCount1)
	}
	if noChange1 {
		t.Fatalf("Snapshot (incremental): noChange = true, want false")
	}

	// Restore using ONLY privB's key (the recovery path).
	r, err := packstore.OpenReader(b, privB)
	if err != nil {
		t.Fatalf("OpenReader(privB): %v", err)
	}

	bundle0Size := artifactSize(t, r, "git/myrepo/bundle-00000000")
	bundle1Size := artifactSize(t, r, "git/myrepo/bundle-00000001")
	if bundle1Size >= bundle0Size {
		t.Fatalf("incremental bundle-1 (%d bytes) not smaller than full bundle-0 (%d bytes)", bundle1Size, bundle0Size)
	}

	out := filepath.Join(t.TempDir(), "restored")
	if err := gitreplica.Restore(r, "myrepo", out); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	wantRefs := allRefs(t, src)
	gotRefs := allRefs(t, out)
	if len(wantRefs) != len(gotRefs) {
		t.Fatalf("restored ref count = %d, want %d (src=%v restored=%v)", len(gotRefs), len(wantRefs), wantRefs, gotRefs)
	}
	for refname, sha := range wantRefs {
		gotSha, ok := gotRefs[refname]
		if !ok {
			t.Fatalf("restored repo missing ref %s", refname)
		}
		if gotSha != sha {
			t.Fatalf("ref %s: restored sha %s, want %s", refname, gotSha, sha)
		}
	}

	runGit(t, out, "fsck", "--no-progress")
}

func TestNoChangeSnapshotWritesNothing(t *testing.T) {
	src := newRepo(t)
	commitFile(t, src, "f", "1", "c1")

	storeDir := t.TempDir()
	b, err := localdir.New(storeDir)
	if err != nil {
		t.Fatalf("localdir.New: %v", err)
	}
	priv, pub := keypair(t)
	recipients := [][]byte{pub}

	if _, err := packstore.Init(b, recipients, 2*1024*1024, 512*1024); err != nil {
		t.Fatalf("Init: %v", err)
	}
	w, err := packstore.OpenWriter(b, priv, recipients)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	if _, _, _, err := gitreplica.Snapshot(w, "r", src); err != nil {
		t.Fatalf("Snapshot (first): %v", err)
	}

	r, err := packstore.OpenReader(b, priv)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	before := r.List()

	seq, _, noChange, err := gitreplica.Snapshot(w, "r", src)
	if err != nil {
		t.Fatalf("Snapshot (no-change): %v", err)
	}
	if !noChange {
		t.Fatalf("Snapshot (no-change): noChange = false, want true")
	}
	if seq != 0 {
		t.Fatalf("Snapshot (no-change): seq = %d, want 0 (unchanged)", seq)
	}

	r2, err := packstore.OpenReader(b, priv)
	if err != nil {
		t.Fatalf("OpenReader (after): %v", err)
	}
	after := r2.List()
	if len(before) != len(after) {
		t.Fatalf("store object count changed on no-change snapshot: before=%d after=%d", len(before), len(after))
	}
}

func TestRefDeletionAndMove(t *testing.T) {
	src := newRepo(t)
	commitFile(t, src, "f", "1", "c1")
	commitFile(t, src, "f", "2", "c2")
	runGit(t, src, "checkout", "-q", "-b", "doomed")
	runGit(t, src, "checkout", "-q", "main")

	storeDir := t.TempDir()
	b, err := localdir.New(storeDir)
	if err != nil {
		t.Fatalf("localdir.New: %v", err)
	}
	priv, pub := keypair(t)
	recipients := [][]byte{pub}

	if _, err := packstore.Init(b, recipients, 2*1024*1024, 512*1024); err != nil {
		t.Fatalf("Init: %v", err)
	}
	w, err := packstore.OpenWriter(b, priv, recipients)
	if err != nil {
		t.Fatalf("OpenWriter: %v", err)
	}
	if _, _, _, err := gitreplica.Snapshot(w, "r", src); err != nil {
		t.Fatalf("Snapshot (first): %v", err)
	}

	// Delete the branch, force-move main back one commit.
	runGit(t, src, "branch", "-D", "doomed")
	runGit(t, src, "update-ref", "refs/heads/main", "HEAD~1")

	before := len(w.List())
	if _, _, noChange, err := gitreplica.Snapshot(w, "r", src); err != nil {
		t.Fatalf("Snapshot (second): %v", err)
	} else if noChange {
		t.Fatalf("Snapshot (second): noChange = true, want false")
	}
	// Deletions/backward moves need no new objects: the snapshot must be a
	// refs-only epoch (manifest rewritten, no new bundle artifact).
	if after := len(w.List()); after != before {
		t.Fatalf("refs-only snapshot changed artifact count: %d -> %d (a new bundle was written?)", before, after)
	}

	r, err := packstore.OpenReader(b, priv)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	out := filepath.Join(t.TempDir(), "restored")
	if err := gitreplica.Restore(r, "r", out); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	wantRefs := allRefs(t, src)
	gotRefs := allRefs(t, out)
	if len(gotRefs) != len(wantRefs) {
		t.Fatalf("restored ref count = %d, want %d (restored=%v want=%v)", len(gotRefs), len(wantRefs), gotRefs, wantRefs)
	}
	for refname, sha := range wantRefs {
		if gotRefs[refname] != sha {
			t.Fatalf("ref %s: restored sha %s, want %s", refname, gotRefs[refname], sha)
		}
	}
	if _, ok := gotRefs["refs/heads/doomed"]; ok {
		t.Fatalf("restored repo still has deleted branch 'doomed'")
	}
}

func TestMultiReplicaIsolation(t *testing.T) {
	srcA := newRepo(t)
	commitFile(t, srcA, "f", "a1", "a-c1")
	commitFile(t, srcA, "f", "a2", "a-c2")

	srcB := newRepo(t)
	commitFile(t, srcB, "g", "b1", "b-c1")

	storeDir := t.TempDir()
	b, err := localdir.New(storeDir)
	if err != nil {
		t.Fatalf("localdir.New: %v", err)
	}
	priv, pub := keypair(t)
	recipients := [][]byte{pub}

	w, err := packstore.Init(b, recipients, 2*1024*1024, 512*1024)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, _, _, err := gitreplica.Snapshot(w, "repo-a", srcA); err != nil {
		t.Fatalf("Snapshot repo-a: %v", err)
	}
	if _, _, _, err := gitreplica.Snapshot(w, "repo-b", srcB); err != nil {
		t.Fatalf("Snapshot repo-b: %v", err)
	}

	r, err := packstore.OpenReader(b, priv)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}

	outA := filepath.Join(t.TempDir(), "restored-a")
	if err := gitreplica.Restore(r, "repo-a", outA); err != nil {
		t.Fatalf("Restore repo-a: %v", err)
	}
	outB := filepath.Join(t.TempDir(), "restored-b")
	if err := gitreplica.Restore(r, "repo-b", outB); err != nil {
		t.Fatalf("Restore repo-b: %v", err)
	}

	if got, want := allRefs(t, outA), allRefs(t, srcA); !refsMatch(got, want) {
		t.Fatalf("repo-a refs mismatch: got=%v want=%v", got, want)
	}
	if got, want := allRefs(t, outB), allRefs(t, srcB); !refsMatch(got, want) {
		t.Fatalf("repo-b refs mismatch: got=%v want=%v", got, want)
	}

	if !fileExists(filepath.Join(outA, "f")) {
		t.Fatalf("restored repo-a missing expected file 'f'")
	}
	if !fileExists(filepath.Join(outB, "g")) {
		t.Fatalf("restored repo-b missing expected file 'g'")
	}
}

func refsMatch(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
