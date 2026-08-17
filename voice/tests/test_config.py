import dataclasses

import pytest

from index_note_voice.config import Config, load_config

REQUIRED = {
    "VAULT_DIR": "/vault",
    "STATE_DIR": "/state",
    "VOICE_MODEL_DIR": "/models",
}


def test_loads_required_vars_and_applies_defaults(monkeypatch):
    monkeypatch.delenv("HF_HUB_OFFLINE", raising=False)

    cfg = load_config(dict(REQUIRED))

    assert cfg == Config(
        vault_dir="/vault",
        state_dir="/state",
        model_dir="/models",
        model_version="ecapa-voxceleb-1",
        listen_addr=":8081",
        sweep_interval_seconds=3600,
    )


def test_load_config_sets_hf_hub_offline(monkeypatch):
    monkeypatch.delenv("HF_HUB_OFFLINE", raising=False)

    load_config(dict(REQUIRED))

    # VOICE.md section 4: HF_HUB_OFFLINE=1 stops SpeechBrain silently reaching
    # out to Hugging Face when the mounted model volume is wrong.
    import os

    assert os.environ["HF_HUB_OFFLINE"] == "1"


def test_config_is_frozen():
    cfg = load_config(dict(REQUIRED))
    with pytest.raises(dataclasses.FrozenInstanceError):
        cfg.vault_dir = "/somewhere-else"


@pytest.mark.parametrize("missing", ["VAULT_DIR", "STATE_DIR", "VOICE_MODEL_DIR"])
def test_missing_required_var_raises_naming_it(missing):
    env = dict(REQUIRED)
    del env[missing]

    with pytest.raises(ValueError, match=missing):
        load_config(env)


@pytest.mark.parametrize("missing", ["VAULT_DIR", "STATE_DIR", "VOICE_MODEL_DIR"])
def test_empty_required_var_is_treated_as_missing(missing):
    env = dict(REQUIRED)
    env[missing] = ""

    with pytest.raises(ValueError, match=missing):
        load_config(env)


def test_optional_vars_are_honoured_when_set():
    env = dict(REQUIRED)
    env["VOICE_MODEL_VERSION"] = "ecapa-voxceleb-2"
    env["LISTEN_ADDR"] = ":9999"
    env["SWEEP_INTERVAL_SECONDS"] = "60"

    cfg = load_config(env)

    assert cfg.model_version == "ecapa-voxceleb-2"
    assert cfg.listen_addr == ":9999"
    assert cfg.sweep_interval_seconds == 60


def test_sweep_interval_seconds_is_an_int():
    cfg = load_config(dict(REQUIRED))
    assert cfg.sweep_interval_seconds == 3600
    assert isinstance(cfg.sweep_interval_seconds, int)


def test_invalid_sweep_interval_seconds_raises():
    env = dict(REQUIRED)
    env["SWEEP_INTERVAL_SECONDS"] = "not-a-number"

    with pytest.raises(ValueError, match="SWEEP_INTERVAL_SECONDS"):
        load_config(env)
