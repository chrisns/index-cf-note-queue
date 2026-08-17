"""Tests for gallery.py -- VOICE.md sections 2, 6, 7."""
from __future__ import annotations

import math

import pytest

from index_note_voice import gallery, state

NOTE_TEXT = """---
recordedAt: 2026-08-16T14:32:07+01:00
recordingId: {rid}
source: index-01
trigger: double-click-hold
tags:
  - index
  - index/double-click-hold
Verified me: {verified}
---

![[{rid}.m4a]]

Body text.
"""


def write_note(vault_dir, name, rid, verified=True):
    year_dir = vault_dir / "2026"
    year_dir.mkdir(exist_ok=True)
    path = year_dir / name
    path.write_text(
        NOTE_TEXT.format(rid=rid, verified=str(verified).lower()), encoding="utf-8"
    )
    return str(path)


# ---------------------------------------------------------------------------
# band_for
# ---------------------------------------------------------------------------


def test_band_for_likely_owner_at_threshold():
    assert gallery.band_for(0.50) == "likely-owner"


def test_band_for_likely_owner_above_threshold():
    assert gallery.band_for(0.9) == "likely-owner"


def test_band_for_uncertain_at_floor():
    assert gallery.band_for(0.30) == "uncertain"


def test_band_for_uncertain_just_below_likely():
    assert gallery.band_for(0.49) == "uncertain"


def test_band_for_unlikely_owner_below_floor():
    assert gallery.band_for(0.29) == "unlikely-owner"


def test_band_for_unlikely_owner_at_zero():
    assert gallery.band_for(0.0) == "unlikely-owner"


# ---------------------------------------------------------------------------
# cosine
# ---------------------------------------------------------------------------


def test_cosine_identical_vectors_is_one():
    assert gallery.cosine([1.0, 0.0, 0.0], [1.0, 0.0, 0.0]) == pytest.approx(1.0)


def test_cosine_orthogonal_vectors_is_zero():
    assert gallery.cosine([1.0, 0.0], [0.0, 1.0]) == pytest.approx(0.0)


def test_cosine_opposite_vectors_is_minus_one():
    assert gallery.cosine([1.0, 0.0], [-1.0, 0.0]) == pytest.approx(-1.0)


def test_cosine_scale_invariant():
    a = [1.0, 2.0, 3.0]
    b = [2.0, 4.0, 6.0]
    assert gallery.cosine(a, b) == pytest.approx(1.0)


def test_cosine_matches_manual_computation():
    a = [1.0, 0.0]
    b = [1.0, 1.0]
    expected = 1.0 / math.sqrt(2.0)
    assert gallery.cosine(a, b) == pytest.approx(expected)


# ---------------------------------------------------------------------------
# score_against_gallery
# ---------------------------------------------------------------------------


def test_score_against_gallery_single_entry_top2_equals_best():
    embedding = [1.0, 0.0]
    gallery_embeddings = [[1.0, 0.0]]
    best, top2 = gallery.score_against_gallery(embedding, gallery_embeddings)
    assert best == pytest.approx(1.0)
    assert top2 == pytest.approx(1.0)


def test_score_against_gallery_best_is_highest_similarity():
    embedding = [1.0, 0.0]
    gallery_embeddings = [[0.0, 1.0], [1.0, 0.0], [-1.0, 0.0]]
    best, _top2 = gallery.score_against_gallery(embedding, gallery_embeddings)
    assert best == pytest.approx(1.0)


def test_score_against_gallery_top2_mean_averages_two_highest():
    embedding = [1.0, 0.0]
    gallery_embeddings = [
        [1.0, 0.0],  # sim 1.0
        [1.0, 1.0],  # sim ~0.707
        [0.0, 1.0],  # sim 0.0
    ]
    best, top2 = gallery.score_against_gallery(embedding, gallery_embeddings)
    assert best == pytest.approx(1.0)
    assert top2 == pytest.approx((1.0 + (1.0 / math.sqrt(2.0))) / 2.0)


# ---------------------------------------------------------------------------
# find_ticked_recording_ids
# ---------------------------------------------------------------------------


def test_find_ticked_recording_ids_includes_only_verified_me_true(tmp_path):
    write_note(tmp_path, "a.md", "rid-a", verified=True)
    write_note(tmp_path, "b.md", "rid-b", verified=False)

    ids = gallery.find_ticked_recording_ids(str(tmp_path))

    assert ids == ["rid-a"]


def test_find_ticked_recording_ids_skips_notes_without_recording_id(tmp_path):
    note_path = write_note(tmp_path, "a.md", "rid-a", verified=True)
    # Strip the recordingId line to simulate a ticked note with no audio.
    with open(note_path, encoding="utf-8") as f:
        text = f.read()
    text = "\n".join(
        line for line in text.splitlines() if not line.startswith("recordingId:")
    ) + "\n"
    with open(note_path, "w", encoding="utf-8") as f:
        f.write(text)

    ids = gallery.find_ticked_recording_ids(str(tmp_path))

    assert ids == []


def test_find_ticked_recording_ids_empty_vault(tmp_path):
    assert gallery.find_ticked_recording_ids(str(tmp_path)) == []


# ---------------------------------------------------------------------------
# build_gallery_from_state
# ---------------------------------------------------------------------------


def test_build_gallery_from_state_includes_scoreable_ticked_records(tmp_path):
    vault_dir = tmp_path / "vault"
    state_dir = tmp_path / "state"
    vault_dir.mkdir()
    state_dir.mkdir()

    write_note(vault_dir, "a.md", "rid-a", verified=True)
    write_note(vault_dir, "b.md", "rid-b", verified=True)

    state.write_record(
        str(state_dir),
        state.Record(
            recording_id="rid-a",
            unscoreable=False,
            embedding=[1.0, 0.0],
            speech_seconds=3.0,
            model="m",
        ),
    )
    state.write_record(
        str(state_dir),
        state.Record(
            recording_id="rid-b",
            unscoreable=False,
            embedding=[0.0, 1.0],
            speech_seconds=3.0,
            model="m",
        ),
    )

    embeddings = gallery.build_gallery_from_state(str(vault_dir), str(state_dir))

    assert sorted(embeddings) == sorted([[1.0, 0.0], [0.0, 1.0]])


def test_build_gallery_from_state_excludes_unscoreable_even_if_ticked(tmp_path):
    # VOICE.md section 6: "A note that is unscoreable is never a gallery
    # entry, even if ticked."
    vault_dir = tmp_path / "vault"
    state_dir = tmp_path / "state"
    vault_dir.mkdir()
    state_dir.mkdir()

    write_note(vault_dir, "a.md", "rid-a", verified=True)

    state.write_record(
        str(state_dir),
        state.Record(
            recording_id="rid-a",
            unscoreable=True,
            embedding=None,
            speech_seconds=0.2,
            model="m",
        ),
    )

    embeddings = gallery.build_gallery_from_state(str(vault_dir), str(state_dir))

    assert embeddings == []


def test_build_gallery_from_state_excludes_ticked_notes_with_no_record_yet(tmp_path):
    vault_dir = tmp_path / "vault"
    state_dir = tmp_path / "state"
    vault_dir.mkdir()
    state_dir.mkdir()

    write_note(vault_dir, "a.md", "rid-a", verified=True)
    # No state.write_record call for rid-a -- not embedded yet in this pass.

    embeddings = gallery.build_gallery_from_state(str(vault_dir), str(state_dir))

    assert embeddings == []


def test_build_gallery_from_state_excludes_unticked_notes(tmp_path):
    vault_dir = tmp_path / "vault"
    state_dir = tmp_path / "state"
    vault_dir.mkdir()
    state_dir.mkdir()

    write_note(vault_dir, "a.md", "rid-a", verified=False)
    state.write_record(
        str(state_dir),
        state.Record(
            recording_id="rid-a",
            unscoreable=False,
            embedding=[1.0, 0.0],
            speech_seconds=3.0,
            model="m",
        ),
    )

    embeddings = gallery.build_gallery_from_state(str(vault_dir), str(state_dir))

    assert embeddings == []


def test_build_gallery_from_state_empty_vault(tmp_path):
    vault_dir = tmp_path / "vault"
    state_dir = tmp_path / "state"
    vault_dir.mkdir()
    state_dir.mkdir()

    assert gallery.build_gallery_from_state(str(vault_dir), str(state_dir)) == []
