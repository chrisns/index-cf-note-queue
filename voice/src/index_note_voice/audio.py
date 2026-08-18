"""Decode owner-ring clips and run the speech-band VAD.

Implements VOICE.md section 5 steps 3-4:
  step 3: decode the m4a attachment to 16kHz mono 16-bit PCM with ffmpeg.
  step 4: reject as unscoreable when speech-band duration is under 1.0s,
          measuring energy in 300-3400Hz rather than raw energy, because the
          owner's clips carry 60-75% of their total energy below 200Hz as
          handling rumble (a naive energy VAD would measure the rumble).

Reads WAV via the stdlib "wave" module only -- VOICE.md section 5 step 5:
torchaudio.load now requires torchcodec and will fail without it.
"""

from __future__ import annotations

import os
import subprocess
import tempfile
import wave

import numpy as np


class AudioDecodeError(Exception):
    """Raised when ffmpeg cannot decode src_path, or src_path is missing."""


# Measured against the first 11 owner-ring clips (2.1-5.8s each), which the
# previous constants rejected 11 times out of 11:
#   * The reference level is the PEAK_PERCENTILE-th percentile of in-band frame
#     energy, not the maximum. A single handling click sets a maximum up to
#     14.5x the 95th percentile, and RELATIVE_SPEECH_THRESHOLD of that click
#     sits above every genuinely spoken frame.
#   * RUMBLE_RATIO_THRESHOLD was 1.0, which required the 300-3400Hz band to
#     out-energise the <200Hz band outright. Those clips run 0.018-0.135 on
#     that ratio, so the gate could never pass. Candidate speech frames bottom
#     out at 0.005, so the threshold is 0.001.
#   * That threshold only separates voice from rumble because the frame is
#     Hann-windowed first. With the rectangular window this used to use, a pure
#     <200Hz tone leaks across the whole spectrum and scores 0.007-0.063 on the
#     speech/rumble ratio -- inside the 0.018-0.135 band real speech occupies,
#     so no threshold could tell them apart. The single exception is a tone
#     sitting exactly on an FFT bin centre, which leaks nothing: bins are
#     16000/480 = 33.333Hz apart, so 100Hz is exactly bin 3 and measures 1e-28.
#     Calibrating on 100Hz alone is what hid this. Hann drops the leakage to
#     1e-6..8e-5 and leaves genuine speech energy untouched.
# The remaining constants, and the 0.1 fraction itself, are still uncalibrated
# placeholders. VOICE.md section 11 is explicit that the band thresholds in its
# own table come from only nine recordings -- "a starting point, not a
# calibration" -- and nobody has yet checked any of this against labelled
# owner/impostor audio. Revisit once labelled audio
# exists; do not treat this VAD as validated in the meantime.
FRAME_MS = 30  # analysis window length, uncalibrated placeholder
FRAME_OVERLAP = 0.5  # fraction of the window overlapped by the next frame, uncalibrated placeholder
RELATIVE_SPEECH_THRESHOLD = 0.1  # fraction of this clip's reference 300-3400Hz frame energy, uncalibrated placeholder
PEAK_PERCENTILE = 95  # percentile of in-band frame energy used as the clip's reference level
RUMBLE_RATIO_THRESHOLD = 0.001  # frame's 300-3400Hz energy must exceed this multiple of its own <200Hz energy


def decode_to_wav(src_path: str, *, ffmpeg_bin: str = "ffmpeg") -> str:
    """Decode src_path (an m4a file) to a fresh 16kHz mono 16-bit PCM WAV.

    Returns the temp WAV path; the caller owns it and must delete it.
    """
    if not os.path.exists(src_path):
        raise AudioDecodeError(f"source file does not exist: {src_path}")

    with tempfile.NamedTemporaryFile(suffix=".wav", delete=False) as tmp:
        wav_path = tmp.name

    # VOICE.md section 5 steps 3+5: 16kHz mono 16-bit PCM is what the ECAPA
    # embedder (via read_wav_samples) expects.
    try:
        result = subprocess.run(
            [
                ffmpeg_bin,
                "-y",
                "-i",
                src_path,
                "-ar",
                "16000",
                "-ac",
                "1",
                "-acodec",
                "pcm_s16le",
                wav_path,
            ],
            capture_output=True,
            text=True,
            check=False,
        )
    except OSError as exc:
        try:
            os.remove(wav_path)
        except OSError:
            pass
        raise AudioDecodeError(str(exc)) from exc
    if result.returncode != 0:
        try:
            os.remove(wav_path)
        except OSError:
            pass
        raise AudioDecodeError(result.stderr)

    return wav_path


def read_wav_samples(wav_path: str) -> tuple[list[int], int]:
    """Read a mono 16-bit PCM WAV via the stdlib "wave" module.

    Returns (samples, sample_rate) with samples as a flat list of signed
    16-bit ints. See the module docstring for why torchaudio is not used.
    """
    with wave.open(wav_path, "rb") as wf:
        sample_rate = wf.getframerate()
        raw = wf.readframes(wf.getnframes())
    samples = np.frombuffer(raw, dtype=np.int16).tolist()
    return samples, sample_rate


def speech_band_seconds(samples: list[int], sample_rate: int) -> float:
    """Seconds of 300-3400Hz speech-band energy, per VOICE.md section 5 step 4.

    Splits samples into overlapping frames, sums FFT power in the
    300-3400Hz band per frame, and marks a frame "speech" when its
    in-band energy exceeds RELATIVE_SPEECH_THRESHOLD times this clip's
    reference in-band energy (the PEAK_PERCENTILE-th percentile across its
    frames) -- self-relative, not an absolute floor, since absolute levels
    vary by recording gain -- AND
    exceeds RUMBLE_RATIO_THRESHOLD times that same frame's <200Hz rumble
    energy, per VOICE.md section 5 step 4 ("measure energy in 300-3400Hz
    against energy below 200Hz").
    """
    frame_len = int(sample_rate * FRAME_MS / 1000)
    hop_len = int(frame_len * (1 - FRAME_OVERLAP))
    arr = np.asarray(samples, dtype=np.float64)
    if frame_len <= 0 or hop_len <= 0 or len(arr) < frame_len:
        return 0.0

    freqs = np.fft.rfftfreq(frame_len, d=1.0 / sample_rate)
    speech_mask = (freqs >= 300) & (freqs <= 3400)
    # VOICE.md section 5 step 4 names the <200Hz rumble band as the thing a
    # naive VAD measures instead of voice, and requires speech energy to be
    # measured against it. A frame only counts as speech once it also clears
    # RUMBLE_RATIO_THRESHOLD times its own rumble energy (see is_speech
    # below), not just the self-relative peak check.
    rumble_mask = freqs < 200

    # Hann, not rectangular. A rectangular window smears a pure <200Hz tone
    # across the speech band at a level indistinguishable from real speech,
    # which makes the rumble ratio below meaningless. See the note above.
    window = np.hanning(frame_len)

    num_frames = 1 + (len(arr) - frame_len) // hop_len
    speech_energy = np.empty(num_frames)
    rumble_energy = np.empty(num_frames)
    for i in range(num_frames):
        start = i * hop_len
        frame = arr[start : start + frame_len] * window
        power = np.abs(np.fft.rfft(frame)) ** 2
        speech_energy[i] = power[speech_mask].sum()
        rumble_energy[i] = power[rumble_mask].sum()

    if speech_energy.max() <= 0:
        return 0.0
    # Percentile, not max: one broadband handling click would otherwise set the
    # reference so high that RELATIVE_SPEECH_THRESHOLD of it clears every
    # spoken frame. See the calibration note above the constants.
    reference = np.percentile(speech_energy, PEAK_PERCENTILE)

    is_speech = (speech_energy > RELATIVE_SPEECH_THRESHOLD * reference) & (
        speech_energy > RUMBLE_RATIO_THRESHOLD * rumble_energy
    )
    # Frames overlap by FRAME_OVERLAP, so each speech frame contributes only
    # its hop length (not its full window) to the total, and overlapping
    # time is never double-counted.
    return float(is_speech.sum() * hop_len / sample_rate)
