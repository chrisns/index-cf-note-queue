"""Owner-voice gallery: banding, scoring, and gallery membership (VOICE.md
sections 2, 6, 7). Depends on state.py and splice.py -- both already
complete, dependency-free modules.
"""
from __future__ import annotations

import logging
import math

from index_note_voice import splice, state

logger = logging.getLogger(__name__)

# VOICE.md section 2's band table, best-similarity cutoffs.
BAND_LIKELY_OWNER = 0.50
BAND_UNCERTAIN_FLOOR = 0.30


def band_for(best: float) -> str:
    # Only called once a real embedding was scored against a non-empty
    # gallery -- "unscoreable" and "no-gallery" are decided by the caller
    # (server.py) before this is ever reached.
    if best >= BAND_LIKELY_OWNER:
        return "likely-owner"
    if best >= BAND_UNCERTAIN_FLOOR:
        return "uncertain"
    return "unlikely-owner"


def cosine(a: list[float], b: list[float]) -> float:
    dot = sum(x * y for x, y in zip(a, b))
    norm_a = math.sqrt(sum(x * x for x in a))
    norm_b = math.sqrt(sum(y * y for y in b))
    return dot / (norm_a * norm_b)


def score_against_gallery(
    embedding: list[float], gallery: list[list[float]]
) -> tuple[float, float]:
    # Caller's responsibility to ensure gallery is non-empty.
    sims = sorted((cosine(embedding, entry) for entry in gallery), reverse=True)
    best = sims[0]
    # Top-2 mean (VOICE.md section 2): a single-entry gallery just repeats
    # its one similarity, per the contract.
    top2_mean = sum(sims[:2]) / len(sims[:2])
    return best, top2_mean


def find_ticked_recording_ids(vault_dir: str) -> list[str]:
    ids = []
    for note_path in splice.all_notes(vault_dir):
        # VOICE.md/SPEC.md section 2: one bad note must never abort the
        # whole sweep -- same per-item isolation as server.py's two loops.
        try:
            flags = splice.read_flags(note_path)
        except OSError:
            logger.exception("gallery: failed to read flags for %s", note_path)
            continue
        if flags.verified_me and flags.recording_id is not None:
            ids.append(flags.recording_id)
    return ids


def build_gallery_from_state(vault_dir: str, state_dir: str) -> list[list[float]]:
    embeddings = []
    for recording_id in find_ticked_recording_ids(vault_dir):
        record = state.read_record(state_dir, recording_id)
        # VOICE.md section 6: "A note that is unscoreable is never a gallery
        # entry, even if ticked." A record missing entirely (not embedded
        # yet in this pass) is likewise excluded -- defensive default, not
        # the primary mechanism (server.py's orchestration order embeds
        # everything before this is called).
        if record is not None and not record.unscoreable:
            embeddings.append(record.embedding)
    return embeddings
