"""Protocol schemas and error envelope for the embedding worker."""

from __future__ import annotations

import re
from dataclasses import dataclass
from typing import TypedDict, Union

from .limits import ALLOWED_ALIASES, MAX_BATCH_SIZE, MAX_JOB_ID_LENGTH

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

class EmbedUsage(TypedDict):
    input_count: int
    truncated_count: int
    total_chars: int


@dataclass
class EmbedResponse:
    job_id: str
    model_alias: str
    model_id: str
    dimensions: int
    vectors: list[list[float]]
    checksums: list[str]
    usage: EmbedUsage


JsonValue = Union[str, int, float, bool, None, list["JsonValue"], dict[str, "JsonValue"]]


class ErrorBody(TypedDict):
    code: str
    message: str


class ErrorBodyWithDetails(ErrorBody, total=False):
    details: JsonValue


class ErrorEnvelope(TypedDict):
    error: ErrorBody


class ModelHealth(TypedDict):
    ready: bool
    model_id: str
    dimensions: int
    artifact_path: str
    expected_memory_mb: int | None


class HealthSnapshot(TypedDict):
    status: str
    uptime_seconds: float
    models: dict[str, ModelHealth]


class EmbedResponseBody(TypedDict):
    job_id: str
    model_alias: str
    model_id: str
    dimensions: int
    vectors: list[list[float]]
    checksums: list[str]
    usage: EmbedUsage


def error_envelope(code: str, message: str, details: JsonValue | None = None) -> ErrorEnvelope:
    err: ErrorBody = {"code": code, "message": message}
    if details is not None:
        body: ErrorBodyWithDetails = {"code": code, "message": message, "details": details}
        return {"error": body}
    return {"error": err}


class ProtocolError(Exception):
    def __init__(self, code: str, message: str, http_status: int = 400):
        super().__init__(message)
        self.code = code
        self.message = message
        self.http_status = http_status


def parse_embed_request(data: object) -> EmbedRequest:
    """Untrusted JSON boundary: validate keys and types before constructing.

    Malformed input raises ProtocolError with a stable code; a non-string
    object in any string field (e.g. a dict as model_alias) is a 400 here,
    never a raw TypeError deeper in validation.
    """
    if not isinstance(data, dict):
        raise ProtocolError("invalid_json", "request body must be a JSON object", 400)
    job_id = data.get("job_id")
    if not isinstance(job_id, str):
        raise ProtocolError("job_id_required", "job_id is required and must be a non-empty string", 400)
    model_alias = data.get("model_alias")
    if not isinstance(model_alias, str):
        raise ProtocolError("invalid_model_alias", "model_alias must be a string", 400)
    inputs = data.get("inputs")
    if not isinstance(inputs, list):
        raise ProtocolError("inputs_required", "inputs must be a non-empty list of strings", 400)
    for i, item in enumerate(inputs):
        if not isinstance(item, str):
            raise ProtocolError("input_not_string", f"inputs[{i}] must be a string", 400)
    normalize = data.get("normalize", True)
    if normalize is None:
        normalize = True
    if not isinstance(normalize, bool):
        raise ProtocolError("invalid_normalize", "normalize must be a boolean if present", 400)
    req = EmbedRequest(job_id=job_id, model_alias=model_alias, inputs=inputs, normalize=normalize)
    req.validate()
    return req
