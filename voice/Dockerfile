FROM python:3.14-slim

# VOICE.md section 5 step 3: ffmpeg binary must be on PATH to decode m4a -> wav.
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg \
    && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir uv

WORKDIR /app

# Copy project metadata first so the (large, ml-extra) dependency install is
# its own cacheable layer, independent of source code churn. --no-install-project
# skips installing the local package itself, which needs src/ (not copied yet).
COPY pyproject.toml uv.lock* ./
RUN uv sync --extra ml --no-dev --no-install-project

# Now bring in the source and install the project itself -- fast, deps are
# already resolved and cached in the layer above.
COPY src/ ./src/
RUN uv sync --extra ml --no-dev

ENV PATH="/app/.venv/bin:$PATH"
# VOICE.md section 4: set this so SpeechBrain cannot silently reach out to
# Hugging Face when VOICE_MODEL_DIR / the mounted volume is wrong.
ENV HF_HUB_OFFLINE=1
ENV PYTHONDONTWRITEBYTECODE=1

# Non-root, matching the Go service's USER 65532:65532 (SPEC.md section 8.2)
# so the same numeric runAsUser/runAsNonRoot hardening applies here too.
# HOME=/tmp: the only writable path under a read-only root filesystem is the
# /tmp emptyDir the Deployment mounts, so anything defaulting to $HOME/.cache
# (torch, huggingface_hub) lands there instead of failing to create ~/.cache.
RUN chown -R 65532:65532 /app
USER 65532:65532
ENV HOME=/tmp

EXPOSE 8081

# VOICE_MODEL_DIR is a runtime-mounted read-only volume (VOICE.md section 4)
# -- no model weights are baked into this image.
ENTRYPOINT ["python", "-m", "index_note_voice.main"]
