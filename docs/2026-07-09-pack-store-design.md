# porter pack store — metadata-opaque encrypted storage on untrusted providers

**Status:** v0 design, 2026-07-09. Experimental build in progress (see §Experimental build).
**Origin:** the fluster_rs concept — treat cloud storage as pooled unreliable media behind an allocation structure — reshaped for object-storage semantics.

## Goal

An encrypted store on untrusted cloud providers (Google Drive first; R2, local dock as mirrors) where the provider learns **nothing** about contents: no filenames, no file sizes, no file boundaries, no file count, no per-file access pattern. The provider sees only N uniform, opaquely-named ciphertext blocks.

Serves as the **media layer** under porter. Namespaces written into it (each just blobs in the store):
- porter backup artifacts (replacing the current cleartext `CarriedWorld-Porter/backups/<source>/<ts>.casket` paths, which leak source names + cadence)
- cairn encrypted bundle upstream (push-time-RPO offsite remote; GitHub demoted to optional projection)
- later, the parked porter object-FS, if its re-entry trigger fires

## Non-goals

- POSIX mount / FUSE. Nothing mounts this.
- In-place block mutation (FAT-style). Object stores have no partial writes; the design is log-structured instead.
- Traffic-analysis resistance (dummy writes, fixed-rate padding). Threat model is honest-but-curious provider, single owner. Timing/volume/churn still leak; accepted.
- Multi-writer. Single-writer per store (porter already owns a lease pattern if ever needed).

## Design: log-structured packs + generation-versioned index

Everything on the provider is write-once. Three object kinds, all casket envelopes, all opaquely named:

### 1. Packs (the "sectors")
- **Fixed size: 32 MiB ciphertext, exactly, padded.** A per-store constant in the superblock (`pack_size`), not a code constant. Rationale: Drive supports Range GET so reads don't need small packs; casket single-shot envelope wants ≪ RAM; padding costs ≤1 pack per sync epoch (~2–4s upload per cairn push at 32 MiB); 5TB full = ~164k objects (listable, GC-scannable); restic's 16 MiB default was driven by parallelism/memory constraints we don't share.
- Contents: concatenated **blobs** (see below), then random padding to exactly `pack_size`. Interior layout described only by the index — the pack itself needs no readable structure.
- Envelope: casket multi-recipient (cluster key + operator recovery key), pack-id in the AAD.
- Name on provider: random 128-bit hex. No counters, no timestamps in names. (Provider still sees object mtime — accepted leak, consistent with the timing non-goal.)

### 2. Blobs (the units of content)
- A blob = one chunk of plaintext. Artifacts larger than a chunk are split; artifacts smaller share packs.
- **Chunking: fixed 8 MiB** for v0 (simple, streams fine). Content-defined chunking (restic-style, better dedup on shifted data) is a format-versioned upgrade, not v0.
- **blob-id = SHA-256(plaintext chunk)** → content-addressed → duplicate chunks stored once, integrity check on read is the id itself (this is fluster's per-block CRC, but cryptographic and free).

### 3. Index generations + superblock (the "allocation table")
- Index: `blob-id → (pack-id, offset, length)` plus the namespace layer (`snapshot / ref name → ordered blob list`). Serialized, casket-sealed, written as a **new object per sync** (a generation), never mutated. Large indexes shard.
- Superblock: one tiny casket-sealed object naming the current index generation, `pack_size`, format version, store id. Written last in every sync. Provider-side "latest" is resolved by lexicographic generation-numbered name (`sb-<gen>-<random>`), so even the superblock is write-once; old generations are GC'd.
- **Crash consistency falls out:** packs upload first, index next, superblock last. A crash at any point leaves orphan packs/indexes that no superblock references — harmless, swept by GC. No fsck, no half-written table (the failure mode that kills an actual FAT here).

## Operations

- **Write (sync epoch):** batch pending blobs → fill packs → pad final pack → upload packs → upload index gen → upload superblock. cairn push = one small epoch; porter backup = one large one.
- **Read:** superblock → index → Range-GET blob extents from packs → verify SHA-256 → casket-open.
- **Delete/GC:** mark from live snapshots/refs; packs below a liveness threshold get their live blobs rewritten into new packs; then drop old packs + superseded index/superblock generations. Write amplification bounded by threshold choice.
- **Verify (fluster's drive-failure detection):** scheduled job samples packs per remote, decrypt + hash check, alert on failure — catches silent corruption or a locked account *before* restore day.
- **Mirror (fluster's pooling):** identical object set fanned to `targets: [drive, r2, local-dock]`. Any single healthy remote restores the store.

## Recovery property (must hold, inherited from NEX-625)

Recovery-key-only restore: operator recovery key + provider login + this spec ⇒ full restore. No cluster, no custodian, no live nexus. Every object is a self-describing casket envelope; the superblock bootstraps everything else. This property is the acceptance test.

## What the provider still learns (accepted leaks)

Total stored bytes, pack count, upload timing, churn rate, object mtimes. Mitigation is batching (fewer, larger epochs), nothing more.

## Relationship to existing pieces

- **casket:** the only crypto. Multi-recipient DEK wrap as proven in porter backups. 32 MiB packs deliberately avoid needing framed/streaming envelopes (that remains the separate forge-45GB work item — chunking at 8 MiB means even huge artifacts never need a big envelope here either).
- **porter backup chain:** custodian OAuth + Drive plumbing reused as the transport; the pack store replaces its cleartext folder layout.
- **cairn:** post-receive → incremental bundle → blob(s) in next epoch → refs manifest in the namespace layer. Signed refs manifest gives tamper-evidence on the upstream.
- **prior art consulted:** restic pack/index format (closest shape; differs: our fixed padded packs for opacity, casket instead of its crypto), git-remote-gcrypt (proves encrypted-remote UX), borg segments, s3backer+LUKS (the rejected block-device shape).

## Experimental build (v0 scale)

The experiment runs the same format at small scale — `pack_size` is a superblock constant, so no code differs:

- **`pack_size` = 2 MiB**, chunk size = 512 KiB (¼ of pack, same ratio as production 8/32). Multi-pack, multi-chunk, GC, and padding paths all get exercised with megabytes of test data.
- Lives in `internal/packstore` with a pluggable `Backend` interface: **local-dir backend first** (fast, deterministic tests), the existing `internal/drive` client wired as the second backend once the round-trip proves out.
- Crypto = the existing `internal/envelope` (casket multi-recipient) with pack-id/index-gen in the AAD path — no new crypto surface.
- Acceptance test = the recovery property: write blobs across epochs → tear down all state except the store objects + recovery key → restore, byte-identical.
- Production defaults (32 MiB / 8 MiB) become the store-creation defaults only after the experiment settles the format.

## Open questions

1. GC liveness threshold + schedule (cost/opacity tradeoff — GC epochs are visible churn).
2. Namespace-layer schema for cairn refs (signed manifest format; who holds the signing key — cluster vs operator).
3. Whether porter-backup migrates onto the pack store immediately or the two layouts run side-by-side until the store is proven.
4. Drive folder strategy: flat vs 2-level fanout dirs for the ~164k-object listing case (needs a measured LIST benchmark).
