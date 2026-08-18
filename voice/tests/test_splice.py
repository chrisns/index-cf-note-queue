"""Tests for splice.py -- VOICE.md section 10, the first mutation path."""
from __future__ import annotations

import os
import stat

import pytest

from index_note_voice import splice

NOTE_TEXT = """---
recordedAt: 2026-08-16T14:32:07+01:00
recordingId: 01J8XQ4T7V
source: index-01
trigger: double-click-hold
tags:
  - index
  - index/double-click-hold
Verified me: false
---

![[01J8XQ4T7V.m4a]]

Remember to order more filament.
"""


def write_note(path, text=NOTE_TEXT):
    path.write_text(text, encoding="utf-8")
    return str(path)


# ---------------------------------------------------------------------------
# read_flags
# ---------------------------------------------------------------------------


def test_read_flags_basic(tmp_path):
    note_path = write_note(tmp_path / "note.md")
    flags = splice.read_flags(note_path)
    assert flags.verified_me is False
    assert flags.recording_id == "01J8XQ4T7V"
    assert flags.already_scored is False
    assert flags.mtime == pytest.approx(os.path.getmtime(note_path))


def test_read_flags_verified_me_true_literal_bool(tmp_path):
    text = NOTE_TEXT.replace("Verified me: false", "Verified me: true")
    note_path = write_note(tmp_path / "note.md", text)
    assert splice.read_flags(note_path).verified_me is True


def test_read_flags_verified_me_truthy_string_is_not_true(tmp_path):
    # "Verified me" must be the literal boolean True, not a truthy string --
    # yaml.safe_load("yes") also becomes bool True, but a quoted "true"
    # string must not pass the `is True` check.
    text = NOTE_TEXT.replace("Verified me: false", 'Verified me: "true"')
    note_path = write_note(tmp_path / "note.md", text)
    assert splice.read_flags(note_path).verified_me is False


def test_read_flags_no_recording_id(tmp_path):
    text = NOTE_TEXT.replace("recordingId: 01J8XQ4T7V\n", "")
    note_path = write_note(tmp_path / "note.md", text)
    assert splice.read_flags(note_path).recording_id is None


def test_read_flags_already_scored(tmp_path):
    text = NOTE_TEXT.replace("Verified me: false", "Verified me: false\nvoice: likely-owner")
    note_path = write_note(tmp_path / "note.md", text)
    assert splice.read_flags(note_path).already_scored is True


def test_read_flags_not_already_scored_by_default(tmp_path):
    note_path = write_note(tmp_path / "note.md")
    assert splice.read_flags(note_path).already_scored is False


# ---------------------------------------------------------------------------
# all_notes
# ---------------------------------------------------------------------------


def test_all_notes_finds_notes_at_any_depth_and_excludes_attachments(tmp_path):
    # Any depth and any folder name, so a layout change (or simply the year
    # rolling over) never hides captures from the scorer. Notes without a
    # recordingId are the callers' business, not this function's.
    (tmp_path / "2026").mkdir()
    (tmp_path / "2027" / "q1").mkdir(parents=True)
    (tmp_path / "attachments").mkdir()
    write_note(tmp_path / "2026" / "a.md")
    write_note(tmp_path / "2027" / "q1" / "b.md")
    write_note(tmp_path / "loose.md")
    (tmp_path / "attachments" / "01J8XQ4T7V.m4a").write_bytes(b"fake audio")
    # Anything under attachments/ stays invisible, whatever its extension.
    write_note(tmp_path / "attachments" / "sidecar.md")

    notes = splice.all_notes(str(tmp_path))
    assert sorted(notes) == sorted(
        [
            str(tmp_path / "2026" / "a.md"),
            str(tmp_path / "2027" / "q1" / "b.md"),
            str(tmp_path / "loose.md"),
        ]
    )


def test_all_notes_still_finds_a_note_in_a_future_year(tmp_path):
    # Regression for the year-scoped glob: 2027 and beyond must just work.
    (tmp_path / "2031").mkdir()
    write_note(tmp_path / "2031" / "c.md")
    assert splice.all_notes(str(tmp_path)) == [str(tmp_path / "2031" / "c.md")]


def test_all_notes_empty_vault(tmp_path):
    assert splice.all_notes(str(tmp_path)) == []


# ---------------------------------------------------------------------------
# splice_voice_fields
# ---------------------------------------------------------------------------


def test_splice_inserts_voice_fields_with_score(tmp_path):
    note_path = write_note(tmp_path / "note.md")
    splice.splice_voice_fields(
        note_path, band="likely-owner", score=0.69, top2_mean=0.65, model="ecapa-voxceleb-1"
    )
    text = tmp_path.joinpath("note.md").read_text(encoding="utf-8")
    lines = text.splitlines()
    close_idx = lines[1:].index("---") + 1
    inserted = lines[close_idx - 3 : close_idx]
    assert inserted == [
        "voice: likely-owner",
        "voiceScore: 0.6900",
        "voiceModel: ecapa-voxceleb-1",
    ]


def test_splice_omits_voice_score_when_score_is_none(tmp_path):
    note_path = write_note(tmp_path / "note.md")
    splice.splice_voice_fields(
        note_path, band="unscoreable", score=None, top2_mean=None, model="ecapa-voxceleb-1"
    )
    text = tmp_path.joinpath("note.md").read_text(encoding="utf-8")
    assert "voiceScore" not in text
    lines = text.splitlines()
    close_idx = lines[1:].index("---") + 1
    inserted = lines[close_idx - 2 : close_idx]
    assert inserted == ["voice: unscoreable", "voiceModel: ecapa-voxceleb-1"]


def test_splice_does_not_write_top2_mean_key(tmp_path):
    note_path = write_note(tmp_path / "note.md")
    splice.splice_voice_fields(
        note_path, band="likely-owner", score=0.69, top2_mean=0.65, model="ecapa-voxceleb-1"
    )
    text = tmp_path.joinpath("note.md").read_text(encoding="utf-8")
    assert "top2" not in text.lower()


def test_splice_never_touches_verified_me(tmp_path):
    note_path = write_note(tmp_path / "note.md")
    splice.splice_voice_fields(
        note_path, band="likely-owner", score=0.69, top2_mean=0.65, model="ecapa-voxceleb-1"
    )
    text = tmp_path.joinpath("note.md").read_text(encoding="utf-8")
    assert "Verified me: false" in text


def test_splice_preserves_body_after_frontmatter(tmp_path):
    note_path = write_note(tmp_path / "note.md")
    splice.splice_voice_fields(
        note_path, band="likely-owner", score=0.69, top2_mean=0.65, model="ecapa-voxceleb-1"
    )
    text = tmp_path.joinpath("note.md").read_text(encoding="utf-8")
    assert "![[01J8XQ4T7V.m4a]]" in text
    assert "Remember to order more filament." in text


def test_splice_is_noop_when_already_scored(tmp_path):
    text = NOTE_TEXT.replace(
        "Verified me: false", "Verified me: false\nvoice: uncertain\nvoiceModel: old-model"
    )
    note_path = write_note(tmp_path / "note.md", text)
    before_mtime = os.path.getmtime(note_path)
    before_text = tmp_path.joinpath("note.md").read_text(encoding="utf-8")

    splice.splice_voice_fields(
        note_path, band="likely-owner", score=0.9, top2_mean=0.9, model="new-model"
    )

    after_text = tmp_path.joinpath("note.md").read_text(encoding="utf-8")
    assert after_text == before_text
    assert os.path.getmtime(note_path) == before_mtime


def test_splice_force_overrides_already_scored(tmp_path):
    text = NOTE_TEXT.replace(
        "Verified me: false", "Verified me: false\nvoice: uncertain\nvoiceModel: old-model"
    )
    note_path = write_note(tmp_path / "note.md", text)

    splice.splice_voice_fields(
        note_path,
        band="likely-owner",
        score=0.9,
        top2_mean=0.9,
        model="new-model",
        force=True,
    )

    after_text = tmp_path.joinpath("note.md").read_text(encoding="utf-8")
    # The new fields were spliced in (a duplicate voice: key is a known,
    # accepted limitation of force -- see the ponytail comment in splice.py).
    assert "voice: likely-owner" in after_text
    assert "voiceModel: new-model" in after_text


def test_splice_raises_when_no_frontmatter_block(tmp_path):
    note_path = write_note(tmp_path / "note.md", "no frontmatter here\njust text\n")
    with pytest.raises(ValueError):
        splice.splice_voice_fields(
            note_path, band="likely-owner", score=0.5, top2_mean=0.5, model="m"
        )


def test_splice_raises_when_frontmatter_never_closes(tmp_path):
    note_path = write_note(tmp_path / "note.md", "---\nrecordingId: abc\nno closing marker\n")
    with pytest.raises(ValueError):
        splice.splice_voice_fields(
            note_path, band="likely-owner", score=0.5, top2_mean=0.5, model="m"
        )


def test_splice_writes_file_mode_0644(tmp_path):
    note_path = write_note(tmp_path / "note.md")
    os.chmod(note_path, 0o600)
    splice.splice_voice_fields(
        note_path, band="likely-owner", score=0.5, top2_mean=0.5, model="m"
    )
    mode = stat.S_IMODE(os.stat(note_path).st_mode)
    assert mode == 0o644


def test_splice_score_formatted_to_four_decimal_places(tmp_path):
    note_path = write_note(tmp_path / "note.md")
    splice.splice_voice_fields(
        note_path, band="uncertain", score=0.3, top2_mean=0.3, model="m"
    )
    text = tmp_path.joinpath("note.md").read_text(encoding="utf-8")
    assert "voiceScore: 0.3000" in text
