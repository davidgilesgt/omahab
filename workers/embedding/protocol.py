"""Protocol schemas and error envelope for the embedding worker."""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from typing import Any

from .limits import ALLOWED_ALIASES, MAX_BATCH_SIZE, MAX_JOB_ID_LENGTH, MAX_TEXT_LENGTH

_JOB_ID_RE = re.compile(r"^[A-Za-z0-9._-]{1,128}$")


@dataclass(frozen=True)
class EmbedRequest:
    job_id: str
    model_alias: str
    inputs: list[str]
    normalize: bool = True

    def validate(self) -> None:
        if not self.job_id or not isinstance(self.job_id, str):
            raise ProtocolError("job_id_required", "job_id is required and must be a non-empty string", 400)
        if len(self.job_id) > MAX_JOB_ID_LENGTH:
            raise ProtocolError("job_id_too_long", f"job_id exceeds {MAX_JOB_ID_LENGTH} chars", 400)
        if not _JOB_ID_RE.match(self.job_id):
            raise ProtocolError(
                "job_id_invalid",
                "job_id must match ^[A-Za-z0-9._-]+$ (1-128 chars)",
                400,
            )
        if self.model_alias not in ALLOWED_ALIASES:
            raise ProtocolError(
                "invalid_model_alias",
                f"model_alias must be one of {sorted(ALLOWED_ALIASES)}; got {self.model_alias!r}",
                400,
            )
        if not isinstance(self.inputs, list) or len(self.inputs) == 0:
            raise ProtocolError("inputs_required", "inputs must be a non-empty list of strings", 400)
        if len(self.inputs) > MAX_BATCH_SIZE:
            raise ProtocolError(
                "batch_too_large",
                f"batch size {len(self.inputs)} exceeds limit {MAX_BATCH_SIZE}",
                400,
            )
        total_chars = 0
        for i, t in enumerate(self.inputs):
            if not isinstance(t, str):
                raise ProtocolError("input_not_string", f"inputs[{i}] must be a string", 400)
            if t == "":
                raise ProtocolError("input_empty", f"inputs[{i}] must not be empty", 400)
            if len(t) > 50_000:
                # hard reject absurdly large individual chunks even before truncation counting
                # (MAX_TEXT_LENGTH truncation is handled in worker.py; this is DoS guard)
                raise ProtocolError(
                    "input_too_large",
                    f"inputs[{i}] length {len(t)} exceeds hard limit 50000",
                    400,
                )
            total_chars += len(t)
        # total chars guard is also enforced in worker, but early check here
        from .limits import MAX_TOTAL_CHARS_PER_REQUEST

        if total_chars > MAX_TOTAL_CHARS_PER_REQUEST:
            raise ProtocolError(
                "total_chars_too_large",
                f"total chars {total_chars} exceeds limit {MAX_TOTAL_CHARS_PER_REQUEST}",
                400,
            )


@dataclass
class EmbedResponse:
    job_id: str
    model_alias: str
    model_id: str
    dimensions: int
    vectors: list[list[float]]
    checksums: list[str]
    usage: dict[str, Any]


def error_envelope(code: str, message: str, details: dict[str, Any] | None = None) -> dict[str, Any]:
    err: dict[str, Any] = {"code": code, "message": message}
    if details:
        err["details"] = details
    return {"error": err}


class ProtocolError(Exception):
    def __init__(self, code: str, message: str, http_status: int = 400):
        super().__init__(message)
        self.code = code
        self.message = message
        self.http_status = http_status


def parse_embed_request(data: dict[str, Any]) -> EmbedRequest:
    if not isinstance(data, dict):
        raise ProtocolError("invalid_json", "request body must be a JSON object", 400)
    job_id = data.get("job_id")
    model_alias = data.get("model_alias")
    inputs = data.get("inputs")
    normalize = data.get("normalize", True)
    if normalize is not None and not isinstance(normalize, bool):
        raise ProtocolError("invalid_normalize", "normalize must be a boolean if present", 400)
    req = EmbedRequest(
        job_id=job_id if isinstance(job_id, str) else job_id or "",
        model_alias=model_alias if isinstance(model_alias, str) else model_alias or "",
        inputs=inputs if isinstance(inputs, list) else [],  # type: ignore
        normalize=bool(normalize) if normalize is not None else True,
    )
    req.validate()
    return req


HEALTH_OK = "ok"
HEALTH_DEGRADED = "degraded"
HEALTH_UNHEALTHY = "unhealthy"
