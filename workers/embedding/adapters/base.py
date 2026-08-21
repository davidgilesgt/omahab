"""Adapter interfaces for tokenizer and embedding backends."""

from __future__ import annotations

from abc import ABC, abstractmethod


class TokenizerAdapter(ABC):
    """Tokenizer owned by the worker — model specific."""

    @property
    @abstractmethod
    def max_length(self) -> int:
        """Maximum sequence length (tokens)."""

    @abstractmethod
    def encode_batch(self, texts: list[str]) -> dict:
        """Encode batch of texts.

        Returns dict with at least ``input_ids`` (list[list[int]]) and
        ``attention_mask`` (list[list[int]]). Implementation may pad/truncate.
        """
        ...

    def encode(self, text: str) -> list[int]:
        out = self.encode_batch([text])
        return out["input_ids"][0]


class EmbeddingBackend(ABC):
    """Low-level ONNX (or other) inference backend."""

    @property
    @abstractmethod
    def dimensions(self) -> int:
        """Output embedding dimensions."""

    @abstractmethod
    def embed_token_batch(
        self, input_ids: list[list[int]], attention_mask: list[list[int]]
    ) -> list[list[float]]:
        """Run inference over already-tokenized batch. Returns list of vectors."""
        ...


class EmbeddingModel(ABC):
    """High-level model: text -> vectors (handles tokenization + pooling + normalization)."""

    @property
    @abstractmethod
    def model_id(self) -> str:
        ...

    @property
    @abstractmethod
    def dimensions(self) -> int:
        ...

    @property
    @abstractmethod
    def max_length(self) -> int:
        ...

    @abstractmethod
    def embed_texts(self, texts: list[str]) -> list[list[float]]:
        """Embed batch of texts; returns already-normalized vectors unless noted."""
        ...

    def close(self) -> None:
        """Optional cleanup (e.g., close ORT session)."""
        pass
