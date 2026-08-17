# index-note: voice scoring

Attach advisory metadata to each note indicating whether the speaker is the owner.

This file is the specification for the voice scorer. [SPEC.md](SPEC.md) specifies the ingest path and is unchanged by this, except for one fire-and-forget trigger described in section 8.

---

## 1. What this is, and what it is not

It answers one question: **is the voice in this clip the owner, or something the ring captured?** The radio, a colleague, a second person in the room.

It is **not** a security control. The bearer and the ring identifier in `SPEC.md` section 6.2 and 6.2.1 already prove a request came from the owner's ring. This adds nothing there, and a note is **never gated, rejected, quarantined or moved** on the strength of a voice score.

It writes metadata. Downstream consumers may act on it. Nothing in the ingest path ever reads it.

## 2. Why this is believed to work

Measured on real audio from the owner's own ring, 2026-08-17. Every number below is a raw cosine between ECAPA embeddings.

**Owner notes, each scored by its best match against the other owner notes** (leave-one-out over 7 notes, 5 normal and 2 whispered):

| Note | Best match | Top-2 mean |
|---|---|---|
| 953 | 0.6247 | 0.5935 |
| 959 | 0.6555 | 0.6401 |
| 974 | 0.6909 | 0.5930 |
| 995 | 0.6909 | 0.6343 |
| 1011 (whisper) | 0.6989 | 0.5140 |
| 1023 (whisper) | 0.6989 | 0.5092 |
| 1040 | 0.6555 | 0.6166 |

**Two different real people, recorded through the same ring**, scored against that gallery:

| Speaker | Best-of-gallery | Top-2 mean |
|---|---|---|
| impostor 1051 | **0.0926** | 0.0915 |
| impostor 1066 | **0.2085** | 0.1593 |

| | Best | Top-2 |
|---|---|---|
| owner floor | 0.6247 | 0.5092 |
| impostor ceiling | 0.2085 | 0.2085 |
| **margin** | **+0.4162** | **+0.3007** |

Three supporting facts:

- The two impostors score **0.3159 against each other** — higher than either scores against the owner. Non-owner voices resemble each other more than they resemble the owner, which is the structure the design needs.
- The whispered notes are the **strongest** entries in the gallery, at 0.6989, because they match each other. Whispered audio embeds well.
- The owner-versus-owner mean was reproduced by three independent pipelines: 0.526 (WeSpeaker ResNet34 ONNX with a hand-written front end), 0.533 (SpeechBrain ECAPA), 0.551 (WeSpeaker ResNet221 official). Agreement within 0.025 across three architectures and three front ends.

## 3. Why the gallery, and not a profile

A single averaged profile fails, because the owner uses **at least three registers**: whispering, normal speech, and speaking in noisy places. Whisper-versus-normal scores **0.26**, against 0.53 for normal-versus-normal and 0.70 for whisper-versus-whisper. An averaged profile punishes the owner for having more than one voice, and register cannot be detected automatically — zero-crossing rate, spectral tilt and high-frequency energy all interleave between the owner's whispered and normal notes.

**Best-match against a gallery dissolves that.** A whispered note finds a whispered neighbour, a normal note finds a normal one. No register detection, no per-register profiles, no cap on how many registers the owner has. Noisy-environment notes will form their own neighbourhood as they accumulate.

The cost, named explicitly: best-of-N inflates impostor scores as the gallery grows, because a stranger gets more chances to resemble something. Section 6 bounds the gallery for this reason, and top-2 mean is recorded alongside so the effect can be watched.

## 4. Components

One new workload. The ingest service and `cloudflared` are untouched apart from the trigger.

| Component | Purpose |
|---|---|
| `index-note-voice` | Decodes audio, embeds it, scores it against the gallery, writes the band into the note. |

It runs in the same namespace, mounts the same `ReadWriteMany` volume, and holds no credential.

**Model weights live on a separate read-only volume**, not baked into the image. That gives three things: the model can be replaced without rebuilding, every replica shares one copy, and nothing is fetched at runtime — set `HF_HUB_OFFLINE=1` so SpeechBrain cannot silently reach out to Hugging Face when the volume is wrong.

An honest note on size: the weights are the *small* part. ECAPA is roughly 80 MB against `torch` at roughly 800 MB, so moving the model out shrinks the image by under a tenth. The real gains are swappability and the offline guarantee. If image size is the actual goal, the lever is replacing `torch` with `onnxruntime`, which is a different decision recorded in section 5.

Because the model is no longer pinned by the image, `voiceModel` in the frontmatter stops being a nicety and becomes the only record of what produced a given score. Bump it whenever the volume contents change.

## 5. The scoring pass

1. Build the gallery: embed every note whose frontmatter has `Verified me: true` (section 6), reusing cached embeddings from `STATE_DIR` where present.
2. Find work: every `attachments/*.m4a` with no cached record in `STATE_DIR`.
3. Decode to 16 kHz mono PCM with `ffmpeg`.
4. Reject as `unscoreable` if speech-band duration is under **1.0 s**. Measure energy in 300–3400 Hz against energy below 200 Hz; the owner's clips carry 60–75% of their total energy below 200 Hz as handling rumble, so a naive energy VAD measures the rumble rather than the voice.
5. Embed with **SpeechBrain ECAPA** (`speechbrain/spkrec-ecapa-voxceleb`, 192-dim). Read the WAV with the standard library `wave` module; `torchaudio.load` now requires `torchcodec` and will fail without it.
6. Score against every gallery entry. Record **best** and **top-2 mean**.
7. Write the record to `STATE_DIR`, then splice the band into the note.


The pass is idempotent. A note with a cached record is skipped. Deleting its record re-scores that note; deleting all of them plus every `voice:` key rebuilds from scratch.

Gallery membership is recomputed from the tick boxes on every pass, so it is never stale.

## 6. The gallery

**The gallery is exactly the set of notes the owner has ticked.** Nothing joins it automatically.

Every note carries a frontmatter property:

```yaml
Verified me: false
```

Obsidian's Properties panel renders a `checkbox`-type property as a clickable tick box. The owner ticks the notes that are genuinely them. The scorer treats every note with `Verified me: true` as a gallery entry, and everything else as not.

A body checkbox (`- [ ]`) was considered and rejected: Obsidian treats it as a **task**, so every note would pollute task queries and the Tasks plugin.

Properties of this design:

- **No poisoning.** A wrong note cannot enter the gallery without a deliberate human tick.
- **Reversible.** Untick a note and it leaves the gallery on the next pass. Membership is recomputed every time, never accumulated.
- **Seeded once, at cutover.** The seven notes measured in section 2 are known to be the owner's and are ticked as part of deployment. After that, the owner ticks whatever they like.
- **Adapts.** New registers and a changing voice are absorbed by ticking a few new notes.

Rules:

- A note that is `unscoreable` is never a gallery entry, even if ticked. There is too little speech to embed.
- **The ingest service writes `Verified me: false`** when it creates the note, so the tick box exists from the moment a note appears. The scorer only ever **reads** it and must never write it. See `SPEC.md` section 7.6.
- **The empty gallery case.** With nothing ticked, no note can be scored. Write `voice: no-gallery` and no `voiceScore`. Do not guess, and do not treat an empty gallery as evidence of anything.
- Recomputing membership on every pass means reading the frontmatter of ticked notes only. The embeddings themselves live outside the vault (section 9), so this is cheap.

## 7. What is published

Into the note's frontmatter:

```yaml
voice: likely-owner
voiceScore: 0.69
voiceModel: ecapa-voxceleb-1
Verified me: false
```

`Verified me` is the gallery tick box of section 6. It is written by the **ingest service**, not the scorer, and the scorer must never overwrite it — a tick is never undone by a rescore.

| Band | Condition | Measured headroom |
|---|---|---|
| `likely-owner` | best ≥ **0.50** | owner floor is 0.6247, so 0.125 |
| `uncertain` | 0.30 to 0.50 | — |
| `unlikely-owner` | best < **0.30** | impostor ceiling is 0.2085, so 0.09 |
| `unscoreable` | under 1.0 s of speech, or decode failure | — |
| `no-gallery` | nothing is ticked yet, so there is nothing to compare against | — |

Rules:

- `voiceScore` is a **cosine similarity, not a probability**. Never present it as a percentage or a confidence.
- `voiceModel` is recorded so a band written today can be understood after the model changes.
- **Never write the band into `tags`, and never into the filename.** A wrong tag drives Obsidian search and is effectively permanent.
- On any failure, write **nothing**. Absent metadata is a true statement. A default value is a lie that survives forever.

## 8. How the scorer is triggered

The scorer is a **long-running service**, not a `CronJob`.

**Fire-and-forget webhook.** After the note is durable and the 200 has been returned, the ingest service POSTs to the scorer. It carries no note data — it is a poke, not a delivery. It cannot delay the response, and a failure is swallowed and never retried.

**Hourly sweep, in the same process.** A timer inside the scorer runs every hour and picks up anything the webhook missed. Unscored notes are found by the absence of a record in `STATE_DIR`, so this is cheap.

The sweep is what makes the webhook safe to lose. Neither path is authoritative: both do exactly the same pass, and the pass is idempotent.

## 9. Storage layout

```
<STATE_DIR>/embeddings/<recordingId>.json   the embedding, both scores, model version, speech seconds
<VAULT_DIR>/2026/<note>.md                  gains four frontmatter keys
```

**`STATE_DIR` is outside the vault.** A voice embedding is biometric data — a mathematical model of the owner's voice, sufficient to attempt a match elsewhere. It must never sync to other devices or to any cloud sync the vault is later attached to. The scorer needs it; Obsidian never does. Use a separate path on the same volume, or a separate volume.

The only thing that enters the vault is the four frontmatter keys.

## 10. Writing into an existing note

This is the **first mutation path** in a system whose correctness story is built entirely from `O_EXCL` creates, and whose spec explicitly forbids stat-then-rename. It is a deliberate exception and must be commented as one.

- Splice **textually**: find the closing `---`, insert the three lines, write a temp file in the **same directory**, `Chmod 0644`, `Sync`, rename, flush the parent directory.
- Do **not** round-trip the YAML. A parser reorders keys, restyles quoting and drops comments, giving every note in the vault a gratuitous diff.
- Skip any note already carrying `voice:`, unless it is being deliberately rescored.
- **Never overwrite `Verified me`.** It is the owner's input, not the scorer's output.
- Skip any note modified in the last hour, so the scorer never races the owner editing in Obsidian.
- Never touch a note the scorer did not derive a score for.

## 10a. Gap between spec and code, closed

`SPEC.md` section 7.6 requires the ingest service to write `Verified me: false` into every note's frontmatter. The Go service now does this.

## 11. What is still unproven

Stated plainly, because the margin looks comfortable and that invites overconfidence.

- **n = 2 impostors.** A 0.42 margin on two voices is strong evidence, not proof. Neither appears to be a close vocal match for the owner. A sibling, a parent or a similar-sounding colleague is untested. Add impostor recordings opportunistically and re-check the ceiling; do not block on it.
- **Noisy-register notes are not in the measured set.** The owner reports using the ring in noisy places, but no such note was among the nine. Those will land in `uncertain` until enough accumulate to form a neighbourhood.
- **Clips are short.** Two to five seconds, with under two seconds of speech. The measured margin already reflects that, but it leaves little room for further degradation.
- The gallery is **seeded from seven notes**, all recorded within about two hours on a single day. It captures the owner's voice on one day, in two registers, not across seasons, colds or noisy rooms. Expect `uncertain` results until the owner ticks a wider spread.
- The thresholds in section 7 are derived from **nine recordings**. They are a starting point, not a calibration. Revisit once there are more.

## 12. Out of scope

- Gating, rejecting or moving a note based on the score.
- Identifying **who** an unknown speaker is.
- Publishing a probability or a percentage.
- Any change to the ingest path beyond the trigger in section 8.
- Real-time scoring.
