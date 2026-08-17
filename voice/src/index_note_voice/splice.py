"""The first mutation path (VOICE.md section 10).

Everything else in this system is create-only: O_EXCL creates, never
stat-then-rename (SPEC.md section 7.4). Writing the voice band into an
already-existing note is the one deliberate exception, and it is boxed in
here rather than spread through the codebase.

The write is a textual splice, never a YAML round-trip: a parser reorders
keys, restyles quoting and drops comments, giving every note in the vault a
gratuitous diff (VOICE.md section 10). Reading (read_flags) is not the
banned round-trip -- only the write path is -- so it uses yaml.safe_load
freely.
"""
from __future__ import annotations

import glob
import os
import tempfile
from dataclasses import dataclass

import yaml


@dataclass
class NoteFlags:
    verified_me: bool
    recording_id: str | None
    already_scored: bool
    mtime: float


def _extract_frontmatter(text: str) -> dict:
    """Parse the block between the first "---" line and the next bare "---".

    Read-only use of yaml.safe_load -- fine per VOICE.md section 10, which
    only forbids round-tripping on the write path. Any note that doesn't
    open with frontmatter, or whose YAML doesn't parse to a mapping, is
    treated as having none: a corrupt note must not crash the sweep.
    """
    lines = text.splitlines()
    if not lines or lines[0] != "---":
        return {}
    for i in range(1, len(lines)):
        if lines[i] == "---":
            try:
                data = yaml.safe_load("\n".join(lines[1:i]))
            except yaml.YAMLError:
                return {}
            return data if isinstance(data, dict) else {}
    return {}


def read_flags(note_path: str) -> NoteFlags:
    with open(note_path, encoding="utf-8") as f:
        text = f.read()
    frontmatter = _extract_frontmatter(text)
    return NoteFlags(
        # Literal bool True, not merely truthy -- a quoted "true" string
        # must not tick the gallery box (VOICE.md section 6).
        verified_me=frontmatter.get("Verified me", False) is True,
        recording_id=frontmatter.get("recordingId"),
        already_scored="voice" in frontmatter,
        mtime=os.path.getmtime(note_path),
    )


def all_notes(vault_dir: str) -> list[str]:
    # Four-digit year directories only (SPEC.md section 7.1). This glob
    # already excludes attachments/ -- that name isn't four digits -- but
    # the exclusion is intentional, not incidental.
    return glob.glob(os.path.join(vault_dir, "[0-9][0-9][0-9][0-9]", "*.md"))


def splice_voice_fields(
    note_path: str,
    *,
    band: str,
    score: float | None,
    top2_mean: float | None,
    model: str,
    force: bool = False,
) -> None:
    """Splice voice/voiceScore/voiceModel into the note's frontmatter.

    top2_mean is accepted for the caller's convenience but deliberately
    never written: VOICE.md section 7's published block has no
    voiceTop2Mean key, and section 9 keeps it in STATE_DIR only.
    """
    if not force and read_flags(note_path).already_scored:
        return

    with open(note_path, encoding="utf-8") as f:
        lines = f.read().splitlines(keepends=True)

    if not lines or lines[0].rstrip("\n") != "---":
        raise ValueError(f"{note_path}: no opening '---' frontmatter line")

    close_idx = None
    for i in range(1, len(lines)):
        if lines[i].rstrip("\n") == "---":
            close_idx = i
            break
    if close_idx is None:
        raise ValueError(f"{note_path}: frontmatter block never closes with '---'")

    insert = [f"voice: {band}\n"]
    if score is not None:
        insert.append(f"voiceScore: {score:.4f}\n")
    insert.append(f"voiceModel: {model}\n")
    # ponytail: force just inserts another set of lines rather than
    # replacing the old ones (a duplicate `voice:` key), since the contract
    # only asks for insertion and default call sites never pass force=True.
    # Dedup on rescore is a real future need, not a speculative one -- add
    # it when force actually gets used.
    new_text = "".join(lines[:close_idx] + insert + lines[close_idx:])

    dest_dir = os.path.dirname(note_path) or "."
    fd, tmp_path = tempfile.mkstemp(dir=dest_dir)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as tmp_f:
            tmp_f.write(new_text)
            os.chmod(tmp_path, 0o644)
            tmp_f.flush()
            os.fsync(tmp_f.fileno())
        os.replace(tmp_path, note_path)
    except BaseException:
        try:
            os.remove(tmp_path)
        except OSError:
            pass
        raise

    # Best-effort parent-directory flush. NFS gives a weaker guarantee here
    # than local disk, and that weakness is accepted (SPEC.md section 7.8).
    try:
        dir_fd = os.open(dest_dir, os.O_RDONLY)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    except OSError:
        pass
