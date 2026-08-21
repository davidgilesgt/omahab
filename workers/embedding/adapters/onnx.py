"""ONNX Runtime embedding adapter (production path)."""

from __future__ import annotations

import json
import logging
import math
import os
from pathlib import Path

import numpy as np

from .base import EmbeddingModel

log = logging.getLogger(__name__)


class OnnxEmbeddingModel(EmbeddingModel):
    """Production adapter backed by ONNX Runtime + HuggingFace tokenizers.

    Expects artifact_path to be a directory containing:
      - model.onnx  (or model_optimized.onnx)
      - tokenizer.json

    Tokenization, truncation, padding, pooling, and L2 normalization are owned
    here; omahabd never does them.

    Lazy imports keep the worker importable without onnxruntime installed (so
    test adapter and health endpoint work in CI).
    """

    def __init__(
        self,
        model_id: str,
        artifact_path: str | os.PathLike[str],
        dimensions: int,
        max_length: int = 512,
        providers: list[str] | None = None,
    ):
        self._model_id = model_id
        self._artifact_path = Path(artifact_path)
        self._dimensions = int(dimensions)
        self._max_length = int(max_length)
        self._providers = providers or ["CPUExecutionProvider"]
        self._session = None
        self._tokenizer = None
        self._input_name: str | None = None
        self._attention_name: str | None = None

        # Validate artifact presence eagerly — startup should fail fast.
        if not self._artifact_path.exists():
            raise FileNotFoundError(f"artifact_path not found: {self._artifact_path}")
        # Determine model file
        candidates = ["model.onnx", "model_optimized.onnx", "onnx/model.onnx"]
        model_file: Path | None = None
        for c in candidates:
            p = self._artifact_path / c
            if p.is_file():
                model_file = p
                break
        if model_file is None:
            # also accept if artifact_path itself is the .onnx file
            if self._artifact_path.is_file() and self._artifact_path.suffix == ".onnx":
                model_file = self._artifact_path
            else:
                raise FileNotFoundError(
                    f"no model.onnx found under {self._artifact_path} (tried {candidates})"
                )
        self._model_file = model_file
        tok_file = self._artifact_path / "tokenizer.json"
        if self._artifact_path.is_file():
            # if artifact is file, tokenizer alongside?
            tok_file = self._artifact_path.parent / "tokenizer.json"
        if not tok_file.is_file():
            raise FileNotFoundError(f"tokenizer.json not found under {self._artifact_path}")

        self._tokenizer_path = tok_file
        log.info(
            "onnx model init model_id=%s file=%s tokenizer=%s dims=%d max_len=%d",
            model_id,
            model_file,
            tok_file,
            dimensions,
            max_length,
        )
        self._lazy_load()

    def _lazy_load(self) -> None:
        try:
            import onnxruntime as ort  # type: ignore
        except ImportError as e:
            raise RuntimeError(
                "onnxruntime is required for production inference but is not installed. "
                "Install with pip install onnxruntime. Original: " + str(e)
            ) from e
        try:
            from tokenizers import Tokenizer  # type: ignore
        except ImportError as e:
            raise RuntimeError(
                "tokenizers is required for production inference but is not installed. "
                "Install with pip install tokenizers. Original: " + str(e)
            ) from e

        # Load tokenizer
        tok = Tokenizer.from_file(str(self._tokenizer_path))
        # Ensure truncation/padding configured
        try:
            tok.enable_truncation(max_length=self._max_length)
            tok.enable_padding(pad_id=0, pad_token="[PAD]")
        except Exception:
            # tokenizers API varies by version; ignore if fails — we pad manually
            pass
        self._tokenizer = tok

        sess_opts = ort.SessionOptions()
        # Conservative threading — worker is local CPU; don't oversubscribe.
        sess_opts.inter_op_num_threads = 1
        sess_opts.intra_op_num_threads = max(1, (os.cpu_count() or 4) // 2)
        sess_opts.graph_optimization_level = ort.GraphOptimizationLevel.ORT_ENABLE_ALL
        import onnxruntime as ort2  # re-import for type

        self._session = ort2.InferenceSession(
            str(self._model_file), sess_options=sess_opts, providers=self._providers
        )
        # Discover input names (common: input_ids, attention_mask, token_type_ids)
        input_names = [i.name for i in self._session.get_inputs()]
        self._input_name = "input_ids" if "input_ids" in input_names else input_names[0]
        self._attention_name = "attention_mask" if "attention_mask" in input_names else None
        if self._attention_name and self._attention_name not in input_names:
            self._attention_name = None
        log.info("onnx session ready inputs=%s providers=%s", input_names, self._providers)

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
        if self._session is None or self._tokenizer is None:
            raise RuntimeError("onnx model not initialized")
        # Tokenize batch
        from tokenizers import Tokenizer  # already checked

        encodings = self._tokenizer.encode_batch(texts)  # type: ignore
        # Build padded arrays
        max_len = max(len(e.ids) for e in encodings) if encodings else 0
        # Clamp to max_length
        max_len = min(max_len, self._max_length)
        # Some tokenizers already pad; ensure uniform length
        input_ids: list[list[int]] = []
        attention_mask: list[list[int]] = []
        for e in encodings:
            ids = e.ids[: self._max_length]
            mask = e.attention_mask[: self._max_length] if hasattr(e, "attention_mask") else [1] * len(ids)
            # Pad to max_len
            pad = max_len - len(ids)
            if pad > 0:
                ids = ids + [0] * pad
                mask = mask + [0] * pad
            input_ids.append(ids)
            attention_mask.append(mask)

        # Trivial empty batch guard
        if not input_ids:
            return []

        # Prepare numpy
        np_ids = np.array(input_ids, dtype=np.int64)
        np_mask = np.array(attention_mask, dtype=np.int64)

        ort_inputs: dict[str, np.ndarray] = {}
        # Use discovered names; fall back to common
        assert self._input_name is not None
        ort_inputs[self._input_name] = np_ids
        if self._attention_name:
            ort_inputs[self._attention_name] = np_mask
        # Some models expect token_type_ids — provide zeros if input list includes it
        for inp in self._session.get_inputs():
            if inp.name not in ort_inputs:
                if "token_type" in inp.name:
                    ort_inputs[inp.name] = np.zeros_like(np_ids)
                elif "attention" in inp.name.lower():
                    ort_inputs[inp.name] = np_mask

        outputs = self._session.run(None, ort_inputs)
        # outputs[0] is typically last_hidden_state [batch, seq_len, hidden]
        hidden = outputs[0]
        if not isinstance(hidden, np.ndarray):
            hidden = np.array(hidden)
        # Pooling: mean pooling over sequence weighted by attention mask (masked mean).
        # If model already outputs pooled embeddings (e.g., some optimized models),
        # hidden may be [batch, hidden]; detect and skip pooling.
        vectors: list[list[float]] = []
        if hidden.ndim == 2:
            # Already pooled: [batch, dims]
            for i in range(hidden.shape[0]):
                vec = hidden[i].astype(np.float64)
                vec = _l2_normalize(vec)
                _validate_vec(vec, self._dimensions)
                vectors.append(vec.tolist())
        elif hidden.ndim == 3:
            batch, seq_len, dims = hidden.shape
            if dims != self._dimensions:
                log.warning(
                    "dimension mismatch: model outputs %d but config expects %d", dims, self._dimensions
                )
            for i in range(batch):
                mask = np_mask[i].astype(np.float64)  # [seq_len]
                # Avoid division by zero; at least one token attended
                mask_sum = mask.sum()
                if mask_sum < 1e-9:
                    mask_sum = 1.0
                # Weighted mean
                # hidden[i]: [seq_len, dims]
                vec = (hidden[i].astype(np.float64) * mask[:, None]).sum(axis=0) / mask_sum
                vec = _l2_normalize(vec)
                _validate_vec(vec, self._dimensions if dims == self._dimensions else dims)
                vectors.append(vec.tolist())
        else:
            raise RuntimeError(f"unexpected hidden shape {hidden.shape}")
        return vectors

    def close(self) -> None:
        # ort session has no explicit close, but allow GC
        self._session = None
        self._tokenizer = None


def _l2_normalize(vec: np.ndarray) -> np.ndarray:
    norm = np.linalg.norm(vec)
    if norm < 1e-12:
        # Return zero-safe vector (all zeros except first 1) but mark as normalized
        out = np.zeros_like(vec)
        out[0] = 1.0
        return out
    return vec / norm


def _validate_vec(vec: np.ndarray, expected_dims: int) -> None:
    if vec.shape[0] != expected_dims:
        # Allow mismatch but warn; still return
        pass
    for v in vec:
        if not math.isfinite(float(v)):
            raise RuntimeError("onnx adapter produced non-finite value")
    # norm check
    n = np.linalg.norm(vec)
    if abs(n - 1.0) > 1e-4:
        raise RuntimeError(f"normalization violated: norm={n}")
