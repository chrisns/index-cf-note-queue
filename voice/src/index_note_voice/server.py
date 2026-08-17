"""Integration layer: the scoring pass plus the stdlib HTTP server that
triggers it (VOICE.md sections 5, 8). Imports every other module for real.
"""
from __future__ import annotations

import logging
import os
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from index_note_voice import audio, gallery, splice, state
from index_note_voice.config import Config
from index_note_voice.embed import EmbedFn
from index_note_voice.state import Record

logger = logging.getLogger(__name__)


def ensure_scored(recording_id: str, cfg: Config, embed_fn: EmbedFn) -> Record:
    """VOICE.md section 5 steps 2-5, idempotent: a cached record is reused."""
    existing = state.read_record(cfg.state_dir, recording_id)
    if existing is not None:
        return existing

    m4a_path = f"{cfg.vault_dir}/attachments/{recording_id}.m4a"
    try:
        wav_path = audio.decode_to_wav(m4a_path)
    except audio.AudioDecodeError:
        record = Record(
            recording_id=recording_id,
            unscoreable=True,
            embedding=None,
            speech_seconds=0.0,
            model=cfg.model_version,
        )
        state.write_record(cfg.state_dir, record)
        return record

    try:
        samples, sample_rate = audio.read_wav_samples(wav_path)
    finally:
        os.remove(wav_path)

    seconds = audio.speech_band_seconds(samples, sample_rate)
    # VOICE.md section 5 step 4: under 1.0s of speech-band energy is unscoreable.
    if seconds < 1.0:
        record = Record(
            recording_id=recording_id,
            unscoreable=True,
            embedding=None,
            speech_seconds=seconds,
            model=cfg.model_version,
        )
    else:
        embedding = embed_fn(samples, sample_rate)
        record = Record(
            recording_id=recording_id,
            unscoreable=False,
            embedding=embedding,
            speech_seconds=seconds,
            model=cfg.model_version,
        )

    state.write_record(cfg.state_dir, record)
    return record


def run_sweep_pass(cfg: Config, embed_fn: EmbedFn) -> dict:
    """VOICE.md section 5 end to end. Returns a small stats dict for logging
    only -- never written anywhere.
    """
    stats = {"scored": 0, "unscoreable": 0, "no_gallery": 0, "skipped": 0, "errors": 0}

    # Steps 1+2 combined: this both embeds every gallery candidate and every
    # other unscored attachment, since ensure_scored is a cheap no-op for
    # anything already cached. Same per-item isolation as the note loop below
    # -- one bad attachment must never abort the whole sweep.
    for recording_id in state.list_attachment_ids(cfg.vault_dir):
        try:
            ensure_scored(recording_id, cfg, embed_fn)
        except Exception:
            logger.exception("sweep: failed to score attachment %s", recording_id)
            stats["errors"] += 1

    # Named gallery_embeddings, not gallery, so it never shadows the imported
    # gallery module used below.
    gallery_embeddings = gallery.build_gallery_from_state(cfg.vault_dir, cfg.state_dir)

    for note_path in splice.all_notes(cfg.vault_dir):
        try:
            flags = splice.read_flags(note_path)
            if flags.already_scored or flags.recording_id is None:
                stats["skipped"] += 1
                continue
            # VOICE.md section 10: skip any note modified in the last hour,
            # so the scorer never races the owner editing in Obsidian.
            if (time.time() - flags.mtime) < 3600:
                stats["skipped"] += 1
                continue

            record = ensure_scored(flags.recording_id, cfg, embed_fn)

            if record.unscoreable:
                splice.splice_voice_fields(
                    note_path,
                    band="unscoreable",
                    score=None,
                    top2_mean=None,
                    model=cfg.model_version,
                )
                stats["unscoreable"] += 1
                continue

            if not gallery_embeddings:
                splice.splice_voice_fields(
                    note_path,
                    band="no-gallery",
                    score=None,
                    top2_mean=None,
                    model=cfg.model_version,
                )
                stats["no_gallery"] += 1
                continue

            best, top2 = gallery.score_against_gallery(record.embedding, gallery_embeddings)
            band = gallery.band_for(best)
            record.best, record.top2_mean, record.band = best, top2, band
            state.write_record(cfg.state_dir, record)
            splice.splice_voice_fields(
                note_path,
                band=band,
                score=best,
                top2_mean=top2,
                model=cfg.model_version,
            )
            stats["scored"] += 1
        except Exception:
            # VOICE.md/SPEC.md section 2: "nothing monitors this system" --
            # one bad note must never abort the whole sweep.
            logger.exception("sweep: failed to process note %s", note_path)
            stats["errors"] += 1

    return stats


def run_hourly_sweep(
    cfg: Config,
    embed_fn: EmbedFn,
    stop_event: threading.Event,
    sweep_lock: threading.Lock,
) -> None:
    """VOICE.md section 8: the timer path. Exits cleanly when stop_event is set."""
    while not stop_event.is_set():
        with sweep_lock:
            try:
                run_sweep_pass(cfg, embed_fn)
            except Exception:
                logger.exception("hourly sweep: pass failed")
        stop_event.wait(cfg.sweep_interval_seconds)


def make_handler(cfg: Config, embed_fn: EmbedFn, sweep_lock: threading.Lock) -> type[BaseHTTPRequestHandler]:
    """Builds the request handler class, closing over cfg/embed_fn/sweep_lock."""

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, format: str, *args) -> None:
            logger.info("%s - %s", self.address_string(), format % args)

        def do_POST(self) -> None:
            if self.path != "/score":
                self.send_response(404)
                self.end_headers()
                return

            # VOICE.md section 8: "it is a poke, not a delivery" -- always
            # returns 200 immediately, never waits for the pass to finish.
            # A non-blocking lock acquire is the debounce: if a pass is
            # already running (webhook or hourly timer), a second POST
            # /score just returns without starting a duplicate one.
            if sweep_lock.acquire(blocking=False):

                def run_and_release() -> None:
                    try:
                        run_sweep_pass(cfg, embed_fn)
                    except Exception:
                        logger.exception("triggered sweep: pass failed")
                    finally:
                        sweep_lock.release()

                try:
                    threading.Thread(target=run_and_release, daemon=True).start()
                except RuntimeError:
                    # Thread creation failed (e.g. an OS thread-count limit)
                    # after the lock was already acquired; run_and_release
                    # never got to run its release, so release it here.
                    # Otherwise every future /score and the hourly sweep
                    # would wedge on this lock forever, unnoticed (nothing
                    # monitors this system).
                    logger.exception("triggered sweep: could not start thread")
                    sweep_lock.release()

            self.send_response(200)
            self.end_headers()

        def do_GET(self) -> None:
            if self.path != "/healthz":
                self.send_response(404)
                self.end_headers()
                return
            self.send_response(200)
            self.end_headers()

        def do_HEAD(self) -> None:
            self.send_response(404)
            self.end_headers()

    return Handler


def create_server(cfg: Config, embed_fn: EmbedFn, sweep_lock: threading.Lock) -> ThreadingHTTPServer:
    """Builds the ThreadingHTTPServer bound to cfg.listen_addr.

    listen_addr follows the Go service's own "[host]:port" convention (e.g.
    ":8081" for all interfaces) -- Python's HTTPServer already treats an
    empty host string as "listen on all interfaces", so the split below is
    all that's needed.
    """
    host, _, port = cfg.listen_addr.rpartition(":")
    return ThreadingHTTPServer((host, int(port)), make_handler(cfg, embed_fn, sweep_lock))
