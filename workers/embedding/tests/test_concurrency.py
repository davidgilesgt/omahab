"""Bounded-inference invariant: ONNX calls never overlap; failures release the lock."""

from __future__ import annotations

import threading
from pathlib import Path

from workers.embedding.adapters.base import EmbeddingModel
from workers.embedding.config import WorkerConfig
from workers.embedding.protocol import EmbedRequest, ProtocolError
from workers.embedding.worker import EmbeddingWorker


MODEL_ID = "nomic-ai/nomic-embed-text-v1.5"
DIMS = 768
ALIAS = "omahab-embed-english"


def one_hot(idx: int) -> list[float]:
    vec = [0.0] * DIMS
    vec[idx % DIMS] = 1.0
    return vec


class BlockingModel(EmbeddingModel):
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self.active = 0
        self.max_active = 0
        self.entered_first = threading.Event()
        self.release = threading.Event()
        self.attempted_second = threading.Event()

    @property
    def model_id(self) -> str:
        return MODEL_ID

    @property
    def dimensions(self) -> int:
        return DIMS

    @property
    def max_length(self) -> int:
        return 8192

    def embed_texts(self, texts: list[str]) -> list[list[float]]:
        with self._lock:
            self.active += 1
            self.max_active = max(self.max_active, self.active)
        try:
            if not self.entered_first.is_set():
                self.entered_first.set()
                assert self.release.wait(timeout=10), "release not signaled"
            return [one_hot(i) for i in range(len(texts))]
        finally:
            with self._lock:
                self.active -= 1


def make_worker_with_model(model: EmbeddingModel) -> EmbeddingWorker:
    cfg = WorkerConfig(models={}, models_base_dir=Path("/tmp"), allow_test_adapter=True, config_path=None)
    worker = EmbeddingWorker(cfg)
    worker.models[ALIAS] = model
    return worker


def make_req(job: str, text: str) -> EmbedRequest:
    return EmbedRequest(job_id=job, model_alias=ALIAS, inputs=[text], normalize=True)


def test_concurrent_requests_never_overlap_inference() -> None:
    model = BlockingModel()
    worker = make_worker_with_model(model)
    errors: list[BaseException] = []

    def first() -> None:
        try:
            worker.embed(make_req("job-first", "hello one"))
        except BaseException as e:  # noqa: BLE001
            errors.append(e)

    def second() -> None:
        model.attempted_second.set()
        try:
            worker.embed(make_req("job-second", "hello two"))
        except BaseException as e:  # noqa: BLE001
            errors.append(e)

    t1 = threading.Thread(target=first)
    t1.start()
    assert model.entered_first.wait(timeout=10), "first request never entered inference"
    t2 = threading.Thread(target=second)
    t2.start()
    assert model.attempted_second.wait(timeout=10), "second request never attempted"
    with model._lock:
        assert model.max_active == 1, f"max_active={model.max_active}, want 1 (no overlap)"
        assert model.active == 1, f"active={model.active}, want 1 while first blocked"
    model.release.set()
    t1.join(timeout=10)
    t2.join(timeout=10)
    assert not t1.is_alive(), "first thread hung"
    assert not t2.is_alive(), "second thread hung (lock not released?)"
    assert not errors, f"unexpected errors: {errors}"
    assert model.max_active == 1, f"final max_active={model.max_active}, want 1"


class FailOnceModel(EmbeddingModel):
    def __init__(self) -> None:
        self.calls = 0
        self._lock = threading.Lock()

    @property
    def model_id(self) -> str:
        return MODEL_ID

    @property
    def dimensions(self) -> int:
        return DIMS

    @property
    def max_length(self) -> int:
        return 8192

    def embed_texts(self, texts: list[str]) -> list[list[float]]:
        with self._lock:
            self.calls += 1
            call = self.calls
        if call == 1:
            raise RuntimeError("boom")
        return [one_hot(i) for i in range(len(texts))]


def test_inference_exception_does_not_retain_lock() -> None:
    model = FailOnceModel()
    worker = make_worker_with_model(model)
    try:
        worker.embed(make_req("job-fail", "first"))
    except ProtocolError as e:
        assert e.code == "inference_failed"
    else:
        raise AssertionError("expected inference_failed")
    result_holder: dict[str, object] = {}

    def second() -> None:
        try:
            resp = worker.embed(make_req("job-ok", "second"))
            result_holder["vectors"] = resp.vectors
        except BaseException as e:  # noqa: BLE001
            result_holder["error"] = e

    t = threading.Thread(target=second)
    t.start()
    t.join(timeout=10)
    assert not t.is_alive(), "second request hung: lock retained after failure"
    assert "error" not in result_holder, f"second failed: {result_holder.get('error')}"
    vectors = result_holder.get("vectors")
    assert isinstance(vectors, list) and len(vectors) == 1
