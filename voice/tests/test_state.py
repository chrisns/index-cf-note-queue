import json
import logging
import os

from index_note_voice import state


def test_record_path_is_embeddings_subdir_with_json_suffix(tmp_path):
    path = state.record_path(str(tmp_path), "abc123")
    assert path == os.path.join(str(tmp_path), "embeddings", "abc123.json")


def test_read_record_returns_none_when_no_file(tmp_path):
    assert state.read_record(str(tmp_path), "missing") is None


def test_read_record_returns_none_and_logs_on_corrupt_json(tmp_path, caplog):
    embeddings_dir = tmp_path / "embeddings"
    embeddings_dir.mkdir()
    (embeddings_dir / "bad.json").write_text("{not valid json")

    with caplog.at_level(logging.WARNING):
        result = state.read_record(str(tmp_path), "bad")

    assert result is None
    assert any("bad" in rec.message or "bad" in str(rec) for rec in caplog.records)


def test_write_record_creates_embeddings_dir_and_is_readable_back(tmp_path):
    record = state.Record(
        recording_id="rec1",
        unscoreable=False,
        embedding=[0.1, 0.2, 0.3],
        speech_seconds=2.5,
        model="ecapa-voxceleb-1",
    )

    state.write_record(str(tmp_path), record)

    assert (tmp_path / "embeddings").is_dir()
    read_back = state.read_record(str(tmp_path), "rec1")
    assert read_back == record


def test_write_record_roundtrip_preserves_unscoreable_none_embedding(tmp_path):
    record = state.Record(
        recording_id="rec2",
        unscoreable=True,
        embedding=None,
        speech_seconds=0.4,
        model="ecapa-voxceleb-1",
    )

    state.write_record(str(tmp_path), record)
    read_back = state.read_record(str(tmp_path), "rec2")

    assert read_back == record
    assert read_back.embedding is None
    assert read_back.best is None
    assert read_back.top2_mean is None
    assert read_back.band is None


def test_write_record_roundtrip_preserves_scored_fields(tmp_path):
    record = state.Record(
        recording_id="rec3",
        unscoreable=False,
        embedding=[0.5] * 192,
        speech_seconds=3.2,
        model="ecapa-voxceleb-1",
        best=0.69,
        top2_mean=0.64,
        band="likely-owner",
    )

    state.write_record(str(tmp_path), record)
    read_back = state.read_record(str(tmp_path), "rec3")

    assert read_back == record


def test_write_record_overwrites_existing_record(tmp_path):
    first = state.Record(
        recording_id="rec4",
        unscoreable=True,
        embedding=None,
        speech_seconds=0.2,
        model="ecapa-voxceleb-1",
    )
    state.write_record(str(tmp_path), first)

    second = state.Record(
        recording_id="rec4",
        unscoreable=False,
        embedding=[0.9, 0.8],
        speech_seconds=4.0,
        model="ecapa-voxceleb-1",
    )
    state.write_record(str(tmp_path), second)

    read_back = state.read_record(str(tmp_path), "rec4")
    assert read_back == second


def test_write_record_leaves_no_stray_temp_files(tmp_path):
    record = state.Record(
        recording_id="rec5",
        unscoreable=False,
        embedding=[0.1],
        speech_seconds=1.0,
        model="ecapa-voxceleb-1",
    )
    state.write_record(str(tmp_path), record)

    entries = os.listdir(str(tmp_path / "embeddings"))
    assert entries == ["rec5.json"]


def test_write_record_is_valid_json_on_disk(tmp_path):
    record = state.Record(
        recording_id="rec6",
        unscoreable=False,
        embedding=[0.1, 0.2],
        speech_seconds=1.5,
        model="ecapa-voxceleb-1",
    )
    state.write_record(str(tmp_path), record)

    with open(state.record_path(str(tmp_path), "rec6")) as f:
        data = json.load(f)
    assert data["recording_id"] == "rec6"
    assert data["speech_seconds"] == 1.5


def test_list_attachment_ids_returns_m4a_stems(tmp_path):
    attachments = tmp_path / "attachments"
    attachments.mkdir()
    (attachments / "aaa.m4a").write_bytes(b"")
    (attachments / "bbb.m4a").write_bytes(b"")
    (attachments / "notes.txt").write_bytes(b"")

    ids = state.list_attachment_ids(str(tmp_path))

    assert sorted(ids) == ["aaa", "bbb"]


def test_list_attachment_ids_empty_when_no_attachments_dir(tmp_path):
    assert state.list_attachment_ids(str(tmp_path)) == []


def test_list_attachment_ids_empty_when_attachments_dir_has_no_m4a(tmp_path):
    attachments = tmp_path / "attachments"
    attachments.mkdir()
    (attachments / "readme.md").write_bytes(b"")

    assert state.list_attachment_ids(str(tmp_path)) == []


def test_record_dataclass_defaults_are_none():
    record = state.Record(
        recording_id="x",
        unscoreable=False,
        embedding=[0.0],
        speech_seconds=1.0,
        model="m",
    )
    assert record.best is None
    assert record.top2_mean is None
    assert record.band is None
