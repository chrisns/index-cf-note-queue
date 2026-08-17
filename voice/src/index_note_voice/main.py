"""Process entrypoint: wires config, the embedder, the HTTP server and the
hourly sweep together, and handles SIGTERM. See server.py for the actual
scoring/HTTP logic -- this module is orchestration only.
"""
from __future__ import annotations

import logging
import os
import signal
import threading

from index_note_voice import server
from index_note_voice.config import load_config
from index_note_voice.embed import load_embedder

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)


def main() -> None:
    cfg = load_config(os.environ)
    embed_fn = load_embedder(cfg.model_dir)

    sweep_lock = threading.Lock()
    stop_event = threading.Event()

    httpd = server.create_server(cfg, embed_fn, sweep_lock)

    serving_started = threading.Event()

    def serve() -> None:
        serving_started.set()
        httpd.serve_forever()

    http_thread = threading.Thread(target=serve, daemon=True)

    sweep_thread = threading.Thread(
        target=server.run_hourly_sweep,
        args=(cfg, embed_fn, stop_event, sweep_lock),
        daemon=True,
    )

    def handle_sigterm(signum, frame) -> None:
        logger.info("SIGTERM received, shutting down")
        stop_event.set()
        # serve_forever() only sets its shutdown-ready state once it has
        # actually started running; shutdown() before that deadlocks.
        serving_started.wait()
        httpd.shutdown()

    signal.signal(signal.SIGTERM, handle_sigterm)

    http_thread.start()
    sweep_thread.start()

    logger.info("index-note-voice listening on %s", cfg.listen_addr)

    http_thread.join()
    stop_event.set()
    sweep_thread.join()


if __name__ == "__main__":
    main()
