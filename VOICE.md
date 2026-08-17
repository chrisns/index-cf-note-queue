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

## 5. The scoring pass

1. Find work: every `attachments/*.m4a` with no sidecar in `.speaker/`.
2. Decode to 16 kHz mono PCM with `ffmpeg`.
3. Reject as `unscoreable` if speech-band duration is under **1.0 s**. Measure energy in 300–3400 Hz against energy below 200 Hz; the owner's clips carry 60–75% of their total energy below 200 Hz as handling rumble, so a naive energy VAD measures the rumble rather than the voice.
4. Embed with **SpeechBrain ECAPA** (`speechbrain/spkrec-ecapa-voxceleb`, 192-dim). Read the WAV with the standard library `wave` module; `torchaudio.load` now requires `torchcodec` and will fail without it.
5. Score against every gallery entry. Record **best** and **top-2 mean**.
6. Write the sidecar, then splice the band into the note.
7. Consider the note for gallery membership (section 6).

The pass is idempotent. A note with a sidecar is skipped. Deleting a sidecar re-scores that note.

## 6. The gallery

Seeded from a curated list of `recordingId` values the owner confirms are theirs, held in a `ConfigMap`. The seven notes measured in section 2 are the initial seed.

A newly scored note **joins** the gallery when its best score is **≥ 0.55**, comfortably above the measured impostor ceiling of 0.209.

Bounds, so the gallery cannot drift or be poisoned:

- Seeds are never removed.
- Auto-added entries are capped at **50**, most recent kept.
- A note is never added on a `uncertain` or `unlikely-owner` score.
- The gallery is embeddings only. It never needs the audio again unless the model changes.

If the gallery is ever suspect, delete the auto-added entries and every sidecar. The next pass rebuilds from the seeds.

## 7. What is published

Into the note's frontmatter:

```yaml
voice: likely-owner
voiceScore: 0.69
voiceModel: ecapa-voxceleb-1
```

| Band | Condition | Measured headroom |
|---|---|---|
| `likely-owner` | best ≥ **0.50** | owner floor is 0.6247, so 0.125 |
| `uncertain` | 0.30 to 0.50 | — |
| `unlikely-owner` | best < **0.30** | impostor ceiling is 0.2085, so 0.09 |
| `unscoreable` | under 1.0 s of speech, or decode failure | — |

Rules:

- `voiceScore` is a **cosine similarity, not a probability**. Never present it as a percentage or a confidence.
- `voiceModel` is recorded so a band written today can be understood after the model changes.
- **Never write the band into `tags`, and never into the filename.** A wrong tag drives Obsidian search and is effectively permanent.
- On any failure, write **nothing**. Absent metadata is a true statement. A default value is a lie that survives forever.

## 8. How the scorer is triggered

The ingest service pokes the scorer **after the note is durable and after the 200 has been returned**, using the same discipline as any other side effect in `SPEC.md`: it cannot delay the response, and a failed poke is swallowed.

Because a lost poke would otherwise mean a note is never scored, the scorer also **sweeps for unscored notes when it starts**. Unscored notes are trivially findable by the absence of a sidecar, so this costs almost nothing.

A `CronJob` on a timer would be simpler and would touch the ingest service not at all. The trigger was chosen deliberately for immediacy; the sweep is what makes it safe.

## 9. Storage layout

```
<VAULT_DIR>/.speaker/<recordingId>.json     the embedding, both scores, model version, speech seconds
<VAULT_DIR>/2026/<note>.md                  gains three frontmatter keys
```

`.speaker/` is a dot-directory, which Obsidian does not index — invisible in the file explorer, search and graph view.

## 10. Writing into an existing note

This is the **first mutation path** in a system whose correctness story is built entirely from `O_EXCL` creates, and whose spec explicitly forbids stat-then-rename. It is a deliberate exception and must be commented as one.

- Splice **textually**: find the closing `---`, insert the three lines, write a temp file in the **same directory**, `Chmod 0644`, `Sync`, rename, flush the parent directory.
- Do **not** round-trip the YAML. A parser reorders keys, restyles quoting and drops comments, giving every note in the vault a gratuitous diff.
- Skip any note already carrying `voice:`.
- Skip any note modified in the last hour, so the scorer never races the owner editing in Obsidian.
- Never touch a note the scorer did not derive a score for.

## 11. What is still unproven

Stated plainly, because the margin looks comfortable and that invites overconfidence.

- **n = 2 impostors.** A 0.42 margin on two voices is strong evidence, not proof. Neither appears to be a close vocal match for the owner. A sibling, a parent or a similar-sounding colleague is untested. Add impostor recordings opportunistically and re-check the ceiling; do not block on it.
- **Noisy-register notes are not in the measured set.** The owner reports using the ring in noisy places, but no such note was among the nine. Those will land in `uncertain` until enough accumulate to form a neighbourhood.
- **Clips are short.** Two to five seconds, with under two seconds of speech. The measured margin already reflects that, but it leaves little room for further degradation.
- The thresholds in section 7 are derived from **nine recordings**. They are a starting point, not a calibration. Revisit once there are more.

## 12. Out of scope

- Gating, rejecting or moving a note based on the score.
- Identifying **who** an unknown speaker is.
- Publishing a probability or a percentage.
- Any change to the ingest path beyond the trigger in section 8.
- Real-time scoring.
