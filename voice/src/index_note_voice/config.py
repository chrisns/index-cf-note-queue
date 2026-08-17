"""Startup config, read once. Mirrors the Go service's startup() (SPEC.md
section 6.7) in spirit: fail fast and loud on missing required config, name
the missing var, never fall back to a guessed default for anything required.
"""

import os
from collections.abc import Mapping
from dataclasses import dataclass


@dataclass(frozen=True)
class Config:
    vault_dir: str
    state_dir: str
    model_dir: str
    model_version: str
    listen_addr: str
    sweep_interval_seconds: int


def load_config(env: Mapping[str, str]) -> Config:
    vault_dir = _require(env, "VAULT_DIR")
    state_dir = _require(env, "STATE_DIR")
    model_dir = _require(env, "VOICE_MODEL_DIR")

    model_version = env.get("VOICE_MODEL_VERSION") or "ecapa-voxceleb-1"
    listen_addr = env.get("LISTEN_ADDR") or ":8081"

    sweep_interval_raw = env.get("SWEEP_INTERVAL_SECONDS") or "3600"
    try:
        sweep_interval_seconds = int(sweep_interval_raw)
    except ValueError as exc:
        raise ValueError(
            f"SWEEP_INTERVAL_SECONDS: not an integer: {sweep_interval_raw!r}"
        ) from exc

    # VOICE.md section 4: set HF_HUB_OFFLINE=1 so SpeechBrain cannot silently
    # reach out to Hugging Face when VOICE_MODEL_DIR turns out to be wrong.
    os.environ["HF_HUB_OFFLINE"] = "1"

    return Config(
        vault_dir=vault_dir,
        state_dir=state_dir,
        model_dir=model_dir,
        model_version=model_version,
        listen_addr=listen_addr,
        sweep_interval_seconds=sweep_interval_seconds,
    )


def _require(env: Mapping[str, str], name: str) -> str:
    value = env.get(name)
    if not value:
        raise ValueError(f"{name} is not set")
    return value
