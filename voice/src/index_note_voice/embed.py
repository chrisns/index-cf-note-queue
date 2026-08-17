"""Speaker-embedding seam. VOICE.md section 5 step 5.

torch and speechbrain live only under pyproject.toml's "ml" extra, never the
default dependency set, so plain "uv sync" (used for tests/CI) stays fast and
never pulls a multi-hundred-MB ML install. That only holds if the real
imports happen lazily, inside load_embedder()'s body -- never at module top
level -- so "import index_note_voice.embed" itself works with only
numpy/pyyaml installed. Every other module receives the returned EmbedFn as a
parameter and never imports torch/speechbrain itself.
"""

from collections.abc import Callable

# (samples, sample_rate) -> 192-dim speaker embedding.
EmbedFn = Callable[[list[int], int], list[float]]

# Lazy singleton cache keyed by model_dir, so repeated calls with the same
# model_dir reuse the already-loaded (large, slow-to-load) model.
_classifier_cache: dict[str, object] = {}


def load_embedder(model_dir: str) -> EmbedFn:
    """Loads the SpeechBrain ECAPA classifier and returns an EmbedFn closure.

    Imports torch/speechbrain here, not at module scope -- see module
    docstring. Only calling this function requires the "ml" extra.
    """
    import torch
    from speechbrain.inference import EncoderClassifier

    classifier = _classifier_cache.get(model_dir)
    if classifier is None:
        classifier = EncoderClassifier.from_hparams(
            source="speechbrain/spkrec-ecapa-voxceleb", savedir=model_dir
        )
        _classifier_cache[model_dir] = classifier

    def embed_fn(samples: list[int], sample_rate: int) -> list[float]:
        # int16 -> float32 in [-1, 1], the range encode_batch expects.
        waveform = torch.tensor(samples, dtype=torch.float32) / 32768.0
        embedding = classifier.encode_batch(waveform.unsqueeze(0))
        return embedding.squeeze().tolist()

    return embed_fn
