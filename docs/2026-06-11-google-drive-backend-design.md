# porter: pluggable backends + Google Drive (shed storage + offsite backup)

**Status:** spec (operator-directed 2026-06-11). Refocuses porter's first backend from S3 to Google Drive; keeps the backend pluggable.
**Driver:** shadow (with the operator)
**Builds on:** `docs/2026-05-29-porter-design.md` (the core FS/manifest/check-in design — unchanged; this spec only abstracts the storage dependency and adds a Drive implementation + the deployment shape).

## Why

The operator has ~5TB on Google Drive bundled with Google AI + YouTube Premium — storage they keep regardless, at zero marginal cost. (OneDrive/365 is being dropped — unused, no Windows machine.) Two needs, one substrate:

1. **Shed storage** — a shared, casket-encrypted filesystem for inter-agent / inter-machine communication and working data, mounted *inside* the pods that need it.
2. **Offsite backup** — cluster data snapshotted encrypted into the same Google account.

Google and Google's AGy must never see plaintext: casket encrypts before bytes leave the cluster; Drive holds opaque ciphertext blobs only.

## Two architectural decisions (operator, locked)

**D1 — Google Drive only, not multi-cloud.** One backend, implemented well, beats two half-supported ones. The backend stays *pluggable* (S3 remains a future implementation) but v1 ships Drive alone. No OneDrive.

**D2 — Porter is the FS in the pods; one backup pod owns the Google connection.** Agents talk to a porter filesystem and never know Google is underneath — no Drive mounts on agent pods, no OAuth tokens scattered across the fleet, no per-pod sync. A single **backup/sync pod** holds the one credential, runs the sync, and handles Drive's rate limits + chunking. Credential sprawl and "how do we control the sync" both collapse to one component.

```
   agent pods ─┐
   agent pods ─┼─▶  porter FS (casket-encrypted, in-cluster)
   agent pods ─┘            │
                            ▼
                   ┌──────────────────┐   one OAuth token
                   │   backup/sync pod │──(custodian-brokered)──▶ Google Drive
                   │  (porter backend) │                          (ciphertext blobs)
                   └──────────────────┘
```

## Component 1 — `ObjectBackend` interface (the refactor)

Extract porter's storage dependency (today implicitly S3 GET/PUT/LIST/If-Match/versioning) into a minimal interface the core FS/manifest/check-in logic consumes. The mount-capabilities seam already exists (`versioned? / encryption-on? / lease-backend / event-refresh?`, design §"Mount capabilities") — formalize it as the backend contract:

- `Get(id) → bytes, version` (hydrate-on-open)
- `PutConditional(id|path, bytes, expectedVersion) → newVersion | Conflict` (the atomic compare-and-swap that powers check-in; S3 = `PUT If-Match: etag`, Drive = update with revision/etag precondition)
- `List(prefix) → [{path, id, version, size, ...}]` (manifest build/refresh)
- `Changes(cursor) → [delta], newCursor` (optional freshness upgrade; S3 = SQS events, Drive = the **Changes API** — a real native change-feed, *better* than S3's poll-and-diff)
- `Versioning() → {supported, enable()}` (porter warns/offers per design §Versioning; Drive has native revisions — always on)
- Capability flags so the core degrades gracefully (design §"degrade gracefully" stays verbatim).

The core's correctness model (local-edit → atomic conditional check-in → MVCC-isolation, design §"check-in"/§"isolation") is preserved — it just calls `PutConditional` instead of S3 directly. S3 becomes one impl of this interface (future), Drive the first.

## Component 2 — Google Drive backend

Maps the interface onto Drive API v3. The non-trivial mappings (the real work):

- **Identity:** Drive addresses by opaque **file ID**, not path/key. Porter's manifest already holds `path → {key, version-id, ...}`; for Drive, `key` = file ID, and porter owns the path↔ID mapping (Drive folders model the tree, or a flat app-folder with the path in the manifest — **decide at plan time**; flat-with-manifest matches porter's S3 model and sidesteps Drive's folder-rename quirks).
- **Conditional write:** Drive supports preconditions via `If-Match` on the file's ETag / `headRevisionId` for the compare-and-swap; a `412`/mismatch surfaces as porter's existing `Conflict` → re-merge loop (design §merge, unchanged).
- **Versioning:** Drive **revisions** are native and automatic — porter's "warn if Versioning off" path is a no-op here (always versioned); wire a revision-retention policy (Drive keeps revisions but can prune — set `keepForever` on porter check-ins or a retention window, mirroring the noncurrent-version-expiration lifecycle porter wants on S3).
- **Change feed:** the Drive **Changes API** with a page token is a first-class delta stream — implement `Changes()` properly here (better freshness than the S3 poll default; porter design §refresh anticipated this as the "opt-in freshness upgrade").
- **Scope:** use a **dedicated Drive folder / app data scope**, not the whole Drive — blast radius is "this app's folder," never the operator's personal files. (`drive.file` scope = only files the app created.)
- **Throughput reality:** Drive is a consumer API with per-user rate limits and is not a block store — chunked resumable uploads for large blobs, exponential backoff on `403 rateLimitExceeded`/`429`, batch metadata calls. Porter's "low-IO, read-mostly, offsite" profile (design §What-it-is) is the right fit; the backend must still handle throttling cleanly.

## Component 3 — credentials (custodian's first real cloud job)

The one OAuth refresh token lives in **custodian**, brokered to the backup pod at use-time — no raw token in the pod env, no token on agent pods (they never touch Drive). This is the concrete first customer of the cloud-credential brokering in NEX-570 / the satchel on-ramp: prove custodian can hold and refresh a Google OAuth token and hand the backup pod a short-lived access token per sync. Token capture is one-time interactive (the maren/`cw cred` pattern); refresh is automatic thereafter (Google OAuth refresh tokens are long-lived).

## Component 4 — deployment shapes

Two access patterns over the **same** Drive backend + casket key:

- **Shed FS (hot):** porter mounted in the pods that need shared scratch/comms — agents read/write files; porter handles hydrate-on-open + atomic check-in; casket per-file. Single-writer lease via the broker (porter design §concurrency, lease-backend = nexus) for the contended files; pure-optimistic `If-Match` fallback otherwise.
- **Backup (cold):** the backup pod snapshots designated cluster data (start: `nexus.db`/sqld export, aspect PVCs, the ledger, cairn repos — **enumerate at plan time**) into a backup prefix on the same Drive, casket-sealed, versioned via Drive revisions. Scheduled (the in-nexus scheduler, NEX-304 — not cron). Restore path = porter check-out of the snapshot.

## Non-goals (v1)

- OneDrive / S3 backends (interface stays open; not implemented now).
- Mounting Drive on agent pods (the whole point: porter abstracts it, backup pod owns the connection).
- High-throughput / database / git storage on Drive (porter's standing non-goal; cairn + sqld keep their real-disk storage).
- Multi-account or cross-operator Drive.

## Open questions (operator / plan time)

- Flat-app-folder-with-manifest vs Drive-native-folder-tree for the path model (lean flat, per S3 parity).
- Backup scope enumeration + retention window (how many revisions / how long).
- Shed vs backup: same Drive folder (different prefixes) or two folders/scopes — leaning same account, separate top-level folders for blast-radius clarity.
- Does the shed FS need the broker lease from day one, or is optimistic-If-Match enough for agent-comms patterns?

## Sequencing

Spec now; **build behind the roundtable week**. Phase order when it starts: (1) ObjectBackend refactor + the existing S3 design re-expressed against it (no new S3 code — just the seam); (2) Drive backend; (3) custodian token brokering (the NEX-570 dependency); (4) backup pod + scheduler wiring; (5) shed-FS mount in a first consumer pod. Porter also still owes its core implementation (it's design-only today) — this spec assumes that lands first or alongside Phase 1.
