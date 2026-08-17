"""Tests for embed.py.

VOICE.md section 4/5 step 5 + the frozen contract's dependency note: torch and
speechbrain are ONLY declared under the optional "ml" extra, never the default
dependency set, so a bare "uv sync" (used for tests/CI) never installs them.
That only works if embed.py imports them lazily, inside load_embedder()'s
body, never at module top level. These tests run in an environment that has
only numpy/pyyaml installed (by design -- see pyproject.toml) and prove that
discipline holds, rather than exercising the real embedder.
"""

import inspect
import sys

import pytest


def test_import_succeeds_without_ml_extras():
    """Importing embed.py must not require torch/speechbrain.

    Proves the lazy-import discipline: if load_embedder's real imports leaked
    to module scope, this import would raise ModuleNotFoundError since this
    test environment deliberately has no ml extras installed.
    """
    assert "torch" not in sys.modules
    assert "speechbrain" not in sys.modules

    from index_note_voice import embed  # noqa: F401

    # Still not imported as a side effect of importing embed.py.
    assert "torch" not in sys.modules
    assert "speechbrain" not in sys.modules


def test_embed_fn_type_alias_exists():
    from index_note_voice import embed

    assert hasattr(embed, "EmbedFn")


def test_load_embedder_is_callable_with_expected_signature():
    """Checks the signature only -- never calls load_embedder here.

    Calling it for real would trigger the lazy torch/speechbrain import,
    which is deliberately absent from this test environment.
    """
    from index_note_voice import embed

    assert callable(embed.load_embedder)
    sig = inspect.signature(embed.load_embedder)
    params = list(sig.parameters)
    assert params == ["model_dir"]


@pytest.mark.skip(
    reason=(
        "Real validation requires torch+speechbrain (the 'ml' extra) and "
        "VOICE_MODEL_DIR pointing at a real speechbrain/spkrec-ecapa-voxceleb "
        "snapshot on disk -- neither is available in the default test env "
        "by design (see pyproject.toml's ml extra / VOICE.md section 4). "
        "Run manually with: uv sync --extra ml && VOICE_MODEL_DIR=... "
        "uv run pytest tests/test_embed.py -k integration"
    )
)
def test_load_embedder_real_integration():
    from index_note_voice import embed

    embed_fn = embed.load_embedder("/path/to/real/model/dir")
    samples = [0] * 16000
    vec = embed_fn(samples, 16000)
    assert len(vec) == 192
