"""Tests for server.py -- the orchestration pass (VOICE.md section 5) and
the stdlib HTTP server that triggers it (VOICE.md section 8).

audio.decode_to_wav/read_wav_samples/speech_band_seconds are faked via the
autouse fake_audio fixture below: this module's job is orchestration, not
audio decoding, and audio.py already has its own real tests. No torch is
ever imported here -- fake_embed_fn is a plain python closure.
"""
from __future__ import annotations

import http.client
import os
import tempfile
import threading
import time

import pytest

from index_note_voice import audio, server, state
from index_note_voice.config import Config

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

ALREADY_SCORED_NOTE_TEXT = """---
recordedAt: 2026-08-16T14:32:07+01:00
recordingId: {rid}
source: index-01
Verified me: false
voice: unlikely-owner
voiceScore: 0.1000
voiceModel: some-old-model
---

![[{rid}.m4a]]
"""


def write_note(vault_dir, name, rid, *, verified=False, mtime=None, already_scored=False):
    year_dir = vault_dir / "2026"
    year_dir.mkdir(exist_ok=True)
    path = year_dir / name
    template = ALREADY_SCORED_NOTE_TEXT if already_scored else NOTE_TEXT
    path.write_text(template.format(rid=rid, verified=str(verified).lower()), encoding="utf-8")
    if mtime is not None:
        os.utime(path, (mtime, mtime))
    return str(path)


def write_attachment(vault_dir, rid):
    attachments = vault_dir / "attachments"
    attachments.mkdir(exist_ok=True)
    path = attachments / f"{rid}.m4a"
    path.write_bytes(b"fake-m4a-bytes")
    return str(path)


def make_config(tmp_path, **overrides):
    vault_dir = tmp_path / "vault"
    state_dir = tmp_path / "state"
    model_dir = tmp_path / "models"
    vault_dir.mkdir()
    state_dir.mkdir()
    model_dir.mkdir()
    kwargs = {
        "vault_dir": str(vault_dir),
        "state_dir": str(state_dir),
        "model_dir": str(model_dir),
        "model_version": "test-model",
        "listen_addr": ":0",
        "sweep_interval_seconds": 3600,
    }
    kwargs.update(overrides)
    return Config(**kwargs), vault_dir, state_dir


# Fake "embeddings", keyed by recordingId -- fake_read_wav_samples below
# returns these as the "samples" for its recording, and fake_embed_fn just
# echoes samples back as floats, so the numbers chosen here double as the
# vectors gallery.cosine scores against each other.
SAMPLES_BY_RID = {
    "gallery-1": [1, 0],
    "match-1": [1, 0],  # embeds identical to gallery-1 -> likely-owner
    "nomatch-1": [0, 1],  # embeds orthogonal to gallery-1 -> unlikely-owner
    "quiet-1": [0, 0],  # content irrelevant -- speech_band_seconds is faked directly
}


def fake_embed_fn(samples, sample_rate):
    return [float(x) for x in samples]


@pytest.fixture(autouse=True)
def fake_audio(monkeypatch, tmp_path):
    """Fakes the ffmpeg/wave/VAD layer so tests never need real audio.

    decode_to_wav creates a real (empty) temp file so os.remove in
    ensure_scored's finally block has something real to delete; the mapping
    back to a recordingId is carried in a closure dict, not the file itself.
    """
    wav_to_rid: dict[str, str] = {}

    def fake_decode_to_wav(src_path, *, ffmpeg_bin="ffmpeg"):
        rid = os.path.splitext(os.path.basename(src_path))[0]
        fd, wav_path = tempfile.mkstemp(suffix=".wav", dir=str(tmp_path))
        os.close(fd)
        wav_to_rid[wav_path] = rid
        return wav_path

    def fake_read_wav_samples(wav_path):
        rid = wav_to_rid[wav_path]
        return SAMPLES_BY_RID[rid], 16000

    def fake_speech_band_seconds(samples, sample_rate):
        return 2.0  # comfortably above the 1.0s scoreable cutoff

    monkeypatch.setattr(audio, "decode_to_wav", fake_decode_to_wav)
    monkeypatch.setattr(audio, "read_wav_samples", fake_read_wav_samples)
    monkeypatch.setattr(audio, "speech_band_seconds", fake_speech_band_seconds)


# ---------------------------------------------------------------------------
# ensure_scored
# ---------------------------------------------------------------------------


def test_ensure_scored_writes_and_caches_a_record(tmp_path):
    cfg, vault_dir, state_dir = make_config(tmp_path)
    write_attachment(vault_dir, "match-1")

    record = server.ensure_scored("match-1", cfg, fake_embed_fn)

    assert record.unscoreable is False
    assert record.embedding == [1.0, 0.0]
    assert state.read_record(str(state_dir), "match-1") == record


def test_ensure_scored_is_idempotent_and_does_not_recall_embed_fn(tmp_path):
    cfg, vault_dir, _state_dir = make_config(tmp_path)
    write_attachment(vault_dir, "match-1")
    calls = []

    def counting_embed_fn(samples, sample_rate):
        calls.append(1)
        return fake_embed_fn(samples, sample_rate)

    first = server.ensure_scored("match-1", cfg, counting_embed_fn)
    second = server.ensure_scored("match-1", cfg, counting_embed_fn)

    assert first == second
    assert len(calls) == 1  # second call hit the cached record, never re-embedded


def test_ensure_scored_marks_unscoreable_on_decode_failure(tmp_path, monkeypatch):
    cfg, vault_dir, state_dir = make_config(tmp_path)
    write_attachment(vault_dir, "broken-1")

    def failing_decode(src_path, *, ffmpeg_bin="ffmpeg"):
        raise audio.AudioDecodeError("boom")

    monkeypatch.setattr(audio, "decode_to_wav", failing_decode)

    record = server.ensure_scored("broken-1", cfg, fake_embed_fn)

    assert record.unscoreable is True
    assert record.embedding is None
    assert state.read_record(str(state_dir), "broken-1") == record


def test_ensure_scored_marks_unscoreable_when_too_little_speech(tmp_path, monkeypatch):
    cfg, vault_dir, _state_dir = make_config(tmp_path)
    write_attachment(vault_dir, "quiet-1")
    monkeypatch.setattr(audio, "speech_band_seconds", lambda samples, sr: 0.5)

    record = server.ensure_scored("quiet-1", cfg, fake_embed_fn)

    assert record.unscoreable is True
    assert record.embedding is None
    assert record.speech_seconds == 0.5


# ---------------------------------------------------------------------------
# run_sweep_pass
# ---------------------------------------------------------------------------


def test_run_sweep_pass_scores_notes_against_the_gallery(tmp_path):
    cfg, vault_dir, state_dir = make_config(tmp_path)
    old = time.time() - 7200  # older than the 1-hour "don't race Obsidian" guard

    write_attachment(vault_dir, "gallery-1")
    write_attachment(vault_dir, "match-1")
    write_attachment(vault_dir, "nomatch-1")
    write_note(vault_dir, "gallery.md", "gallery-1", verified=True, mtime=old)
    write_note(vault_dir, "match.md", "match-1", verified=False, mtime=old)
    write_note(vault_dir, "nomatch.md", "nomatch-1", verified=False, mtime=old)

    stats = server.run_sweep_pass(cfg, fake_embed_fn)

    match_text = (vault_dir / "2026" / "match.md").read_text(encoding="utf-8")
    nomatch_text = (vault_dir / "2026" / "nomatch.md").read_text(encoding="utf-8")
    assert "voice: likely-owner" in match_text
    assert "voiceScore: 1.0000" in match_text
    assert "voiceModel: test-model" in match_text
    assert "voice: unlikely-owner" in nomatch_text
    assert "voiceScore: 0.0000" in nomatch_text

    record = state.read_record(str(state_dir), "match-1")
    assert record.band == "likely-owner"
    assert record.best == pytest.approx(1.0)

    assert stats["scored"] == 3  # gallery note included: it gets scored too, not just banked
    assert stats["unscoreable"] == 0
    assert stats["no_gallery"] == 0


def test_run_sweep_pass_skips_notes_modified_within_the_last_hour(tmp_path):
    cfg, vault_dir, state_dir = make_config(tmp_path)
    write_attachment(vault_dir, "gallery-1")
    write_attachment(vault_dir, "fresh-1")
    write_note(vault_dir, "gallery.md", "gallery-1", verified=True, mtime=time.time() - 7200)
    fresh_path = write_note(vault_dir, "fresh.md", "fresh-1", verified=False, mtime=time.time())

    stats = server.run_sweep_pass(cfg, fake_embed_fn)

    with open(fresh_path, encoding="utf-8") as f:
        text = f.read()
    assert "voice:" not in text
    assert state.read_record(str(state_dir), "fresh-1") is None
    assert stats["skipped"] >= 1


def test_run_sweep_pass_writes_no_gallery_band_when_nothing_is_ticked(tmp_path):
    cfg, vault_dir, _state_dir = make_config(tmp_path)
    write_attachment(vault_dir, "nomatch-1")
    write_note(vault_dir, "a.md", "nomatch-1", verified=False, mtime=time.time() - 7200)

    stats = server.run_sweep_pass(cfg, fake_embed_fn)

    text = (vault_dir / "2026" / "a.md").read_text(encoding="utf-8")
    assert "voice: no-gallery" in text
    assert "voiceScore:" not in text
    assert stats["no_gallery"] == 1


def test_run_sweep_pass_writes_unscoreable_band_and_no_score(tmp_path, monkeypatch):
    cfg, vault_dir, _state_dir = make_config(tmp_path)
    write_attachment(vault_dir, "quiet-1")
    write_note(vault_dir, "a.md", "quiet-1", verified=False, mtime=time.time() - 7200)
    monkeypatch.setattr(audio, "speech_band_seconds", lambda samples, sr: 0.2)

    stats = server.run_sweep_pass(cfg, fake_embed_fn)

    text = (vault_dir / "2026" / "a.md").read_text(encoding="utf-8")
    assert "voice: unscoreable" in text
    assert "voiceScore:" not in text
    assert stats["unscoreable"] == 1


def test_run_sweep_pass_never_touches_an_already_scored_note(tmp_path):
    cfg, vault_dir, _state_dir = make_config(tmp_path)
    write_attachment(vault_dir, "old-1")
    note_path = write_note(
        vault_dir, "a.md", "old-1", verified=False, mtime=time.time() - 7200, already_scored=True
    )
    with open(note_path, encoding="utf-8") as f:
        before = f.read()

    server.run_sweep_pass(cfg, fake_embed_fn)

    with open(note_path, encoding="utf-8") as f:
        after = f.read()
    assert after == before  # untouched: still the one old voiceModel, no duplicate splice


def test_run_sweep_pass_is_idempotent_across_repeated_calls(tmp_path):
    cfg, vault_dir, _state_dir = make_config(tmp_path)
    write_attachment(vault_dir, "gallery-1")
    write_attachment(vault_dir, "match-1")
    write_note(vault_dir, "gallery.md", "gallery-1", verified=True, mtime=time.time() - 7200)
    note_path = write_note(vault_dir, "match.md", "match-1", verified=False, mtime=time.time() - 7200)

    server.run_sweep_pass(cfg, fake_embed_fn)
    with open(note_path, encoding="utf-8") as f:
        once = f.read()
    server.run_sweep_pass(cfg, fake_embed_fn)
    with open(note_path, encoding="utf-8") as f:
        twice = f.read()

    assert once == twice  # second pass is a true no-op: already carries "voice:"


def test_run_sweep_pass_one_bad_note_does_not_abort_the_rest(tmp_path):
    cfg, vault_dir, _state_dir = make_config(tmp_path)
    write_attachment(vault_dir, "gallery-1")
    write_attachment(vault_dir, "match-1")
    write_note(vault_dir, "gallery.md", "gallery-1", verified=True, mtime=time.time() - 7200)
    write_note(vault_dir, "match.md", "match-1", verified=False, mtime=time.time() - 7200)
    # A note whose recordingId has no fake sample data registered --
    # ensure_scored raises inside the note loop; the sweep must still score
    # the good note rather than aborting on this one.
    write_note(vault_dir, "broken.md", "ghost-1", verified=False, mtime=time.time() - 7200)

    stats = server.run_sweep_pass(cfg, fake_embed_fn)

    match_text = (vault_dir / "2026" / "match.md").read_text(encoding="utf-8")
    assert "voice: likely-owner" in match_text
    assert stats["errors"] >= 1


# ---------------------------------------------------------------------------
# HTTP server
# ---------------------------------------------------------------------------


def start_server(cfg, embed_fn, sweep_lock):
    httpd = server.create_server(cfg, embed_fn, sweep_lock)
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    return httpd, thread


def stop_server(httpd, thread):
    httpd.shutdown()
    thread.join(timeout=2)


def test_get_healthz_returns_200_with_no_body(tmp_path):
    cfg, _vault_dir, _state_dir = make_config(tmp_path)
    httpd, thread = start_server(cfg, fake_embed_fn, threading.Lock())
    try:
        conn = http.client.HTTPConnection("127.0.0.1", httpd.server_port, timeout=2)
        conn.request("GET", "/healthz")
        resp = conn.getresponse()
        assert resp.status == 200
        assert resp.read() == b""
        conn.close()
    finally:
        stop_server(httpd, thread)


def test_unknown_path_returns_404(tmp_path):
    cfg, _vault_dir, _state_dir = make_config(tmp_path)
    httpd, thread = start_server(cfg, fake_embed_fn, threading.Lock())
    try:
        conn = http.client.HTTPConnection("127.0.0.1", httpd.server_port, timeout=2)
        conn.request("GET", "/nope")
        resp = conn.getresponse()
        assert resp.status == 404
        conn.close()

        conn = http.client.HTTPConnection("127.0.0.1", httpd.server_port, timeout=2)
        conn.request("POST", "/nope")
        resp = conn.getresponse()
        assert resp.status == 404
        conn.close()
    finally:
        stop_server(httpd, thread)


def test_post_score_returns_200_immediately_without_waiting_for_the_pass(tmp_path, monkeypatch):
    cfg, _vault_dir, _state_dir = make_config(tmp_path)
    release_call = threading.Event()

    def slow_sweep(cfg_, embed_fn_):
        release_call.wait(timeout=2)

    monkeypatch.setattr(server, "run_sweep_pass", slow_sweep)

    httpd, thread = start_server(cfg, fake_embed_fn, threading.Lock())
    try:
        started = time.time()
        conn = http.client.HTTPConnection("127.0.0.1", httpd.server_port, timeout=2)
        conn.request("POST", "/score")
        resp = conn.getresponse()
        elapsed = time.time() - started

        assert resp.status == 200
        assert elapsed < 1.0  # never waited for the (still-blocked) slow pass
        conn.close()
    finally:
        release_call.set()
        stop_server(httpd, thread)


def test_post_score_debounces_two_rapid_triggers(tmp_path, monkeypatch):
    cfg, _vault_dir, _state_dir = make_config(tmp_path)
    sweep_lock = threading.Lock()

    call_started = threading.Event()
    release_call = threading.Event()
    call_count = {"n": 0}

    def slow_sweep(cfg_, embed_fn_):
        call_count["n"] += 1
        call_started.set()
        release_call.wait(timeout=2)

    monkeypatch.setattr(server, "run_sweep_pass", slow_sweep)

    httpd, thread = start_server(cfg, fake_embed_fn, sweep_lock)
    try:
        conn = http.client.HTTPConnection("127.0.0.1", httpd.server_port, timeout=2)
        conn.request("POST", "/score")
        resp = conn.getresponse()
        assert resp.status == 200
        conn.close()

        assert call_started.wait(timeout=2)  # first pass is now "running", holding sweep_lock

        # A second trigger while the first pass is still in flight must be a
        # debounced no-op, not a second overlapping pass.
        conn2 = http.client.HTTPConnection("127.0.0.1", httpd.server_port, timeout=2)
        conn2.request("POST", "/score")
        resp2 = conn2.getresponse()
        assert resp2.status == 200
        conn2.close()

        time.sleep(0.2)  # give a stray second pass a chance to have started, if debounce broke
        assert call_count["n"] == 1

        release_call.set()
    finally:
        stop_server(httpd, thread)


def test_post_score_starts_a_new_pass_once_the_previous_one_finished(tmp_path, monkeypatch):
    cfg, _vault_dir, _state_dir = make_config(tmp_path)
    sweep_lock = threading.Lock()
    call_count = {"n": 0}
    count_lock = threading.Lock()

    def counting_sweep(cfg_, embed_fn_):
        with count_lock:
            call_count["n"] += 1

    monkeypatch.setattr(server, "run_sweep_pass", counting_sweep)

    httpd, thread = start_server(cfg, fake_embed_fn, sweep_lock)
    try:
        for expected in (1, 2):
            conn = http.client.HTTPConnection("127.0.0.1", httpd.server_port, timeout=2)
            conn.request("POST", "/score")
            resp = conn.getresponse()
            assert resp.status == 200
            conn.close()

            deadline = time.time() + 2
            while call_count["n"] < expected and time.time() < deadline:
                time.sleep(0.02)
            assert call_count["n"] == expected
            # Wait for the background thread to release the lock too, so the
            # next iteration's trigger is never itself debounced by a lock
            # release that's still in flight.
            while sweep_lock.locked() and time.time() < deadline:
                time.sleep(0.02)

        assert call_count["n"] == 2
    finally:
        stop_server(httpd, thread)
