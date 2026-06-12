# porter backup-MVP — encrypted offsite backups to Google Drive

**Status:** Approved direction (operator, 2026-06-12: "let's make that chain work")
**Parent designs:** `docs/2026-05-29-porter-design.md` (core), `docs/2026-06-11-google-drive-backend-design.md` (Drive backend, D1/D2 locked)
**Scope:** the BACKUP (cold) slice only. Shed FS, manifest MVCC, check-in/conflict, leases, Changes feed — all deferred; cold backups are new-object writes with no contention.

## The chain

```
snapshot (per-source consistent copy)
  → casket envelope (DEK; recipients = cluster key + operator recovery key)
    → custodian-brokered Google OAuth (refresh token never leaves custodian's vault... pod exchanges for short-lived access tokens)
      → Drive app folder (drive.file scope, ciphertext only, Drive revisions = history)
        → mason-declared backup pod on a schedule
          → PROVEN restore (the acceptance)
```

## Backup set (operator principle: "anything needed to bring back up that we can't get out of repos")

| Source | What | Method | ~Size |
|---|---|---|---|
| k8s Secrets | cwb: almanac-org-seed, custodian-org-seed, herald-secrets, **cwb-ca keypair**, *-tls issuer material; croft: croft-* secrets; nexus: aspect keyfiles, nexus-broker-env, lynxai-env | `kubectl get secret -o yaml` (curated list, not blanket) | <1MB |
| almanac | almanac.db | sqlite3 `.backup` (WAL-safe, no pause) | 57KB |
| custodian | custodian.db | sqlite3 `.backup` | 36KB |
| herald | herald.db | sqlite3 `.backup` | 912KB |
| ledger | ledger.db | sqlite3 `.backup` | 372KB |
| cairn | cairn.db + repos/ | `.backup` + tar repos/ | ~4MB |
| sqld | iku.db/ tree | tar (libSQL tolerates live copy) | 28MB |
| croft home | memory + sessions (~/.claude), ~/shadow, home.git, dotfiles — **excluding ~/src and ~/work clone caches** (re-clonable = not backed up) | tar with excludes | ~1.5GB |

Sources are enumerated in a versioned config (almanac parameter `cwb/porter/backup/sources`, YAML) — adding a source is a config change, not a build. forge's 45GB joins after chunked-upload is proven at this scale.

## Encryption: multi-recipient casket envelope (new casket-go capability)

casket-go today is single-recipient (envelope.go Seal/Open). The satchel design already
calls for the multi-recipient envelope as a *casket* primitive ("DEK wrapped to each
recipient's ECDH public key, age-style — in casket, not in satchel"). porter is the
first consumer; building it here advances satchel for free.

- Per-snapshot random DEK; body = casket AEAD under the DEK (existing primitive).
- DEK wrapped to each recipient X25519 public key (ECDH + HKDF + AEAD, age-style).
- Recipients v1: (1) the **porter cluster key** (k8s Secret, in the backup set itself),
  (2) the **operator recovery key** — X25519 keypair generated at setup; the private
  half is handed to the operator ONCE for off-machine custody (file → printed/USB/
  password manager → deleted from the cluster). Restores must work from bare hardware
  with only the recovery key + Google login.
- AAD binds (RepoIdentity="porter-backup", ObjectPath=snapshot path) per casket convention.

## Credentials: custodian kind "oauth" (custodian's first cloud credential)

- custodian M1 is kind=git only (store.go normalize gate). Add kind `oauth` with bundle
  `{client_id, client_secret, refresh_token, token_uri, scope}` — storage stays opaque
  sealed bytes; no custodian-side token logic (it's a vault, not a token service).
- The backup pod fetches the bundle (mTLS, scope cred:read, audited) and performs the
  refresh→access exchange itself per sync. Refresh token never appears in pod env or
  manifests.
- One-time human step (operator): create the Google OAuth client (Desktop type) in
  Google Cloud console + run the device/loopback consent for scope `drive.file`;
  `cw credential` (or a porter setup command) stores the bundle into custodian.

## Drive layout

- Dedicated app folder `CarriedWorld-Porter/backups/` (drive.file scope: porter sees
  only files it created — blast radius is the folder, never personal files).
- Object naming: `backups/<source>/<UTC timestamp>.casket` + a per-run
  `backups/manifests/<timestamp>.json.casket` (source list, sizes, SHA-256 of plaintext,
  DEK wrap info) — the manifest is what a restore reads first.
- Retention: keep all manifests; prune source snapshots older than 30 days except the
  first of each month (config-driven; enforced by the pod post-sync).
- Uploads: resumable/chunked (8MB chunks), exponential backoff on 403/429.

## The backup pod (`porter-backup`)

- Go binary in the porter repo (`cmd/porter-backup`): one sync pass = read sources
  config from almanac → snapshot each source → seal → upload → write manifest → prune
  per retention → emit a summary log line per source (size, duration, Drive file id).
- Snapshot access: the pod runs on the single node and mounts the host storage dir
  (`/var/lib/rancher/k3s/storage`, read-only) + the k8s API for Secrets (read-only,
  curated by RBAC to the named secrets... or namespace-scoped read on cwb/croft/nexus
  secrets — accepted for the backup role, it's the machine that must hold them anyway).
  sqlite `.backup` runs against the host paths via the modernc sqlite driver (no exec
  into service pods).
- Schedule: in-pod ticker (default every 6h, env) — the in-nexus scheduler (NEX-304)
  replaces it when it lands; k8s CronJob is the documented alternative if the pod
  proves annoying. Deployed as a **mason-declared app** (`cw app declare porter-backup`)
  — needs the app model's first extension: `hostPathMounts` + `serviceAccountName`
  RBAC already exists.  If the declaration model can't express it cleanly in one step,
  deploy v1 as a raw manifest (almanac/custodian precedent) and file the mason
  app-model extension as follow-up — do NOT distort mason's schema in a hurry.
- `porter restore <timestamp> [--source X]` (same binary): fetch manifest + snapshots,
  unwrap DEK with EITHER recipient key, verify hashes, write to an output dir with
  per-source restore notes. Restore NEVER writes into live service paths.

## Acceptance (the M1 gate)

1. End-to-end sync: all sources snapshotted, sealed, uploaded; manifest readable;
   Drive folder shows only ciphertext.
2. **Bare-metal-style restore drill**: on a clean dir (and using ONLY the recovery key
   + Drive access — no cluster keys), `porter restore` recovers almanac.db, the
   secrets bundle, and croft memory; sqlite integrity_check passes; a sealed almanac
   secret decrypts with the restored org seed.
3. Tamper check: a bit-flipped blob fails AEAD open loudly.
4. Custodian audit shows the oauth fetch per sync; refresh token absent from pod
   env/logs/manifests.
5. Pruning: artificial old snapshots pruned per policy, monthly keeper retained.

## Non-goals (unchanged from parent + this slice)

Shed FS / hot mounts; S3/OneDrive; multi-account; forge's 45GB (phase 2 of backups);
incremental/dedup (snapshots are small; revisit when forge joins); backup of ~/src and
~/work clone caches (re-clonable by definition).
