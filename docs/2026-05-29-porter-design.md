# porter: a casket-encrypted, searchable S3-bucket-as-filesystem (low-IO / offsite)

**Date:** 2026-05-29
**Status:** Design — promoted from brainstorm. One decision **OPEN** (§6 hydration mechanism); several smaller knobs in §14. Pre-implementation-plan.
**Name:** **porter** — "one who carries" (`port-`/*portare*, to carry): the tool fetches and **carries** objects from the remote store to you locally. Lives in the CarriedWorldUniverse roster beside `nexus`/`cairn`/`casket`/`keel`/`anvil`.
**Repo:** `github.com/CarriedWorldUniverse/porter` (this repo).
**Scope note:** a **standalone CarriedWorldUniverse product** — *not* part of the cairn central-storage spec.
**Related:** `2026-05-29-cairn-central-storage-design.md` (reuses the same §7.1 casket per-object envelope; cairn's blob bucket is *one possible* mount target, but cairn does **not** depend on this tool — cairn accesses blobs by `s3://` reference). casket (`casket-go`/`-ts`/`-dotnet`, one wire format). nexus (lease/coordination, optional).

---

## 1. Goal & framing

A process you run to **mount an S3 bucket as a structured, searchable, locally-readable filesystem** — browse the tree, grep/search, read and occasionally edit files — with **optional** per-file casket encryption. Target profile: **low-IO, read-mostly, offsite storage** (mount-and-read, occasional whole-file edits). General-purpose; works on any S3-compatible bucket; reusable across hosts/aspects.

This is the **simple/safe end** of S3-FUSE. It is deliberately *not* a high-performance working filesystem, and deliberately distinct from cairn's git storage (high-IO, atomicity-critical → a real local disk; see the cairn spec §7.4). Different profile, different tool.

## 2. Non-goals (YAGNI)

- **Not** for git, databases, or any atomicity/locking-sensitive workload — it's object-granular (whole-file writes, no in-place partial writes/append/cross-file locking).
- **Not** a high-throughput FS — no JuiceFS-class metadata service, chunk maps, or prefetch.
- **Not** part of cairn; cairn reads blobs by reference.
- Encryption is **opt-in**, never mandatory (plain buckets just mount).

## 3. Architecture

```
   apps / aspects
        │  (read / edit / search)
   ┌────┴─────────────────────────────────────────────┐
   │ hydration layer  (FUSE | Win placeholder | daemon) │   ← §6 OPEN
   ├────────────────────────────────────────────────────┤
   │ local cache + write-staging   (exFAT directory)     │   ← §5
   ├────────────────────────────────────────────────────┤
   │ in-RAM manifest  (path → object metadata; search)   │   ← §4
   ├────────────────────────────────────────────────────┤
   │ casket crypto (opt-in, per-file AEAD = cairn §7.1)  │   ← §7
   └────────────────────────────────────────────────────┘
        │  S3 API (GET/PUT/LIST, conditional If-Match)
   ┌────┴─────────────────────────────────────────────┐
   │ S3 bucket  (objects, optionally casket-ciphertext) │
   └────────────────────────────────────────────────────┘
        │  lease (write exclusivity)
   nexus  (optional; §9)
```

The manifest answers metadata/search with zero S3 round-trips; only file *content* touches S3 (hydrate on read, PUT on check-in). S3 is the source of truth; everything local is cache.

## 4. In-RAM manifest

A RAM-resident index of the bucket: `path → { s3key, etag, version-id, size, content-type, encrypted?, dirty?, hydrated→cache-path }`. Backs `readdir`/`getattr`/`lookup` and search with no S3 calls.

**Refresh (plain S3 has no native change-feed — pick deliberately):**
- **Periodic `LIST` + diff** — *default*; zero bucket config, any S3-compatible store; staleness = poll interval; cost scales with object count (per-prefix + paginate). Best fit for low-IO/offsite (poll lazily).
- **S3 Event Notifications → SQS** — *opt-in freshness upgrade*; per-object deltas, near-real-time; needs bucket config + a queue; events are at-least-once/reorderable → dedupe by key+version/etag.
- **Self-writes** update the manifest directly.

S3 stays authoritative → on a manifest miss / etag-mismatch, fall back to a live `HEAD`/`GET` to self-heal. Memory scales with object count: fine for modest offsite buckets; persist a manifest snapshot for warm cold-start, page for huge buckets. If names are encrypted, the manifest holds the **decrypted** structure (plaintext-in-RAM; S3 stays blind).

## 5. Local cache & materialization (exFAT)

A local **exFAT directory** is the cache + write-staging medium. **Why exFAT:** cross-platform (Windows/macOS/Linux/removable media), and its lack of POSIX features (perms, symlinks, hardlinks, xattrs) *matches* S3's flat object model — you're not faking semantics the backend can't represent.

- **Dirty vs clean:** clean entries are evictable cache (LRU; re-hydrate on next read); dirty entries are pending write-back.
- **Caveats:** no xattrs → per-file etag/version/casket-keyref live in the **manifest**, not on the file. exFAT is **case-insensitive** but S3 keys are **case-sensitive** → the manifest must detect/disambiguate case-fold collisions (else silent corruption). Write **temp-then-rename** so a torn write never poisons the cache.

## 6. Hydration layer — OPEN decision

"Fetch the file on read/open" = **on-demand hydration**. A plain directory can't trigger fetch-on-open by itself; this needs a layer:

- **FUSE** (Linux/macOS) — transparent `open()`→fetch. Gives the reliable lifecycle hooks: `open` = check-out/hydrate, `flush`/`fsync` = write-back hints, **`release` (last close) = check-in**, refcounted across concurrent opens. `release` can be skipped on a hard crash → also commit-on-`fsync` + a dirty-cache recovery pass on mount. **Lead candidate.**
- **Windows Cloud Files / placeholder API** — the "Files On-Demand" mechanism (what OneDrive uses) for native Windows hydration.
- **Explicit / sync-daemon** — hydrate on access via a watcher, or an explicit `hydrate <path>`. Simplest, least transparent; no FUSE dependency.

**[OPEN]** — choose per platform; FUSE for Linux/macOS is the lead, placeholder API for Windows, with the sync-daemon as a fallback where neither is wanted.

> **Note:** the OS does **not** take a file lock on `open()` (locks are advisory + opt-in; most apps never lock). So the lifecycle hook is **open/`release`**, *not* lock-release. Locks are for exclusivity (§9), not the commit trigger.

## 7. Encryption — opt-in, per-file

When enabled, each object is **independently sealed with the casket per-file AEAD envelope** (the cairn spec §7.1 format — suite-first versioned fixed-width descriptor, `keyref` = casket fingerprint, framed body for large files). **Per-file AEAD, never disk/XTS** — these are discrete objects, not block-device sectors.

- Cross-language via `casket-go`/`-ts`/`-dotnet` (one wire format).
- **Decrypt on hydrate, (re-)seal on write-back.** Keys supplied from nexus / casket identity (off-host).
- **Filetype is decided on the plaintext** (sniff after decrypt, or carry the plaintext content-type in the manifest/descriptor) — needed for search and merge decisions.
- Filenames/paths *may* be encrypted (then the manifest holds the decrypted tree).
- Plain (unencrypted) buckets just mount — encryption is a feature you turn on.

## 8. Read / edit / write-back lifecycle — check-out → edit-local → atomic versioned check-in

- **Read/open** → fetch via the reference into the exFAT cache (hydrate on miss; decrypt if sealed).
- **Edit** → all on the local copy (normal POSIX edits, fast, no per-write streaming).
- **Check-in** (on `release`/last-close, or an explicit "save version") → (re-)seal if encrypted → **conditional `PUT If-Match: <etag recorded at check-out>`** → new object version → update manifest.

This **sidesteps S3's no-partial-writes entirely** (never mutate in place; always PUT a complete new version). `PUT` is atomic per object; with bucket **Versioning** on, each write-back is a retained immutable version (history/rollback). **Never HEAD-then-PUT** (TOCTOU) — the conditional PUT is the atomic compare-and-swap.

## 9. Concurrency — single-writer / non-blocking readers

The local-edit + atomic-check-in design gives **MVCC-style isolation for free**: the writer edits a *local* copy, S3 holds the last committed version until check-in, so **readers always read committed state** and never block (and aren't blocked by) the writer — they see the writer's changes only after check-in (via the next manifest refresh).

- **Write lock = a single exclusive *lease*** acquired **lazily on first edit** (not on open). Cross-machine → held in **nexus** (the coordination authority), *not* an OS lock. A second would-be editor is **denied → read-only / queue / notify**.
- **Read "lock" = shared, lightweight, non-exclusionary** — not for mutual exclusion (reads never block); its jobs are to **pin the version** against lifecycle GC while reading and **register for staleness notification**. Many readers coexist with the write-lease holder.
- **Lease robustness:** TTL + heartbeat/renewal → a crashed holder's lease **auto-expires** (else locked forever). nexus holds the lease table + expiry.
- **`If-Match` backstop** even with the lease — closes the stale-lease race (lease expires → stolen → original holder's delayed check-in `412`s instead of clobbering). *Lease = single-writer UX; If-Match = correctness.*
- **Default = single-writer lease** (the required model: one editor at a time), with `If-Match` as the correctness backstop. **Pure-optimistic** (`If-Match` only, no lease) is the **fallback** when no nexus/lease backend is available — it still prevents silent clobber (conflicts surface as `412`), just without up-front exclusivity.

## 10. Conflict resolution & merge

When a check-in finds the base moved (`412` / stale base) and the filetype supports it, offer a **3-way merge**. Inputs already exist: **base** = the version checked out (record **version-id at check-out**; fetchable by id with Versioning on — *this is what enables merge*), **ours** = local edits, **theirs** = current committed.

- **Per-filetype merge-strategy registry:**
  - **Mergeable** (plain text / source / markdown / structured text) → 3-way line merge: **auto-resolve non-overlapping hunks**, surface only overlapping conflicts to the operator (git-style markers / merge view).
  - **Non-mergeable** (images, builds, opaque, and **rich `.docx`** = zip-of-XML → line-merge corrupts) → **overwrite-theirs / keep-mine / save-as-new / abort**. Rich-doc merge = a pluggable format-aware merger, out of initial scope.
- **Reuse `diff3` / `git merge-file` / a merge lib — don't build the engine.**
- Filetype decided on the **plaintext** (§7).
- The merged result **re-enters the check-in loop** (`PUT If-Match` against the version merged-against; another race → re-merge).
- **Operator vs autonomous:** clean auto-merge → no human; overlapping conflicts → **escalate to the operator**; non-mergeable → policy/escalate.
- **Caveat:** textually-clean ≠ semantically-correct — optionally flag auto-merged versions for review on important files.

## 11. Versioning awareness

3-way merge **and** clobber-recovery require bucket **Versioning ON**. Precise degradation without it: conflict *detection* (`If-Match`/ETag) still works, but you lose (a) the base → no 3-way merge, and (b) recoverability → overwrites are **permanent**.

- **On mount, `GetBucketVersioning`.** If **off** → **warn + offer to enable** (`PutBucketVersioning`, if creds permit) **paired with a noncurrent-version expiration lifecycle** (else versions accumulate = cost). If creds can't enable or the backend lacks versioning → warn that merge + recovery are unavailable, degrade gracefully.
- **Scope the warning to the write path** — a read-only mount of an unversioned bucket is harmless (don't nag); warn prominently before the first edit/check-in (optional explicit ack, since that save is permanent).
- **Mitigation when off:** pin the checked-out base in the local cache (don't evict while checked-out/dirty) → 3-way merge still works for *your in-flight* edits (you lose only global history + clobber recovery).
- Mount records a `versioned?` capability flag; merge/conflict logic downgrades on false.

## 12. Search

Local search over the manifest: **name/path search** is direct (on the keys, unless names are encrypted). **Content search** of encrypted files = **decrypt locally → index locally** (the plaintext search index stays on your machine; S3 stays blind). Unencrypted buckets index trivially.

## 13. State & capability model

- **Manifest entry:** `path, s3key, etag, version-id, size, content-type, encrypted?, dirty?, hydrated→cache-path, pinned-base-version?`.
- **Mount capabilities:** `versioned?`, `encryption-on?`, `lease-backend (nexus | none)`, `event-refresh? (SQS | poll)`. Conflict/merge/concurrency logic consults these and degrades gracefully.

## 14. Open decisions

- **[OPEN] Hydration mechanism per platform** (§6): FUSE (Linux/mac, lead) / Windows placeholder API / sync-daemon.
- **Commit point:** `release` (last-close) vs explicit "save version" — lean `release` + debounce, with explicit-save available.
- **Denied-write UX** (§9): read-only / queue / notify when the write lease is held.
- **Rich-document merger** (§10): pluggable format-aware merger — deferred.
- **Manifest persistence / cold-start** + paging strategy for very large buckets.
- **Lease representation in nexus** (table schema, TTL/heartbeat cadence, expiry reclaim).
- **Product name** and **its own repo**.

## 15. Relationship to the stack

- **casket** — the crypto substrate (per-file AEAD envelope, cross-language). Encryption is opt-in.
- **nexus** — optional; hosts the write lease + (if used) staleness notifications. Without nexus the tool runs in pure-optimistic mode (`If-Match` only).
- **cairn** — independent. cairn's Class-B blob bucket is one bucket this tool *could* mount, but cairn does not require it (cairn uses `s3://` references).

## 16. Phasing (rough)

- **P1** — read-only mount: manifest + `LIST`-refresh + cache + hydration (unencrypted); versioning-awareness warnings.
- **P2** — opt-in casket per-file encryption (decrypt-on-hydrate; manifest holds decrypted structure).
- **P3** — write/edit + atomic versioned check-in + `If-Match`.
- **P4** — concurrency (nexus lease + non-blocking reads) + conflict merge (mergeable types).
- **P5** — content search index; event-based (SQS) refresh; manifest persistence.

Each phase its own implementation plan.

## 17. Operational safety (implementation sessions)

A live Claude Code bug bricks extended-thinking sessions on tool-use turns (spans ≥2.1.147 through 2.1.154; MCP especially). Build work here is long and tool-dense → run on **pinned CC 2.1.146, extended thinking OFF for mechanical work, MCP avoided (prefer `gh`/shell), committing per step.**
