# porter

> *One who carries.* A casket-encrypted, searchable **S3-bucket-as-a-filesystem** — mount a bucket, browse/search/read it locally, edit the occasional file, and have changes carried back as atomic versioned writes.

**Status:** design — pre-implementation. See [`docs/2026-05-29-porter-design.md`](docs/2026-05-29-porter-design.md).

A CarriedWorldUniverse product (sibling to `nexus`, `cairn`, `casket`, `keel`). Built for **low-IO, read-mostly, offsite** storage — *not* a high-throughput filesystem, and explicitly **not** for git or databases (it's object-granular: whole-file writes, no in-place partial writes/locking).

## What it is

- Mounts an S3 (or S3-compatible) bucket as a local, navigable, searchable filesystem.
- **Opt-in** per-file encryption via [casket](https://github.com/CarriedWorldUniverse) (per-file AEAD; plain buckets just mount).
- In-RAM **manifest** (metadata + search, zero S3 round-trips); local cache + hydrate-on-open.
- **check-out → edit locally → atomic versioned check-in** (conditional `PUT If-Match`); single-writer lease (via nexus) + non-blocking reads; type-aware 3-way merge on conflict.
- Warns (and offers to enable) if the bucket lacks Versioning — required for merge + clobber-recovery.

## Status / open

Design is complete (`docs/`). The one open decision is the **hydration mechanism** (FUSE on Linux/macOS — lead — vs the Windows Cloud-Files placeholder API vs a sync-daemon). Language/license TBD at implementation.
