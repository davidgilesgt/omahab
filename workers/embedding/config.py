"""Pinned artifact configuration and startup validation.

Production startup MUST fail clearly without pinned artifacts. No arbitrary
paths/models are ever loaded — artifact_path must be inside models_base_dir and
alias must be allowlisted.
"""

from __future__ import annotations

import hashlib
import json
import os
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .limits import ALLOWED_ALIASES, MAX_DIMENSIONS, MIN_DIMENSIONS


@dataclass(frozen=True)
class ModelPin:
    alias: str
    model_id: str
    revision: str
    artifact_path: Path
    artifact_sha256: str
    dimensions: int
    max_sequence_length: int
    license: str
    size_bytes: int | None
    expected_memory_mb: int | None


@dataclass(frozen=True)
class WorkerConfig:
    models: dict[str, ModelPin]
    models_base_dir: Path
    allow_test_adapter: bool
    config_path: Path | None

    def get_pin(self, alias: str) -> ModelPin:
        return self.models[alias]


# Default search order for config file
_DEFAULT_CANDIDATES = [
    Path("/etc/omahab/embedding/pinned_models.json"),
    Path("workers/embedding/pinned_models.json"),
    Path("pinned_models.json"),
]


def _candidate_paths(explicit: str | os.PathLike[str] | None) -> list[Path]:
    if explicit is not None:
        return [Path(explicit)]
    env = os.environ.get("EMBEDDING_WORKER_CONFIG", "").strip()
    if env:
        return [Path(env)]
    out: list[Path] = []
    # Also try path relative to this file (repo-root discovery)
    # workers/embedding/config.py -> repo root ../../..
    try:
        repo_root = Path(__file__).resolve().parents[2]
        out.append(repo_root / "workers" / "embedding" / "pinned_models.json")
    except Exception:
        pass
    out.extend(_DEFAULT_CANDIDATES)
    return out


def load_config(explicit_path: str | os.PathLike[str] | None = None) -> WorkerConfig:
    """Load and validate pinned config. On failure, raise SystemExit with clear message.

    Callers (server startup) should catch SystemExit and present to stderr / exit non-zero.
    """
    candidates = _candidate_paths(explicit_path)
    chosen: Path | None = None
    data: dict[str, Any] | None = None
    last_err: str | None = None
    for p in candidates:
        if p.is_file():
            try:
                raw = p.read_text(encoding="utf-8")
                data = json.loads(raw)
                chosen = p
                break
            except Exception as e:
                last_err = f"{p}: {e}"
                continue
    if data is None or chosen is None:
        tried = ", ".join(str(c) for c in candidates)
        msg = (
            f"FATAL: pinned embedding config not found. Tried: {tried}. "
            f"Provide --config or set EMBEDDING_WORKER_CONFIG, or place pinned_models.json at "
            f"/etc/omahab/embedding/pinned_models.json. "
            f"See workers/embedding/pinned_models.json.example. Last error: {last_err}"
        )
        print(msg, file=sys.stderr)
        raise SystemExit(2)

    # Parse and validate
    try:
        cfg = _parse_config(data, chosen)
    except SystemExit:
        raise
    except Exception as e:
        print(f"FATAL: invalid pinned config {chosen}: {e}", file=sys.stderr)
        raise SystemExit(2) from e

    # Env overrides
    env_base = os.environ.get("EMBEDDING_WORKER_MODELS_BASE_DIR", "").strip()
    env_allow = os.environ.get("EMBEDDING_WORKER_ALLOW_TEST_ADAPTER", "").strip().lower()
    override_base = Path(env_base) if env_base else None
    override_allow: bool | None = None
    if env_allow in ("1", "true", "yes", "on"):
        override_allow = True
    elif env_allow in ("0", "false", "no", "off"):
        override_allow = False

    if override_base is not None or override_allow is not None:
        # Rebuild with overrides (need to re-validate base containment)
        models = cfg.models
        base = override_base if override_base is not None else cfg.models_base_dir
        allow = override_allow if override_allow is not None else cfg.allow_test_adapter
        # Re-validate artifact paths inside new base
        for alias, pin in models.items():
            _assert_inside_base(pin.artifact_path, base, alias)
        cfg = WorkerConfig(
            models=models,
            models_base_dir=base,
            allow_test_adapter=allow,
            config_path=chosen,
        )

    # Startup gate: verify artifacts exist unless test adapter allowed and alias intentionally missing?
    # Policy:
    # - If allow_test_adapter is False: every allowlisted alias present in config is required and its
    #   artifact must exist and sha256 must match if provided.
    # - If allow_test_adapter is True: we still validate entries that exist, but missing artifact
    #   is tolerated (test adapter will be used). However if artifact_path is present but file missing,
    #   we warn but do not fail, because test mode is for CI.
    gate_errors: list[str] = []
    for alias, pin in cfg.models.items():
        if alias not in ALLOWED_ALIASES:
            gate_errors.append(f"alias {alias!r} not in allowlist {sorted(ALLOWED_ALIASES)}")
            continue
        _assert_inside_base(pin.artifact_path, cfg.models_base_dir, alias)
        if not cfg.allow_test_adapter:
            # Production: must exist
            if not pin.artifact_path.exists():
                gate_errors.append(
                    f"pinned artifact missing for alias {alias!r}: {pin.artifact_path} not found "
                    f"(model_id={pin.model_id}). Provide the pinned artifact or fix artifact_path."
                )
                continue
            # SHA256 check if provided and not placeholder zeros
            if pin.artifact_sha256 and not _is_placeholder_sha(pin.artifact_sha256):
                actual = _sha256_of_path(pin.artifact_path)
                if actual.lower() != pin.artifact_sha256.lower():
                    gate_errors.append(
                        f"artifact_sha256 mismatch for alias {alias!r}: expected {pin.artifact_sha256} "
                        f"got {actual} at {pin.artifact_path}"
                    )
        else:
            # Test mode: if artifact_sha256 is placeholder zeros, skip check
            if pin.artifact_path.exists() and pin.artifact_sha256 and not _is_placeholder_sha(pin.artifact_sha256):
                actual = _sha256_of_path(pin.artifact_path)
                if actual.lower() != pin.artifact_sha256.lower():
                    gate_errors.append(
                        f"artifact_sha256 mismatch for alias {alias!r}: expected {pin.artifact_sha256} got {actual}"
                    )

    # Also require that at least one model is configured. In production, both should be present.
    if not cfg.allow_test_adapter and len(cfg.models) == 0:
        gate_errors.append("no models pinned in config; at least one of {} required".format(sorted(ALLOWED_ALIASES)))

    if gate_errors:
        for e in gate_errors:
            print(f"FATAL: {e}", file=sys.stderr)
        raise SystemExit(3)

    return cfg


def _parse_config(data: dict[str, Any], path: Path) -> WorkerConfig:
    if not isinstance(data, dict):
        raise ValueError("config root must be an object")
    models_raw = data.get("models")
    if not isinstance(models_raw, dict) or len(models_raw) == 0:
        raise ValueError("'models' must be a non-empty object mapping alias -> pin")
    base_raw = data.get("models_base_dir", "/var/lib/omahab/models")
    if not isinstance(base_raw, str) or not base_raw.strip():
        raise ValueError("models_base_dir must be a non-empty string")
    base = Path(base_raw.strip())
    if not base.is_absolute():
        # Allow relative base relative to config file's directory
        base = (path.parent / base).resolve()
    allow_test = bool(data.get("allow_test_adapter", False))

    models: dict[str, ModelPin] = {}
    for alias, pin_raw in models_raw.items():
        if alias not in ALLOWED_ALIASES:
            raise ValueError(f"alias {alias!r} not in allowlist {sorted(ALLOWED_ALIASES)}")
        if not isinstance(pin_raw, dict):
            raise ValueError(f"pin for {alias!r} must be an object")
        model_id = pin_raw.get("model_id")
        revision = pin_raw.get("revision", "")
        artifact_path_raw = pin_raw.get("artifact_path")
        artifact_sha = pin_raw.get("artifact_sha256", "")
        dimensions = pin_raw.get("dimensions")
        max_len = pin_raw.get("max_sequence_length", 512)
        license_s = pin_raw.get("license", "unknown")
        size_bytes = pin_raw.get("size_bytes")
        expected_mem = pin_raw.get("expected_memory_mb")

        if not isinstance(model_id, str) or not model_id.strip():
            raise ValueError(f"{alias}: model_id must be non-empty string")
        if not isinstance(artifact_path_raw, str) or not artifact_path_raw.strip():
            raise ValueError(f"{alias}: artifact_path must be non-empty string")
        if not isinstance(dimensions, int):
            raise ValueError(f"{alias}: dimensions must be int")
        if dimensions < MIN_DIMENSIONS or dimensions > MAX_DIMENSIONS:
            raise ValueError(f"{alias}: dimensions {dimensions} out of bounds [{MIN_DIMENSIONS},{MAX_DIMENSIONS}]")
        if not isinstance(max_len, int) or max_len <= 0 or max_len > 32768:
            raise ValueError(f"{alias}: max_sequence_length must be int in 1..32768")
        if artifact_sha is not None and not isinstance(artifact_sha, str):
            raise ValueError(f"{alias}: artifact_sha256 must be string")
        artifact_sha = (artifact_sha or "").strip()
        if artifact_sha and len(artifact_sha) != 64:
            # allow empty to skip check in dev, but if present must be 64 hex
            # placeholder zeros are 64 zeros — allowed but then _is_placeholder will skip verification
            try:
                int(artifact_sha, 16)
            except ValueError:
                raise ValueError(f"{alias}: artifact_sha256 must be 64 hex chars")
            if len(artifact_sha) != 64:
                raise ValueError(f"{alias}: artifact_sha256 must be 64 hex chars")

        ap = Path(artifact_path_raw.strip())
        if not ap.is_absolute():
            ap = (path.parent / ap).resolve()
        else:
            ap = ap.resolve() if ap.exists() else Path(artifact_path_raw.strip())

        # No provider credentials should ever appear in pin
        for forbidden in ("api_key", "token", "secret", "credentials", "provider"):
            if forbidden in pin_raw:
                raise ValueError(f"{alias}: pin must not contain '{forbidden}' (no provider credentials)")

        _assert_inside_base(ap, base.resolve() if base.exists() else base, alias)

        pin = ModelPin(
            alias=alias,
            model_id=model_id.strip(),
            revision=str(revision).strip(),
            artifact_path=ap,
            artifact_sha256=artifact_sha.lower(),
            dimensions=int(dimensions),
            max_sequence_length=int(max_len),
            license=str(license_s),
            size_bytes=int(size_bytes) if isinstance(size_bytes, int) else None,
            expected_memory_mb=int(expected_mem) if isinstance(expected_mem, int) else None,
        )
        models[alias] = pin

    return WorkerConfig(models=models, models_base_dir=base, allow_test_adapter=allow_test, config_path=path)


def _assert_inside_base(artifact: Path, base: Path, alias: str) -> None:
    """Ensure artifact_path is inside models_base_dir (no arbitrary path escape)."""
    try:
        # Use resolve to handle symlinks; if artifact doesn't exist, use absolute + normalise
        art = artifact.resolve() if artifact.exists() else Path(os.path.abspath(str(artifact)))
        b = base.resolve() if base.exists() else Path(os.path.abspath(str(base)))
        # commonpath check
        common = os.path.commonpath([str(b), str(art)])
        if common != str(b):
            raise ValueError(
                f"alias {alias!r}: artifact_path {artifact} is outside models_base_dir {base} "
                f"(resolved {art} not under {b})"
            )
    except ValueError as e:
        # Re-raise with context if it was our own check; otherwise wrap
        if "outside models_base_dir" in str(e):
            raise
        raise ValueError(f"alias {alias!r}: invalid path containment check: {e}") from e


def _is_placeholder_sha(sha: str) -> bool:
    return sha == "0" * 64 or sha == "1" * 64 or sha.strip() == ""


def _sha256_of_path(p: Path) -> str:
    """Compute sha256 of file or directory (directory = hash of file list + contents)."""
    h = hashlib.sha256()
    if p.is_file():
        with p.open("rb") as f:
            for chunk in iter(lambda: f.read(1024 * 1024), b""):
                h.update(chunk)
        return h.hexdigest()
    if p.is_dir():
        # Deterministic directory hash: sort file paths, hash path + content
        files = sorted(f for f in p.rglob("*") if f.is_file())
        if not files:
            # Empty dir: hash of empty
            return h.hexdigest()
        for f in files:
            rel = f.relative_to(p).as_posix()
            h.update(rel.encode("utf-8"))
            h.update(b"\n")
            with f.open("rb") as fh:
                for chunk in iter(lambda: fh.read(1024 * 1024), b""):
                    h.update(chunk)
            h.update(b"\n")
        return h.hexdigest()
    # Not found — caller should have checked exists; return empty hash
    return h.hexdigest()
