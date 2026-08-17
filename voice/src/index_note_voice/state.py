"""Per-recording state: <STATE_DIR>/embeddings/<recordingId>.json (VOICE.md section 9)."""

import glob
import json
import logging
import os
import tempfile
from dataclasses import asdict, dataclass

logger = logging.getLogger(__name__)


@dataclass
class Record:
    recording_id: str
    unscoreable: bool
    embedding: list[float] | None
    speech_seconds: float
    model: str
    best: float | None = None
    top2_mean: float | None = None
    band: str | None = None


def record_path(state_dir: str, recording_id: str) -> str:
    return os.path.join(state_dir, "embeddings", f"{recording_id}.json")


def read_record(state_dir: str, recording_id: str) -> Record | None:
    path = record_path(state_dir, recording_id)
    try:
        with open(path) as f:
            data = json.load(f)
        return Record(**data)
    except FileNotFoundError:
        return None
    except (json.JSONDecodeError, OSError, TypeError) as exc:
        # A corrupt state file must not crash the sweep (VOICE.md section 9 / 5 step 7).
        logger.warning("state: could not read record %s: %s", path, exc)
        return None


def write_record(state_dir: str, record: Record) -> None:
    embeddings_dir = os.path.join(state_dir, "embeddings")
    os.makedirs(embeddings_dir, exist_ok=True)
    dest = record_path(state_dir, record.recording_id)

    # Same-directory temp file + os.replace mirrors note.go's write discipline
    # (SPEC.md section 7.8): atomic on POSIX, never a bare open-and-truncate.
    fd, tmp_path = tempfile.mkstemp(dir=embeddings_dir)
    try:
        with os.fdopen(fd, "w") as f:
            json.dump(asdict(record), f)
            os.chmod(tmp_path, 0o644)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp_path, dest)
    except BaseException:
        try:
            os.remove(tmp_path)
        except OSError:
            pass
        raise

    # Best-effort parent-directory flush, same acceptance as splice.py's note
    # writes: NFS gives a weaker guarantee here than local disk.
    try:
        dir_fd = os.open(embeddings_dir, os.O_RDONLY)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    except OSError:
        pass


def list_attachment_ids(vault_dir: str) -> list[str]:
    # Go service names attachments exactly "<recordingId>.m4a" (SPEC.md section 7.1).
    paths = glob.glob(os.path.join(vault_dir, "attachments", "*.m4a"))
    return [os.path.splitext(os.path.basename(p))[0] for p in paths]
