"""Adapter interface for embedding backends."""

from __future__ import annotations

from abc import ABC, abstractmethod


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
