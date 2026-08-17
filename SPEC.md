# index-note

Receive voice notes from a Pebble Index 01 ring and write them into an Obsidian vault.

This file is the specification. It is complete and self-consistent. Build from this file.

The GitHub issues on this repository record **why** each decision was made. They are history. Several carry superseded text next to its correction. Do not build from them.

---

## 1. The one rule

**The ring app has no retry.** It sends one POST. On any failure it logs and discards the upload. Nothing tells the wearer.

Every decision below follows from that. When a rule looks over-cautious, this is why.

Source: `IndexWebhookApiImpl.uploadIfEnabled` in `coredevices/mobileapp`. It calls `scope.launch`, sends one POST, and on failure calls `logger.e`. The caller returns before the upload finishes, so the app's own processing queue never learns the outcome and cannot re-run it.

**What a failed POST actually loses.** The webhook is a decorator. `IndexWebhookUploadRecordingOperation.run` runs the inner operation first, so the app finishes its own transcription and note creation before the upload fires. A failed POST therefore loses **the vault copy**, not the thought. The note is still in the Pebble app and can be re-exported by hand.

**The exception.** For a double-click-hold recording, the inner operation is chosen by the app's `secondaryMode` preference. When it is `Search`, the app creates **no** note of its own. The webhook is then the only copy, and a failed POST is a deleted thought. Check this setting on the phone. Treat the loss as total anyway: the design does not get to assume the safe mode is selected.

## 2. Accepted losses

These are decided. Do not design around them.

- A note spoken while the internet, the power or the cluster is down is **lost**.
- **Nothing monitors this system.** No canary, no alarm, no heartbeat. A dead tunnel, a full volume, a crashlooping pod and a Cloudflare rule change all look identical: nothing arrives, and nothing is logged.
- You find out a note is missing by noticing it is not in the vault.

A rolling restart is **not** in this list. That loss is self-inflicted, so both workloads run two replicas.

## 3. Components

| Component | Where | Purpose |
|---|---|---|
| `cloudflared` | k3s namespace `index-note` | Publishes `index-note.cns.me`. The cluster has no other inbound path. |
| `index-note` | k3s namespace `index-note` | Receives the POST and writes the note. |

There is no Cloudflare Worker, no R2, no queue, no `wrangler` and no outbound call of any kind.

## 4. Flow

1. The ring app posts `multipart/form-data` to `https://index-note.cns.me/note`.
2. Cloudflare evaluates the zone rules, then routes to the tunnel.
3. `cloudflared` forwards to the service.
4. The service checks the bearer, streams the body, writes the audio, writes the note, and returns 200.

---

## 5. What the ring sends

`POST`, `Content-Type: multipart/form-data; boundary=<uuid>`.

| Part | Presence | Detail |
|---|---|---|
| `audio` | payload mode `Recording only` or `Both` | `audio/mp4`, filename `<recordingId>.m4a`, AAC-LC, mono, 16 kHz |
| `transcription` | payload mode `Transcription only` or `Both` | plain text |
| `recordedAt` | always | unix timestamp in milliseconds |
| `client` | always | the literal string `ring`. **Ignored.** |

**Measured from real notes, 2026-08-17.** Notes are **2 to 4 seconds**, not the 30 seconds assumed while charting. Bitrate is 24 to 26 kbps. File size is **not** a proxy for duration: every file carries a fixed `free` padding atom of about 23.5 KB, so a 33 KB note holds only about 9 KB of audio. Actual speech, after a speech-band voice activity check, is **under 2 seconds**. Anything reasoning about note length must use the decoded duration, never the byte count.

Headers:

- Every header is user-configured in the app and sent verbatim. This is the only authentication the app can perform.
- `X-Audio-Size` is added automatically when audio is present. It carries the audio byte count.
- `X-Index-Trigger` is added automatically. Its value is `single-click-hold` or `double-click-hold`. It is omitted for legacy tasks.

Set the payload mode to **Both**.

The app's webhook trigger preference has three values: `SingleClick`, `DoubleClickHold` and `Both`. Set it to **`Both`**. The webhook decorates the gesture's normal behaviour rather than replacing it, so nothing is given up. Anything less than `Both` means some notes never reach the vault, silently.

**`recordingId` arrives only as the `audio` part's filename.** A transcription-only payload carries no id at all.

Unrecognised parts, fields and headers are ignored. They are never a reason to reject.

## 6. The HTTP service

### 6.1 Routes

| Route | Behaviour |
|---|---|
| `POST /note` | The webhook. |
| `GET /healthz` | 200. For the kubelet only. Not published through the tunnel. |
| anything else | 404 |
| known path, wrong method | 405 |

There is no redirect anywhere. A redirect could leak the bearer to a third party if the ring's client follows redirects, and it would force an exception in the Cloudflare block rule.

Go's `ServeMux` 301-redirects non-canonical paths such as `//note` on its own. A guard in front of the mux 404s any path that is not exactly `/note` or `/healthz`, so the mux never gets the chance. Use the Go 1.22 method-aware patterns behind the guard.

### 6.2 Authentication

`Authorization: Bearer <token>`.

`INDEX_BEARER_TOKENS` holds a **comma-separated list** of valid tokens. A presented token is valid when it matches any entry. This exists for rotation: add the new token, update the app, then remove the old one. Without it, rotation opens a window where the app and the service disagree, and every note in that window is a 401 and a lost vault copy.

Compare `sha256.Sum256` of the presented token against `sha256.Sum256` of each valid one, with `subtle.ConstantTimeCompare`. Compare the digests, not the raw strings. `ConstantTimeCompare` returns 0 immediately on a length mismatch, so comparing raw strings leaks the length. Digests are always 32 bytes.

A bad or missing bearer returns **401**. Nothing is written, and nothing is buffered anywhere.

### 6.2.1 The ring identifier, a second factor

The `audio` part's filename begins with an identifier specific to the ring, shaped `ring_<device uuid>-<counter>-<per recording uuid>`. That device UUID **identifies the hardware, so it is a secret**. It must never appear in git, in a manifest, in a test fixture or in a commit message.

`INDEX_RING_PREFIXES` holds a comma-separated list of allowed prefixes, supplied the same way as the bearer. A request whose audio filename does not start with one of them returns **401** and writes nothing. A leaked bearer is then not sufficient to write into the vault.

Two deliberate limits:

- A note with **no audio part** carries no identifier and cannot be checked, so it passes on the bearer alone. Rejecting it would delete a real thought for something the sender cannot control.
- The prefix check does **not** replace sanitisation (section 7.2). A valid prefix says nothing about the rest of the filename. Note that `multipart.Part.FileName` already applies `filepath.Base`, so a traversal arrives stripped and then fails the prefix check.

`INDEX_RING_PREFIXES` is required at startup (section 6.7). Unset is fatal, so the check can never be silently inactive.

### 6.3 Reading the body

- Wrap the body in `http.MaxBytesReader` at **16 MiB**.
- Parse the boundary with `mime.ParseMediaType` and stream parts through `multipart.NewReader` over a tail-tracking reader. The tail (the last `len(boundary)+512` bytes) is what tells a real `--boundary--` close delimiter apart from a body that died early: `mime/multipart` reports both as `io.EOF`. The audio part streams to a temp file; the small parts read into memory.
- **Never** call `ParseMultipartForm`. It spills parts over `maxMemory` into `/tmp`, and the root filesystem is read-only.
- **Never** use `part.FileName()` as a path component. Use it only as the source of `recordingId`, and only after sanitising it. See section 7.2.

### 6.4 Truncated and oversized bodies

Hitting the cap, or a body that stops early, is **not** a reason to reject. Write whatever parts completed, set `truncated: true` in the frontmatter, and return **200**.

Return **413** only when the body exceeds the cap before any part has completed, so there is nothing to write.

An authenticated request whose `Content-Type` is missing, unparseable, or carries no boundary is treated the same way: nothing is recoverable, so write the empty-request note with `truncated: true` and return **200**. The ring would never resend after a 4xx.

### 6.5 Partial payloads

Write whatever arrives. Never reject.

| Arrived | Written |
|---|---|
| audio and transcription | note with embed and text |
| transcription only | note with text, no embed |
| audio only | note with embed, empty body |
| neither | a note recording that an empty request arrived |

### 6.6 Server settings

| Setting | Value | Why |
|---|---|---|
| `ReadHeaderTimeout` | 5s | slow-loris |
| `ReadTimeout` | 120s | matches the ring client's 2 minute timeout |
| `WriteTimeout` | 150s | read budget plus the write budget |
| `IdleTimeout` | 60s | |
| `MaxHeaderBytes` | 65536 | the ring sends a handful of small headers |
| listen | `:8080`, container port named `http` | |

`srv.Shutdown(ctx)` on `SIGTERM`.

### 6.7 Startup contract

Log once and exit non-zero when any of these fails. Fail at deploy, not at 3am.

1. `INDEX_BEARER_TOKENS` is set, is non-empty after splitting on commas, and every entry is at least 32 bytes. An empty entry makes an expected digest `sha256("")`, and `Authorization: Bearer ` would authenticate.
1b. `INDEX_RING_PREFIXES` is set and every entry is at least 8 bytes. Unset is fatal, so the second factor in section 6.2.1 can never be silently inactive.
2. `VAULT_DIR` exists, and a probe file can be created and removed inside it.
3. `VAULT_DIR` accepts a probe write, which is then removed. The service also attempts to tighten the directory to `0755`, but that is **best effort and must never be fatal**. Verified on the real cluster: `nfs-subdir-external-provisioner` creates the directory `root:root 0777`, so an unprivileged pod with every capability dropped can write to it but cannot `chmod` or `chown` it. The probe write is the only real gate.
4. `time.LoadLocation("Europe/London")` succeeds. The binary must `import _ "time/tzdata"`, because a `scratch` image has no zoneinfo and the fallback is a silent switch to UTC.

The startup probe replaces a one-off manual write check. A `scratch` image has no shell, so nobody can `exec` into the pod to run one.

### 6.8 Logging

Log the method, path, status, byte count and duration. **Do not log `/healthz`.** The kubelet probes both replicas every few seconds, and those lines say nothing while burying the requests that matter.

- **Never** log the bearer.
- **Never** log the transcription text. It is the private content this system exists to move.
- Log the path with `%q` and truncate it to 256 bytes. It is attacker-controlled. Apply the same treatment to any error string built from a part name, a filename or a header value.
- Log the user agent and content type. They are what you will need if the first real note does not arrive.

---

## 7. The note on disk

### 7.1 Paths

```
<VAULT_DIR>/2026/2026-08-16 1432 remember-to-order-more-filament.md
<VAULT_DIR>/attachments/<recordingId>.m4a
```

`MkdirAll` the year directory and `attachments/` at startup and before each write, with mode `0755`. A freshly provisioned volume is empty, and the first note would otherwise 500 and be lost.

Files are `0644`, directories `0755`. Apply the mode with an explicit `Chmod`. `os.CreateTemp` creates at `0600` and ignores the mode you want.

### 7.2 `recordingId`

It arrives only as the `audio` part's filename. It must be used, and never raw. The destination is a shared volume, and `../../` reaches real files.

1. NFKD-normalise, then keep only `[A-Za-z0-9_-]`.
2. Truncate to 192 bytes. Real ring ids are about 82 bytes, so a tighter cap silently truncated every one of them and risked two recordings sharing an id. 192 keeps `<id>.m4a` well inside the 255-byte filename limit.
3. Require at least one `[A-Za-z0-9]` and no leading hyphen.

If the result is unusable, **do not reject the note**. Synthesise an id from the timestamp and a short random suffix, and set `recordingIdSynthesised: true` in the frontmatter. The synthesised id exists to name the audio file.

When there is no audio part at all, there is no id: omit `recordingId` and the flag (section 7.6), and the collision rules treat the note as id-less (section 7.4).

A sanitised id also gives idempotency. The ring's duplicate guard is an in-memory set, so a duplicate after an app restart is real. The same id lands on the same filename.

### 7.3 The slug

From the transcription: NFKD-normalise, lowercase, keep `[a-z0-9]`, collapse every other run to a single hyphen, trim leading and trailing hyphens, take the first six words, cap at 60 bytes.

An empty transcription means no slug. The filename is then the timestamp alone.

The slug comes from speech and ends up in a filename. The rules above are what make "slash dot dot slash" harmless.

### 7.4 Collisions

Minute resolution means two different notes can collide. Overwriting one with the other is a silent loss, which is the failure this whole system exists to avoid.

Create the final `.md` with `O_CREAT|O_EXCL`, or `link(2)` the temp file into place. **Do not stat and then rename.** `rename(2)` replaces the destination silently, with no error and nothing logged, and two replicas can both see no existing file at the same instant.

On `EEXIST`:

1. Read `recordingId` from the existing file's frontmatter.
2. Both ids present and equal: keep the existing file. It is the same note arriving twice.
3. Otherwise: append a hyphen and the first eight characters of the new `recordingId`, and retry. When there is no id, use the seconds from `recordedAt` instead.

**A missing id is never equal to a missing id.** Two audio-less notes in the same minute must not collapse into one.

Apply the same discipline to `attachments/<recordingId>.m4a`. Use `O_EXCL`, and never overwrite an attachment this request did not create. On `EEXIST` the two cases differ:

- A **ring-supplied** id: the filename is the id, so this is the same recording arriving twice. Keep the existing file.
- A **synthesised** id: a different recording drew the same name. Regenerate the id and retry, and the note's embed references the file actually written.

### 7.5 The clock

Use `recordedAt` from the body, in `Europe/London`, for both the filename and the frontmatter.

When it is missing, zero, or more than a day from now in either direction (exactly a day is allowed), use the server clock and set `clockSource: server` in the frontmatter. A wrong timestamp must be visible, not invented.

### 7.6 Frontmatter

Marshal it with a YAML library. Never format strings.

```yaml
---
recordedAt: 2026-08-16T14:32:07+01:00
recordingId: 01J8XQ4T7V
source: index-01
trigger: double-click-hold
tags:
  - index
  - index/double-click-hold
---
```

| Key | Rule |
|---|---|
| `recordedAt` | always, ISO 8601 with offset |
| `recordingId` | omitted when there is no audio part |
| `source` | always `index-01` |
| `trigger` | omitted when `X-Index-Trigger` is absent, or when its value is not one of the two legal ones. An absent value must never become a fake one. |
| `tags` | always `index`. Add `index/<trigger>` only when `trigger` is present. |
| `truncated` | `true` only when the body was cut short |
| `clockSource` | `server` only on the clock fallback |
| `recordingIdSynthesised` | `true` only when the id was generated |
| `Verified me` | always written as `false`. The owner ticks it in Obsidian to confirm the note is their own voice. The service writes it once and never reads it; only the voice scorer reads it. See [VOICE.md](VOICE.md) section 6. |
| `audioSizeMismatch` | `true` when the received audio byte count differs from `X-Audio-Size` |

### 7.7 Body

```markdown
![[<recordingId>.m4a]]

Remember to order more filament.
```

The embed first, then the transcription, with the transcription's leading and trailing whitespace trimmed. The audio is the source of truth. The transcription is a machine's reading of it.

Obsidian resolves `![[name.m4a]]` by name anywhere in the vault, so the embed does not depend on the directory layout.

Omit the embed when there is no audio.

### 7.8 Write order

The order is the correctness property.

1. Create each temp file with `os.CreateTemp` **in the same directory as its final path**. Same-directory rename is the only rename that behaves on NFS.
2. `defer os.Remove(tmp)` immediately. It is a no-op after a successful rename, and it covers every abort path including a panic.
3. Write, `Chmod` to `0644`, then `f.Sync()`.
4. Put the **audio** into place first, then the **markdown**. The note is the last thing to appear, so it never references a file that is not there yet.
5. Flush the parent directory after each rename. On NFS the guarantee is weaker than on local disk. That weakness is accepted.
6. Return 200.

Once the markdown is in place, return 200 whatever happens afterwards. A 500 must mean the vault holds no markdown for this request.

---

## 8. The repository

### 8.1 Tree

```
.
├── Dockerfile              # at the root: the shared build workflow hardcodes `context: .`
├── go.mod
├── main.go                 # and siblings
├── testdata/               # multipart fixtures and golden files
├── SPEC.md
└── .github/
    ├── workflows/
    │   ├── build.yml       # to add: calls the house build workflow
    │   └── security.yml    # templated
    ├── semver.yaml         # templated, seeded at major 1
    ├── renovate.json5      # templated, extends github>chrisns/.github:renovate
    ├── mergify.yml         # templated
    └── FUNDING.yml         # templated
```

`repomanager` already templated `semver.yaml`, `renovate.json5`, `mergify.yml`, `security.yml`, `LICENSE`, `SECURITY.md` and `CODE_OF_CONDUCT.md`. Only `build.yml`, the Go source and the `Dockerfile` are left to write.

`mergify.yml` auto-merges pull requests from the templating bot and nothing else. The earlier decision against Mergify was about human pull requests, so leave it alone. Removing it would be undone on the next templating run.

There is no `worker/` directory, no `wrangler.jsonc` and no Cloudflare API token. If one was created for a Worker deploy, revoke it and delete the Actions secret.

### 8.2 Image

`scratch`, with the static Go binary and nothing else.

- `import _ "time/tzdata"`. There is no zoneinfo in the image.
- **No `ca-certificates`.** The service makes no outbound TLS calls.
- `USER 65532:65532` and `EXPOSE 8080` as metadata. Defence in depth; the manifest sets the same numerically.

### 8.3 Build

Call the house workflow. It exposes a `platforms` input, so narrowing is a caller argument, not a fork.

```yaml
permissions:
  packages: write
  id-token: write

jobs:
  build:
    uses: chrisns/.github/.github/workflows/dockerbuild.yml@<sha> # main
    with:
      platforms: linux/amd64,linux/arm64
```

The `permissions` block is required, not decoration. This repository defaults `GITHUB_TOKEN` to **read-only**, a called workflow cannot exceed its caller, and the shared workflow needs `packages: write` to push and `id-token: write` for cosign. Without it GitHub refuses the run with `startup_failure` and no job logs, because the failure happens before any job starts. Older repositories such as `docker-bb` omit the block only because they default to write.

It pushes to `ghcr.io/chrisns/index-cf-note-queue` on the default branch only, tags with the `semver-generator` output plus `sha-<long>`, `edge` and `latest`, and signs with cosign in a separate job. It reads `.github/semver.yaml`, which is already templated and seeded at major 1.

Deployments pin the semver tag. Renovate raises the bump. A rollback is then a one-line revert.

---

## 9. Kubernetes

Manifests live in `chrisns/infra/index-note/`. They are applied by hand with `kubectl apply -k index-note/`. There is no GitOps controller.

```
index-note/
├── kustomization.yaml
├── namespace.yaml
├── pvc.yaml
├── deployment.yaml          # the service
├── service.yaml
├── cloudflared.yaml         # deployment
└── cloudflared-config.yaml  # the file behind the configMapGenerator
```

**No Ingress, no Certificate and no `external-dns` annotation.** The house app template includes all three. This app must have none of them, because the tunnel is the only way in and publishing the hostname through Traefik would defeat every Cloudflare rule.

### 9.1 Namespace and volume

Namespace `index-note`, declared as a resource and set with the `namespace:` transformer, matching every other app here.

```yaml
# pvc.yaml
accessModes: [ReadWriteMany]
storageClassName: nfs          # default class, nfs-subdir-external-provisioner, reclaimPolicy Retain
resources: { requests: { storage: 10Gi } }
```

The provisioner creates `/volume4/cluster-store/index-note-<claim>-<uuid>` on `troy.cns.me`, exported to all three nodes. It is a **new** directory. Connecting it to the real Obsidian vault is a later, separate job, and nothing in this design depends on how.

### 9.2 The service Deployment

| Setting | Value |
|---|---|
| replicas | 2 |
| strategy | `RollingUpdate`, `maxUnavailable: 0` |
| `terminationGracePeriodSeconds` | 180 |
| container port | 8080, named `http` |
| volume mount | the PVC at `/vault` |
| `runAsUser` / `runAsGroup` | 65532 / 65532 |
| `runAsNonRoot` | true |
| `readOnlyRootFilesystem` | true |
| `allowPrivilegeEscalation` | false |
| capabilities | drop `ALL` |
| `seccompProfile` | `RuntimeDefault` |
| readiness and liveness | `httpGet /healthz` on port `http` |
| resources | requests 50m / 64Mi, limits 200m / 128Mi |

`runAsNonRoot` needs a numeric `runAsUser`, because `scratch` has no `/etc/passwd`. Without the number the kubelet refuses the pod.

Do not set `fsGroup`. The in-tree NFS plugin sets `Managed: false` and silently ignores it. Section 6.7 step 3 is what makes the directory writable.

Environment:

| Variable | Value |
|---|---|
| `INDEX_BEARER_TOKENS` | from the Secret, comma-separated |
| `INDEX_RING_PREFIXES` | from the Secret, comma-separated. Identifies the hardware, so never in git |
| `VAULT_DIR` | `/vault` |
| `TZ` | `Europe/London` |
| `LISTEN_ADDR` | `:8080` |

Service `index-note`, port 80, `targetPort: http`.

### 9.3 `cloudflared`

Two replicas, `RollingUpdate` with `maxUnavailable: 0`, `terminationGracePeriodSeconds: 40`. Readiness is `httpGet /ready` against the metrics port; the committed config file's `metrics: 0.0.0.0:2000` key provides it. `cloudflared` carries the same `securityContext` hardening as the service; it runs fine as 65532 with a read-only root.

Both generators use `disableNameSuffixHash: true`, the house style. A config or secret edit therefore does not trigger a rollout by itself; deployment is manual `kubectl apply -k` plus a `kubectl rollout restart` when only the config changed.

The tunnel is **locally managed**, so its routing lives in git. A remotely managed tunnel keeps the routing in the Cloudflare dashboard, and a rebuild from `kubectl apply -k` would silently lose it.

```yaml
# cloudflared-config.yaml, committed, loaded with configMapGenerator
tunnel: <tunnel-uuid>
credentials-file: /etc/cloudflared/creds/credentials.json
metrics: 0.0.0.0:2000
no-autoupdate: true
ingress:
  - hostname: index-note.cns.me
    path: ^/note$
    service: http://index-note:80
  - service: http_status:404
```

The `path` rule means only `/note` ever reaches the pod. It does not wait on any Cloudflare rule, and it is in git.

### 9.4 Secrets

One `secretGenerator` over a gitignored `.env`, plus the credentials file.

| Item | Source |
|---|---|
| `INDEX_BEARER_TOKENS` | `.env`, each entry 32 bytes from a CSPRNG, comma-separated |
| `INDEX_RING_PREFIXES` | `.env`, read from a real note's `recordingId`. **Secret: identifies the ring.** |
| `credentials.json` | the credentials file `cloudflared tunnel create` writes, renamed and added as a file entry. A fixed name keeps the config independent of the tunnel uuid. |

Add both filenames to `chrisns/infra/.gitignore` in the same commit that adds the app. The repository's own `.gitignore` must carry them, not only the operator's global one.

---

## 10. Cloudflare

Zone `cns.me`, id `c97f4c09a02352dab8d4afadb632b43a`, Free plan.

### 10.1 What can silently eat a note

The Ruleset Engine phases run in this order: `ddos_l7`, WAF custom rules, rate limiting, WAF Managed Rules, Super Bot Fight Mode, Access, then later phases, Workers, cache, and finally the origin, which is the tunnel.

Running beside all of that, with no documented position: **Bot Fight Mode**, **Browser Integrity Check**, Security Level, IP Access Rules, User Agent Blocking, Zone Lockdown.

- **Browser Integrity Check is on by default.** It challenges clients with an absent or non-standard user agent, which describes the ring exactly. It is the likeliest cause of a lost first note. It can be skipped.
- **Bot Fight Mode is opt-in and off by default.** Verified off on `cns.me` on 2026-08-17. It **cannot** be skipped per hostname on the Free plan, because it does not run on the Ruleset Engine. Choosing this design means `cns.me` gives up Bot Fight Mode permanently. Turning it on later silently breaks note delivery.
- **The Cloudflare Free Managed Ruleset is deployed by default.** The skip rule takes it off this path.
- **Cloudflare Access is not used.** It runs after the challenge phases, so it protects nothing from them. Its default service token lifetime of 8760 hours is a scheduled silent outage, and an unauthenticated request can get a redirect that a redirect-following client reports as success.

### 10.2 Settings

1. One DNS record for `index-note`, a CNAME to `<tunnel-uuid>.cfargotunnel.com`, **proxied**. `cloudflared tunnel route dns <tunnel> index-note.cns.me` creates it. A grey cloud means the hostname does not resolve at all.
2. Bot Fight Mode **off**. Confirm it, and never turn it on for this zone again.
3. Under Attack mode off. Security level is not `under_attack`.
4. **Applied 2026-08-17.** A **Configuration Rule**, not a WAF custom rule. Browser Integrity Check is not skippable from the WAF phases, but a configuration rule turns it off for matching requests only, leaving the rest of the zone protected:
   - name: `index-note ring webhook: no Browser Integrity Check`
   - expression: `(http.host eq "index-note.cns.me" and http.request.method eq "POST" and http.request.uri.path eq "/note")`
   - setting: Browser Integrity Check **off**
   - verified with the ring's own user agent, `CoreApp/1.8.0.6-1-g4546d7b2f`: 200
5. **Verified 2026-08-17, nothing to do.** This zone has **0 of 5** custom rules, **0 of 1** rate limiting rules, and no managed rules, which the Free plan gates behind an upgrade. Confirmed empirically as well: a body carrying SQLi, XSS and traversal patterns returned 200, so nothing inspects this path. The `http_request_firewall_managed` and `http_ratelimit` skips the earlier research called for are neither needed nor possible here. Re-check this if a rule is ever added to the zone.
6. No Cloudflare Access application on the hostname.
7. The ring posts to `https://` explicitly. A 301 from Always Use HTTPS turns the POST into a GET.
8. There is no split-horizon DNS on the LAN. Confirmed: `*.p.cns.me` are public records holding private addresses, so Cloudflare is the only path to this hostname.

### 10.3 The block rule, afterwards

Add it only after a real note has arrived, and write it from the request you actually observed.

Match on **method and path only**: `http.host eq "index-note.cns.me" and not (http.request.method eq "POST" and http.request.uri.path eq "/note")` → Block.

Never fingerprint the user agent or a header. A rule written from assumptions blocks every note, permanently and silently, and there is no monitoring to tell you.

The `cloudflared` `path` rule in section 9.3 already achieves most of this, in git, without waiting.

### 10.4 Making it repeatable

There is no `wrangler` in this design, so nothing is set as a side effect of a deploy. The settings above are a checklist. A short script against the Cloudflare API can replace it. The minimum token scope to read and set Bot Fight Mode is Zone, Bot Management, Edit.

```bash
Z=c97f4c09a02352dab8d4afadb632b43a
curl -s "https://api.cloudflare.com/client/v4/zones/$Z/bot_management" -H "Authorization: Bearer $CF_TOKEN"
```

---

## 11. Testing

There is no way to replay a real note, and the first one is irreplaceable. So:

1. Build a multipart fixture by hand from section 5 and commit it under `testdata/`.
2. Golden-file tests over the renderer: the filename, the frontmatter and the body, for each of the four payload combinations in section 6.5, plus a collision, a synthesised id, a truncated body and a clock fallback.
3. Fuzz or table-test the sanitiser and the slug with `../../etc/passwd`, `..`, `.`, a name that is only hyphens, a leading hyphen, and non-ASCII speech.
4. A `curl` smoke test against the deployed service **before** pointing the ring at it.
5. Replace the hand-built fixture with a captured real one as soon as one exists.

## 12. First deployment, in order

1. `cloudflared tunnel create index-note`. Keep the credentials JSON.
2. `cloudflared tunnel route dns index-note index-note.cns.me`.
3. Apply the Cloudflare settings in section 10.2, including the Skip rule.
4. Add `.github/workflows/build.yml`, push, and let the build produce a tagged image.
5. Create `chrisns/infra/index-note/`, add both filenames to `.gitignore`, write `.env` and the credentials file, then `kubectl apply -k index-note/`.
6. Confirm both Deployments are ready and the startup probe passed.
7. `curl` a fixture at `https://index-note.cns.me/note` with the bearer. Confirm 200 and the files in the volume.
8. Configure the ring: the URL, the `Authorization` header, payload mode **Both**, and the webhook trigger set to **Both**. If the app refuses an `Authorization` header, fall back to `X-Widget-Token` and change the service to match. While you are in Index Settings, read the `secondaryMode` value. If it is `Search`, a double-click-hold note has no copy in the app, and a failed POST loses it outright. See section 1.
9. Record one real note. Confirm 200 in the app and the file in the volume.
10. Read Cloudflare Security Events the **same day**. Free plan retention is short. Look for any row on this hostname from Browser Integrity Check, Bot Fight Mode or Managed Rules.
11. Only now add the block rule from section 10.3.

## 12a. Voice scoring

An out-of-band scorer attaches advisory metadata about whether the speaker is the owner. It is specified separately in [VOICE.md](VOICE.md).

It does not affect this document except for one addition: after the note is durable and the 200 has been returned, the service pokes the scorer. That poke cannot delay or endanger the response, and a failure is swallowed. A note is **never** gated, rejected or moved on the strength of a voice score.

## 13. Out of scope

- Any change to the Pebble ring or its phone app.
- Any processing of the transcription beyond writing the file. No summarisation, no tagging by content.
- Reading or managing the rest of the Obsidian vault.
- More than one ring, one wearer or one vault.
- Any buffer that survives an outage.
- Monitoring of any kind.
