"""Reindex-friendly checksum helpers.

All derived index storage should keep a content checksum alongside the vector so that
model upgrades or text changes trigger a precise reindex. Checksums are over
normalized text (NFKC + whitespace-collapsed) and are therefore stable across
editor whitespace churn.
"""

from __future__ import annotations

import hashlib
import unicodedata
import re

_WS_RE = re.compile(r"\s+")


def normalize_text(text: str) -> str:
    """Normalize for checksum and retrieval stability.

    - NFKC (compatibility composition)
    - strip leading/trailing whitespace
    - collapse internal whitespace to single space
    - preserves case (embedding is case-sensitive)
    """
    if not isinstance(text, str):
        raise TypeError("text must be str")
    # NFKC first, then whitespace collapse
    t = unicodedata.normalize("NFKC", text)
    t = t.strip()
    t = _WS_RE.sub(" ", t)
    return t


def content_checksum(text: str) -> str:
    """SHA-256 hex of normalized text. Stable input for reindex decisions."""
    norm = normalize_text(text)
    return hashlib.sha256(norm.encode("utf-8")).hexdigest()


def chunk_checksum(text: str, model_id: str, prefix: str = "") -> str:
    """Checksum that binds content to the specific model revision.

    Changing the model_id invalidates the checksum, triggering reindex.
    """
    norm = normalize_text(text)
    h = hashlib.sha256()
    # domain separation
    h.update(b"omahab-embedding-v1\n")
    h.update(model_id.encode("utf-8"))
    h.update(b"\n")
    if prefix:
        h.update(prefix.encode("utf-8"))
        h.update(b"\n")
    h.update(norm.encode("utf-8"))
    return h.hexdigest()


def embedding_input_checksum(texts: list[str], model_id: str) -> str:
    """Single checksum for an ordered batch — useful for request log dedup."""
    h = hashlib.sha256()
    h.update(b"omahab-embedding-batch-v1\n")
    h.update(model_id.encode("utf-8"))
    h.update(b"\n")
    for t in texts:
        norm = normalize_text(t)
        # length-prefix each chunk to avoid concatenation ambiguity
        h.update(str(len(norm)).encode("ascii"))
        h.update(b":")
        h.update(norm.encode("utf-8"))
        h.update(b"\n")
    return h.hexdigest()


def needs_reindex(stored_checksum: str, current_text: str, model_id: str, prefix: str = "") -> bool:
    """Return True if stored checksum no longer matches current content/model."""
    current = chunk_checksum(current_text, model_id, prefix=prefix)
    return stored_checksum != current
