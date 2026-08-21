"""Deterministic lightweight test adapter.

THIS IS NOT PRODUCTION INFERENCE. It exists so CI and unit tests can exercise
the bounded protocol, batching, validation, normalization, and benchmark
without downloading multi-hundred-MB ONNX artifacts.

Behavior:
- For each text, SHA-256(text + model_id) seeds a deterministic PRNG.
- Generates a pseudo-random float vector of the configured dimensions.
- L2-normalizes to unit length.
- Identical input -> identical output; different inputs -> different outputs;
  cosine similarity is deterministic but NOT semantically meaningful.

Any use in production must be explicitly opted in via allow_test_adapter.
"""

from __future__ import annotations

import hashlib
import logging
import math
from typing import TYPE_CHECKING

import numpy as np

from .base import EmbeddingModel

log = logging.getLogger(__name__)


class DeterministicTestAdapter(EmbeddingModel):
    """Deterministic hash-seeded adapter — TEST ONLY."""

    def __init__(self, model_id: str, dimensions: int = 768, max_length: int = 8192):
        # Loud warning — this adapter must never be mistaken for real inference.
        log.warning(
            "USING DETERMINISTIC TEST ADAPTER (model_id=%s, dims=%d) — NOT production inference. "
            "Set allow_test_adapter=true explicitly and never ship this to users.",
            model_id,
            dimensions,
        )
        self._model_id = model_id
        self._dimensions = int(dimensions)
        self._max_length = int(max_length)
        if self._dimensions <= 0 or self._dimensions > 4096:
            raise ValueError(f"invalid dimensions {dimensions}")

    @property
    def model_id(self) -> str:
        return self._model_id

    @property
    def dimensions(self) -> int:
        return self._dimensions

    @property
    def max_length(self) -> int:
        return self._max_length

    def embed_texts(self, texts: list[str]) -> list[list[float]]:
        out: list[list[float]] = []
        for text in texts:
            # Seed from sha256(model_id + "\n" + text)
            h = hashlib.sha256()
            h.update(self._model_id.encode("utf-8"))
            h.update(b"\n")
            h.update(text.encode("utf-8"))
            digest = h.digest()
            # Use first 8 bytes as seed, plus mix in remaining bytes for extra diffusion
            seed = int.from_bytes(digest[:8], "little") & 0xFFFFFFFF
            # Mix second half into seed to avoid collisions on short prefixes
            seed ^= int.from_bytes(digest[8:16], "little") & 0xFFFFFFFF
            rng = np.random.RandomState(seed % (2**32))
            vec = rng.randn(self._dimensions).astype(np.float64)
            # L2 normalize to unit length
            norm = np.linalg.norm(vec)
            if norm < 1e-12:
                # Extremely unlikely with randn, but handle
                vec = np.zeros(self._dimensions, dtype=np.float64)
                vec[0] = 1.0
                norm = 1.0
            else:
                vec /= norm
            # Final check: ensure float list, not numpy
            # Clamp tiny numerical errors
            vec_list = vec.astype(float).tolist()
            # Verify norm ~1 for sanity (debug)
            # (we don't re-check heavy, just ensure not NaN)
            for v in vec_list:
                if not math.isfinite(v):
                    raise RuntimeError("test adapter produced non-finite value")
            out.append(vec_list)
        return out
