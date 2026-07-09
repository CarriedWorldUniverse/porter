// Package gitreplica is a namespace layer on top of internal/packstore that
// replicates a git repository into a pack store as a chain of incremental
// `git bundle` files, restorable with only a recovery key.
//
// Artifact naming convention inside a store, for a replica named "name":
//
//	git/<name>/bundle-<seq %08d>  - one git bundle per snapshot, seq from 0.
//	                                Bundle 0 is a full bundle ("--all");
//	                                every later bundle is incremental
//	                                (created with "--not <shas of the
//	                                previous manifest's refs>").
//	git/<name>/manifest           - JSON, OVERWRITTEN on every snapshot:
//	                                {format_version, name, seq, refs}. seq
//	                                is the sequence number of the LAST
//	                                bundle written; refs is the repo's
//	                                refs/heads + refs/tags as of that
//	                                snapshot (refname -> object sha). A
//	                                snapshot whose ref changes need no new
//	                                objects (deletions, backward moves)
//	                                writes a refs-only epoch: a new
//	                                manifest, no new bundle.
//
// A manifest overwrite only replaces the packstore artifact-name entry; the
// packstore Backend itself is still write-once (new pack/index/superblock
// objects each Commit) — see internal/packstore for that layering.
//
// Restore replays bundle-0..bundle-N in order (so every referenced commit
// exists), then makes the manifest's refs map authoritative: any local ref
// left over from an intermediate bundle that the manifest doesn't mention
// (e.g. a since-deleted branch) is deleted, and every manifest ref is
// force-set with `git update-ref`. Deleted/moved refs are therefore restored
// exactly as of the last snapshot; unreferenced objects may remain in the
// restored repo's packs, which is harmless (git's own model; no different
// from any repo with dangling commits).
//
// Deferred: signing the manifest. Integrity against silent corruption
// already comes from casket's AEAD (packstore's envelope layer) plus git's
// own content addressing (a corrupted bundle or a sha that doesn't resolve
// to a commit fails loudly on restore). What's NOT covered is a malicious
// store *writer* forging a manifest that references a rewritten history —
// out of scope for a single-owner store where the only writer holds the
// same key as the reader; revisit if the store ever gains multiple
// writers.
package gitreplica

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/CarriedWorldUniverse/porter/internal/packstore"
)

const formatVersion = 1

// manifest is the namespace layer's per-replica metadata, overwritten each
// snapshot.
type manifest struct {
	FormatVersion int               `json:"format_version"`
	Name          string            `json:"name"`
	Seq           int               `json:"seq"`
	Refs          map[string]string `json:"refs"`
}

// bundleArtifactName builds the packstore artifact name for one bundle in a
// replica's chain.
func bundleArtifactName(name string, seq int) string {
	return fmt.Sprintf("git/%s/bundle-%08d", name, seq)
}

// manifestArtifactName builds the packstore artifact name for a replica's
// manifest.
func manifestArtifactName(name string) string {
	return fmt.Sprintf("git/%s/manifest", name)
}

// Snapshot replicates repoPath's current refs into the store as one new
// incremental bundle (a full bundle when no manifest exists yet for name).
// When the repo's refs are byte-identical to the existing manifest's, it
// writes nothing and returns noChange=true.
func Snapshot(w *packstore.Writer, name, repoPath string) (seq int, refsCount int, noChange bool, err error) {
	refs, err := gitRefs(repoPath)
	if err != nil {
		return 0, 0, false, fmt.Errorf("gitreplica: snapshot %s: reading refs: %w", name, err)
	}

	mfName := manifestArtifactName(name)
	var prev *manifest
	if hasArtifact(w.Reader, mfName) {
		m, err := readManifest(w.Reader, mfName)
		if err != nil {
			return 0, 0, false, fmt.Errorf("gitreplica: snapshot %s: reading manifest: %w", name, err)
		}
		prev = &m
	}

	if prev != nil && refsEqual(prev.Refs, refs) {
		return prev.Seq, len(refs), true, nil
	}

	nextSeq := 0
	var excludeShas []string
	if prev != nil {
		nextSeq = prev.Seq + 1
		seen := make(map[string]bool, len(prev.Refs))
		for _, sha := range prev.Refs {
			if !seen[sha] {
				seen[sha] = true
				excludeShas = append(excludeShas, sha)
			}
		}
		sort.Strings(excludeShas)
	}

	bundleData, err := createBundle(repoPath, excludeShas)
	refsOnly := false
	if err != nil && isEmptyBundleErr(err) && len(excludeShas) > 0 {
		// The refs comparison above already established the refs genuinely
		// differ from the manifest, yet git found no new objects: every
		// current ref is reachable from a previously recorded tip (a branch
		// deleted, or a ref moved BACKWARD to an already-known ancestor).
		// The chain's cumulative closure therefore already holds every
		// object the new ref state needs, so record a refs-only epoch: a
		// new manifest, no bundle. Restore's per-ref cat-file check fails
		// loudly if this invariant were ever violated.
		refsOnly = true
		nextSeq = prev.Seq
		err = nil
	}
	if err != nil {
		if isEmptyBundleErr(err) {
			// Genuinely nothing to record (e.g. the repo has no refs at
			// all yet).
			if prev != nil {
				return prev.Seq, len(prev.Refs), true, nil
			}
			return 0, 0, true, nil
		}
		return 0, 0, false, fmt.Errorf("gitreplica: snapshot %s: creating bundle: %w", name, err)
	}

	if !refsOnly {
		w.Put(bundleArtifactName(name, nextSeq), bundleData)
	}

	mf := manifest{FormatVersion: formatVersion, Name: name, Seq: nextSeq, Refs: refs}
	mfJSON, err := json.Marshal(mf)
	if err != nil {
		return 0, 0, false, fmt.Errorf("gitreplica: snapshot %s: marshalling manifest: %w", name, err)
	}
	w.Put(mfName, mfJSON)

	if err := w.Commit(); err != nil {
		return 0, 0, false, fmt.Errorf("gitreplica: snapshot %s: commit: %w", name, err)
	}

	return nextSeq, len(refs), false, nil
}

// Restore materializes replica name into a new directory at outPath (which
// must not already exist): it clones bundle-0, fetches every subsequent
// bundle in order, then makes the manifest's refs map authoritative (extra
// refs deleted, every manifest ref force-set to its recorded sha) and checks
// out the manifest's HEAD branch if the clone had one and it still exists.
func Restore(r *packstore.Reader, name, outPath string) error {
	if _, err := os.Stat(outPath); err == nil {
		return fmt.Errorf("gitreplica: restore %s: %s already exists", name, outPath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("gitreplica: restore %s: stat %s: %w", name, outPath, err)
	}

	mfName := manifestArtifactName(name)
	mf, err := readManifest(r, mfName)
	if err != nil {
		return fmt.Errorf("gitreplica: restore %s: reading manifest: %w", name, err)
	}

	tmpDir, err := os.MkdirTemp("", "gitreplica-restore-")
	if err != nil {
		return fmt.Errorf("gitreplica: restore %s: making temp dir: %w", name, err)
	}
	defer os.RemoveAll(tmpDir)

	bundlePaths := make([]string, mf.Seq+1)
	for seq := 0; seq <= mf.Seq; seq++ {
		data, err := r.Get(bundleArtifactName(name, seq))
		if err != nil {
			return fmt.Errorf("gitreplica: restore %s: reading bundle %d: %w", name, seq, err)
		}
		p := filepath.Join(tmpDir, fmt.Sprintf("bundle-%08d", seq))
		if err := os.WriteFile(p, data, 0o600); err != nil {
			return fmt.Errorf("gitreplica: restore %s: writing bundle %d: %w", name, seq, err)
		}
		bundlePaths[seq] = p
	}

	if err := runGit("", "clone", "--", bundlePaths[0], outPath); err != nil {
		return fmt.Errorf("gitreplica: restore %s: clone bundle 0: %w", name, err)
	}

	// origBranch, if non-empty, is the branch the initial clone checked
	// out — recorded before we detach HEAD so refs can be freely rewritten.
	origBranch, _ := runGitOutput(outPath, "symbolic-ref", "--short", "HEAD")
	origBranch = strings.TrimSpace(origBranch)
	if err := runGit(outPath, "checkout", "--detach", "HEAD"); err != nil {
		return fmt.Errorf("gitreplica: restore %s: detaching HEAD: %w", name, err)
	}

	for seq := 1; seq <= mf.Seq; seq++ {
		if err := runGit(outPath, "fetch", bundlePaths[seq], "+refs/*:refs/*"); err != nil {
			return fmt.Errorf("gitreplica: restore %s: fetching bundle %d: %w", name, seq, err)
		}
	}

	existingRefs, err := gitRefs(outPath)
	if err != nil {
		return fmt.Errorf("gitreplica: restore %s: listing restored refs: %w", name, err)
	}
	for refname := range existingRefs {
		if _, ok := mf.Refs[refname]; ok {
			continue
		}
		if err := runGit(outPath, "update-ref", "-d", refname); err != nil {
			return fmt.Errorf("gitreplica: restore %s: deleting stale ref %s: %w", name, refname, err)
		}
	}

	refnames := make([]string, 0, len(mf.Refs))
	for refname := range mf.Refs {
		refnames = append(refnames, refname)
	}
	sort.Strings(refnames)
	for _, refname := range refnames {
		sha := mf.Refs[refname]
		if err := runGit(outPath, "cat-file", "-e", sha+"^{commit}"); err != nil {
			return fmt.Errorf("gitreplica: restore %s: ref %s -> %s does not resolve to a commit (chain corrupt?): %w", name, refname, sha, err)
		}
		if err := runGit(outPath, "update-ref", refname, sha); err != nil {
			return fmt.Errorf("gitreplica: restore %s: setting ref %s: %w", name, refname, err)
		}
	}

	if origBranch != "" {
		if _, ok := mf.Refs["refs/heads/"+origBranch]; ok {
			if err := runGit(outPath, "checkout", origBranch); err != nil {
				return fmt.Errorf("gitreplica: restore %s: checking out %s: %w", name, origBranch, err)
			}
		}
	}

	if err := runGit(outPath, "fsck", "--no-progress"); err != nil {
		return fmt.Errorf("gitreplica: restore %s: fsck: %w", name, err)
	}

	return nil
}

// hasArtifact reports whether name is present in r's store.
func hasArtifact(r *packstore.Reader, name string) bool {
	names := r.List()
	i := sort.SearchStrings(names, name)
	return i < len(names) && names[i] == name
}

// readManifest reads and decodes the manifest artifact mfName from r.
func readManifest(r *packstore.Reader, mfName string) (manifest, error) {
	data, err := r.Get(mfName)
	if err != nil {
		return manifest{}, err
	}
	var mf manifest
	if err := json.Unmarshal(data, &mf); err != nil {
		return manifest{}, fmt.Errorf("decoding manifest: %w", err)
	}
	if mf.Refs == nil {
		mf.Refs = map[string]string{}
	}
	return mf, nil
}

// refsEqual reports whether two refname->sha maps are identical.
func refsEqual(a, b map[string]string) bool {
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

// gitRefs returns repoPath's refs/heads and refs/tags as a refname->sha map.
func gitRefs(repoPath string) (map[string]string, error) {
	out, err := runGitOutput(repoPath, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads", "refs/tags")
	if err != nil {
		return nil, fmt.Errorf("listing refs: %w", err)
	}
	refs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("listing refs: unexpected for-each-ref line %q", line)
		}
		refs[fields[0]] = fields[1]
	}
	return refs, nil
}

// createBundle runs `git bundle create` in repoPath, excluding objects
// reachable from excludeShas (a full bundle when excludeShas is empty), and
// returns the resulting bundle file's bytes.
func createBundle(repoPath string, excludeShas []string) ([]byte, error) {
	tmpDir, err := os.MkdirTemp("", "gitreplica-bundle-")
	if err != nil {
		return nil, fmt.Errorf("making temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	bundlePath := filepath.Join(tmpDir, "bundle")
	args := []string{"bundle", "create", bundlePath, "--all"}
	if len(excludeShas) > 0 {
		args = append(args, "--not")
		args = append(args, excludeShas...)
	}
	if err := runGit(repoPath, args...); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(bundlePath)
	if err != nil {
		return nil, fmt.Errorf("reading bundle file: %w", err)
	}
	return data, nil
}

// isEmptyBundleErr reports whether err came from git refusing to create an
// empty bundle (all refs already covered by the exclusion set).
func isEmptyBundleErr(err error) bool {
	return strings.Contains(err.Error(), "empty bundle")
}

// runGit runs `git [-C repoPath] args...`, discarding stdout, and returns an
// error wrapping stderr on failure. repoPath == "" omits -C (for commands
// like clone that take the target directory as an argument instead).
func runGit(repoPath string, args ...string) error {
	_, err := runGitOutput(repoPath, args...)
	return err
}

// runGitOutput runs `git [-C repoPath] args...` and returns trimmed stdout,
// wrapping stderr into the error on failure.
func runGitOutput(repoPath string, args ...string) (string, error) {
	fullArgs := args
	if repoPath != "" {
		fullArgs = append([]string{"-C", repoPath}, args...)
	}
	cmd := exec.Command("git", fullArgs...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(fullArgs, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
