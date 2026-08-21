"""Embedding worker orchestration — validation, batching, normalization, isolation."""

from __future__ import annotations

import logging
import math
import time
from typing import TYPE_CHECKING

import numpy as np

from .adapters.base import EmbeddingModel
from .adapters.test import DeterministicTestAdapter
from .checksum import chunk_checksum
from .config import ModelPin, WorkerConfig
from .limits import MAX_BATCH_SIZE, MAX_TEXT_LENGTH, MAX_TOTAL_CHARS_PER_REQUEST
from .protocol import EmbedRequest, EmbedResponse, ProtocolError

log = logging.getLogger(__name__)


class EmbeddingWorker:
    """Owns one EmbeddingModel per alias; enforces bounds and normalization."""

    def __init__(self, config: WorkerConfig):
        self.config = config
        self.models: dict[str, EmbeddingModel] = {}
        self._started_at = time.monotonic()
        # Load models eagerly (fail fast handled by config gate, but double-check)
        for alias, pin in config.models.items():
            model = self._load_model(pin)
            self.models[alias] = model
        # If allow_test_adapter and no models configured, caller may lazily create test adapters on demand
        # but we enforce that at request time we can create one.

    def _load_model(self, pin: ModelPin) -> EmbeddingModel:
        if self.config.allow_test_adapter and not pin.artifact_path.exists():
            log.warning(
                "artifact missing for %s — using deterministic test adapter (allow_test_adapter=true)",
                pin.alias,
            )
            return DeterministicTestAdapter(
                model_id=pin.model_id, dimensions=pin.dimensions, max_length=pin.max_sequence_length
            )
        # Prefer ONNX if artifact exists; otherwise test adapter if allowed
        if pin.artifact_path.exists():
            # Try ONNX; if onnxruntime not installed fall back to test adapter only when allowed
            try:
                from .adapters.onnx import OnnxEmbeddingModel

                return OnnxEmbeddingModel(
                    model_id=pin.model_id,
                    artifact_path=pin.artifact_path,
                    dimensions=pin.dimensions,
                    max_length=pin.max_sequence_length,
                )
            except Exception as e:
                if self.config.allow_test_adapter:
                    log.warning(
                        "onnx load failed for %s (%s) — falling back to test adapter", pin.alias, e
                    )
                    return DeterministicTestAdapter(
                        model_id=pin.model_id, dimensions=pin.dimensions, max_length=pin.max_sequence_length
                    )
                raise
        if self.config.allow_test_adapter:
            return DeterministicTestAdapter(
                model_id=pin.model_id, dimensions=pin.dimensions, max_length=pin.max_sequence_length
            )
        raise FileNotFoundError(f"artifact missing for {pin.alias}: {pin.artifact_path}")

    def ensure_model(self, alias: str) -> EmbeddingModel:
        if alias in self.models:
            return self.models[alias]
        # On-demand creation for allow_test_adapter when alias not in config at all
        if self.config.allow_test_adapter:
            # Create a placeholder pin for on-demand alias (dimensions default)
            # We still require alias to be allowlisted (checked by caller) and we synthesize pin.
            pin = self.config.models.get(alias)
            if pin is None:
                # No pin at all — synthesize minimal pin for test mode
                # Use generic model_id = alias
                log.warning("on-demand test adapter for alias %s (no pin)", alias)
                synth = ModelPin(
                    alias=alias,
                    model_id=alias,
                    revision="test",
                    artifact_path=self.config.models_base_dir / alias,
                    artifact_sha256="",
                    dimensions=768 if alias == "omahab-embed-english" else 1024,
                    max_sequence_length=8192,
                    license="test",
                    size_bytes=None,
                    expected_memory_mb=None,
                )
                model = DeterministicTestAdapter(
                    model_id=synth.model_id, dimensions=synth.dimensions, max_length=synth.max_sequence_length
                )
                self.models[alias] = model
                return model
        raise ProtocolError("invalid_model_alias", f"model alias {alias!r} not configured", 400)

    def health_snapshot(self) -> dict:
        models_info: dict[str, dict] = {}
        for alias, pin in self.config.models.items():
            model = self.models.get(alias)
            ready = model is not None
            # Test adapter considered ready even without file
            if self.config.allow_test_adapter and not pin.artifact_path.exists():
                ready = True
            models_info[alias] = {
                "ready": ready,
                "model_id": pin.model_id,
                "dimensions": pin.dimensions,
                "artifact_path": str(pin.artifact_path),
                "expected_memory_mb": pin.expected_memory_mb,
            }
        # Include on-demand models
        for alias, model in self.models.items():
            if alias not in models_info:
                models_info[alias] = {
                    "ready": True,
                    "model_id": model.model_id,
                    "dimensions": model.dimensions,
                    "artifact_path": "test-adapter",
                    "expected_memory_mb": None,
                }
        uptime = time.monotonic() - self._started_at
        # Overall status
        all_ready = all(v["ready"] for v in models_info.values()) if models_info else False
        status = "ok" if all_ready else "degraded"
        return {"status": status, "uptime_seconds": round(uptime, 3), "models": models_info}

    def embed(self, req: EmbedRequest) -> EmbedResponse:
        """Validated embed — bounded, normalized, isolated.

        One inference failure does not crash the worker; caller gets 500 and worker stays alive.
        """
        # Validate (also done by protocol, but double-check)
        req.validate()

        model = self.ensure_model(req.model_alias)
        pin = self.config.models.get(req.model_alias)
        model_id = pin.model_id if pin else model.model_id
        dims = pin.dimensions if pin else model.dimensions

        # Truncate per-input to MAX_TEXT_LENGTH (char level). Count truncated.
        truncated_count = 0
        texts: list[str] = []
        total_chars = 0
        for t in req.inputs:
            total_chars += len(t)
            if len(t) > MAX_TEXT_LENGTH:
                truncated_count += 1
                texts.append(t[:MAX_TEXT_LENGTH])
            else:
                texts.append(t)

        # Total chars guard after truncation
        if sum(len(t) for t in texts) > MAX_TOTAL_CHARS_PER_REQUEST:
            raise ProtocolError(
                "total_chars_too_large",
                f"total chars exceeds {MAX_TOTAL_CHARS_PER_REQUEST} after truncation",
                400,
            )

        # Checksums (reindex-friendly) — over normalized + model-bound
        checksums = [chunk_checksum(t, model_id) for t in texts]

        # Inference — isolated so one failure doesn't crash service
        try:
            # Batching: if batch > internal batch size, chunk internally
            # Enforce MAX_BATCH_SIZE already validated; but for memory we sub-batch to e.g., 32
            sub_batch = 32
            all_vectors: list[list[float]] = []
            for i in range(0, len(texts), sub_batch):
                chunk = texts[i : i + sub_batch]
                vecs = model.embed_texts(chunk)
                # Enforce dimensions and normalization (defensive even if adapter already normalized)
                for v in vecs:
                    _enforce_vector(v, expected_dims=dims)
                # Ensure normalization (adapter already does, but we re-normalize if caller set normalize=true)
                if req.normalize:
                    vecs = [_normalize(v) for v in vecs]
                else:
                    # Still validate but respect caller's choice; log discouragement
                    log.debug("normalize=false requested for job %s", req.job_id)
                all_vectors.extend(vecs)
            # Final shape check
            if len(all_vectors) != len(texts):
                raise RuntimeError(f"adapter returned {len(all_vectors)} vectors for {len(texts)} inputs")
        except ProtocolError:
            raise
        except Exception as e:
            # Never crash worker — convert to 500 error envelope via exception
            log.exception("inference failed for job %s alias %s: %s", req.job_id, req.model_alias, e)
            raise ProtocolError(
                "inference_failed", f"inference failed: {e.__class__.__name__}: {e}", 500
            ) from e

        usage = {
            "input_count": len(texts),
            "truncated_count": truncated_count,
            "total_chars": sum(len(t) for t in texts),
        }

        return EmbedResponse(
            job_id=req.job_id,
            model_alias=req.model_alias,
            model_id=model_id,
            dimensions=dims,
            vectors=all_vectors,
            checksums=checksums,
            usage=usage,
        )

    def close(self) -> None:
        for m in self.models.values():
            try:
                m.close()
            except Exception:
                pass


def _enforce_vector(vec: list[float], expected_dims: int) -> None:
    if len(vec) != expected_dims:
        raise ProtocolError(
            "dimension_mismatch",
            f"vector dimensions {len(vec)} != expected {expected_dims}",
            500,
        )
    for v in vec:
        if not math.isfinite(v):
            raise ProtocolError("non_finite_vector", "vector contains non-finite value", 500)
    # Norm check — allow small epsilon
    arr = np.array(vec, dtype=np.float64)
    n = float(np.linalg.norm(arr))
    if abs(n - 1.0) > 1e-3 and n > 1e-9:
        # If adapter claims normalized but drift >1e-3, treat as error (enforced)
        # But if normalize=false path, we skip strict check — caller asked for raw
        # Here we are in normalized path (checked by caller) so enforce
        raise ProtocolError("not_normalized", f"vector norm {n} != 1.0", 500)


def _normalize(vec: list[float]) -> list[float]:
    arr = np.array(vec, dtype=np.float64)
    n = float(np.linalg.norm(arr))
    if n < 1e-12:
        # Zero vector — return unit vector along first dim (already handled by adapter)
        out = np.zeros_like(arr)
        out[0] = 1.0
        return out.tolist()
    return (arr / n).tolist()
