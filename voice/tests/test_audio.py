"""Tests for index_note_voice.audio -- see VOICE.md section 5 steps 3-4."""

from __future__ import annotations

import math
import os
import shutil
import struct
import subprocess
import wave

import numpy as np
import pytest

from index_note_voice import audio

SAMPLE_RATE = 16000


def sine_samples(freq: float, seconds: float, sample_rate: int = SAMPLE_RATE, amp: int = 10000) -> list[int]:
    n = int(sample_rate * seconds)
    t = np.arange(n) / sample_rate
    return (amp * np.sin(2 * math.pi * freq * t)).astype(np.int16).tolist()


def write_wav(path: str, samples: list[int], sample_rate: int = SAMPLE_RATE) -> None:
    with wave.open(path, "wb") as wf:
        wf.setnchannels(1)
        wf.setsampwidth(2)
        wf.setframerate(sample_rate)
        wf.writeframes(struct.pack(f"<{len(samples)}h", *samples))


# --- speech_band_seconds ---------------------------------------------------


def test_speech_band_seconds_1khz_tone_registers_as_speech():
    # A 2s tone squarely inside the 300-3400Hz speech band should dominate
    # its own clip peak and be classified as speech for nearly its whole
    # duration (minus one hop's worth of boundary rounding).
    samples = sine_samples(1000, 2.0)
    seconds = audio.speech_band_seconds(samples, SAMPLE_RATE)
    assert seconds >= 1.9


def test_speech_band_seconds_100hz_tone_does_not_dominate_1khz():
    # A 100Hz tone (below the 200Hz rumble line in VOICE.md section 5 step 4)
    # carries negligible energy in the 300-3400Hz band -- concatenated ahead
    # of a 1kHz tone of the same amplitude, it must not inflate the result:
    # the self-relative peak is set by the 1kHz half, so the 100Hz half
    # should score far below the RELATIVE_SPEECH_THRESHOLD and contribute
    # ~nothing to the total.
    combined = sine_samples(100, 1.0) + sine_samples(1000, 1.0)
    seconds = audio.speech_band_seconds(combined, SAMPLE_RATE)
    assert 0.8 <= seconds <= 1.15


def test_speech_band_seconds_pure_rumble_tone_is_not_speech():
    # A tone entirely inside the <200Hz rumble band, nothing else in the
    # clip -- the realistic all-rumble-no-voice case VOICE.md section 5
    # step 4 exists to reject.
    samples = sine_samples(100, 2.0)
    seconds = audio.speech_band_seconds(samples, SAMPLE_RATE)
    assert seconds < 1.0


def test_speech_band_seconds_silence_is_zero():
    samples = [0] * SAMPLE_RATE  # 1s of digital silence
    seconds = audio.speech_band_seconds(samples, SAMPLE_RATE)
    assert seconds == 0.0


def test_speech_band_seconds_shorter_than_one_frame_is_zero():
    samples = sine_samples(1000, 0.01)  # 10ms, shorter than the 30ms window
    seconds = audio.speech_band_seconds(samples, SAMPLE_RATE)
    assert seconds == 0.0


# --- read_wav_samples --------------------------------------------------------


def test_read_wav_samples_roundtrips_int16_mono(tmp_path):
    original = [0, 1000, -1000, 32767, -32768, 500, -500]
    wav_path = str(tmp_path / "sample.wav")
    write_wav(wav_path, original, sample_rate=16000)

    samples, sample_rate = audio.read_wav_samples(wav_path)

    assert samples == original
    assert sample_rate == 16000


def test_read_wav_samples_reports_actual_sample_rate(tmp_path):
    wav_path = str(tmp_path / "sample.wav")
    write_wav(wav_path, [1, 2, 3], sample_rate=8000)

    _, sample_rate = audio.read_wav_samples(wav_path)

    assert sample_rate == 8000


# --- decode_to_wav ------------------------------------------------------------


def test_decode_to_wav_raises_on_missing_source(tmp_path):
    missing = str(tmp_path / "does-not-exist.m4a")
    with pytest.raises(audio.AudioDecodeError):
        audio.decode_to_wav(missing)


def test_decode_to_wav_raises_on_ffmpeg_failure(tmp_path):
    bad = tmp_path / "garbage.m4a"
    bad.write_bytes(b"not a real audio file")
    with pytest.raises(audio.AudioDecodeError) as exc_info:
        audio.decode_to_wav(str(bad))
    assert str(exc_info.value)  # ffmpeg's stderr, non-empty


@pytest.mark.skipif(shutil.which("ffmpeg") is None, reason="ffmpeg not installed")
def test_decode_to_wav_produces_16khz_mono_wav(tmp_path):
    src = tmp_path / "tone.m4a"
    subprocess.run(
        [
            "ffmpeg",
            "-y",
            "-f",
            "lavfi",
            "-i",
            "sine=frequency=1000:duration=1",
            "-ar",
            "44100",
            str(src),
        ],
        check=True,
        capture_output=True,
    )

    wav_path = audio.decode_to_wav(str(src))
    try:
        assert os.path.exists(wav_path)
        with wave.open(wav_path, "rb") as wf:
            assert wf.getframerate() == 16000
            assert wf.getnchannels() == 1
            assert wf.getsampwidth() == 2
        samples, sample_rate = audio.read_wav_samples(wav_path)
        assert sample_rate == 16000
        assert len(samples) > 0
    finally:
        os.remove(wav_path)
