"""Benchmark command for retrieval recall / latency / memory.

Usage:
  python -m workers.embedding.benchmark --allow-test-adapter --synthetic 200 --queries 20 --k 5
  python -m workers.embedding.benchmark --config pinned_models.json --corpus corpus.jsonl --queries queries.jsonl --k 10 --batch-size 32
  python -m workers.embedding.benchmark --help

Inputs:
  - Corpus: JSONL with {"id": str, "text": str}
  - Queries: JSONL with {"id": str, "text": str, "relevant_ids": [str]}  (relevant_ids optional for recall)
  - If --synthetic N given, generates deterministic synthetic corpus + queries derived from corpus.

Reports:
  - Latency p50/p95/p99, mean, throughput (req/s, chunks/s)
  - Memory RSS before/after via psutil or resource
  - Recall@k if relevance judgments present; otherwise synthetic recall via nearest-neighbor self-check
  - Embedding checksums demonstrated (reindex-friendly)

No provider credentials are used.
"""

from __future__ import annotations

import argparse
import json
import math
import os
import random
import sys
import time
from pathlib import Path
from typing import Any

import numpy as np

from . import __version__
from .checksum import content_checksum, embedding_input_checksum
from .config import load_config
from .protocol import EmbedRequest
from .worker import EmbeddingWorker


def _parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        prog="omahab-embedding-benchmark",
        description="Omahab embedding benchmark — recall / latency / memory (reindex checksums included)",
    )
    p.add_argument("--config", help="pinned_models.json path (or EMBEDDING_WORKER_CONFIG)")
    p.add_argument("--model-alias", default="omahab-embed-english", help="model alias to benchmark")
    p.add_argument("--allow-test-adapter", action="store_true", help="allow deterministic test adapter")
    p.add_argument("--corpus", help="path to corpus JSONL (id/text)")
    p.add_argument("--queries", help="path to queries JSONL (id/text/relevant_ids)")
    p.add_argument("--synthetic", type=int, default=0, help="generate synthetic corpus of N docs (and synthetic queries)")
    p.add_argument("--num-queries", type=int, default=20, help="number of synthetic queries when --synthetic used")
    p.add_argument("-k", "--k", type=int, default=5, dest="k", help="recall@k")
    p.add_argument("--batch-size", type=int, default=32, help="batch size for embedding")
    p.add_argument("--iterations", type=int, default=1, help="repeat corpus embedding N times for latency stability")
    p.add_argument("--json", action="store_true", dest="json_out", help="output JSON to stdout instead of human table")
    p.add_argument("--seed", type=int, default=42, help="RNG seed for synthetic data")
    return p.parse_args(argv)


def _load_jsonl(path: Path) -> list[dict[str, Any]]:
    out = []
    with path.open(encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            out.append(json.loads(line))
    return out


def _synthetic_corpus(n_docs: int, seed: int = 42) -> list[dict[str, Any]]:
    rng = random.Random(seed)
    topics = ["paperless", "immich", "forgejo", "hermes", "omahab", "tailscale", "cloudflare", "backup", "syncthing"]
    docs = []
    for i in range(n_docs):
        topic = rng.choice(topics)
        filler = " ".join(rng.choice(topics) for _ in range(rng.randint(5, 15)))
        text = f"Document {i} about {topic}. {filler}. Content seed {seed} doc {i}."
        docs.append({"id": f"doc_{i}", "text": text})
    return docs


def _synthetic_queries(corpus: list[dict[str, Any]], n_queries: int, seed: int = 42) -> list[dict[str, Any]]:
    rng = random.Random(seed + 1)
    queries = []
    for i in range(n_queries):
        # Pick a random doc as relevant; query is paraphrase of its text prefix
        doc = rng.choice(corpus)
        # Simple paraphrase: take first 6 words
        words = doc["text"].split()
        qtext = " ".join(words[:6]) + " ?"
        queries.append({"id": f"q_{i}", "text": qtext, "relevant_ids": [doc["id"]]})
    return queries


def _cosine_sim(a: np.ndarray, b: np.ndarray) -> float:
    # a,b are 1-D normalized vectors; cosine = dot
    return float(np.dot(a, b))


def _recall_at_k(ranked_ids: list[str], relevant: set[str], k: int) -> float:
    if not relevant:
        return float("nan")
    topk = set(ranked_ids[:k])
    hits = len(topk.intersection(relevant))
    return hits / len(relevant)


def _percentile(data: list[float], p: float) -> float:
    if not data:
        return float("nan")
    s = sorted(data)
    k = (len(s) - 1) * p / 100
    f = math.floor(k)
    c = math.ceil(k)
    if f == c:
        return s[int(k)]
    return s[f] * (c - k) + s[c] * (k - f)


def _memory_rss_mb() -> float | None:
    try:
        import psutil  # type: ignore

        proc = psutil.Process(os.getpid())
        return proc.memory_info().rss / (1024 * 1024)
    except ImportError:
        try:
            import resource  # type: ignore

            # ru_maxrss is KB on Linux, bytes on macOS — handle Linux
            rss_kb = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss
            # Linux: KB, macOS: bytes ; heuristic: if > 10**7 assume bytes
            if rss_kb > 10_000_000:
                return rss_kb / (1024 * 1024)
            return rss_kb / 1024
        except Exception:
            return None


def main(argv: list[str] | None = None) -> None:
    args = _parse_args(argv)
    # Env override for allow_test_adapter
    if args.allow_test_adapter:
        os.environ["EMBEDDING_WORKER_ALLOW_TEST_ADAPTER"] = "1"

    try:
        cfg = load_config(args.config)
    except SystemExit as e:
        sys.exit(e.code if isinstance(e.code, int) else 1)

    worker = EmbeddingWorker(cfg)
    alias = args.model_alias
    # Validate alias exists in allowlist at worker level (will raise on embed if invalid)
    from .limits import ALLOWED_ALIASES

    if alias not in ALLOWED_ALIASES:
        print(f"FATAL: model-alias must be one of {sorted(ALLOWED_ALIASES)}", file=sys.stderr)
        sys.exit(2)

    # Load or synthesize data
    corpus: list[dict[str, Any]]
    queries: list[dict[str, Any]]
    if args.synthetic:
        corpus = _synthetic_corpus(args.synthetic, seed=args.seed)
        queries = _synthetic_queries(corpus, args.num_queries, seed=args.seed)
    else:
        if not args.corpus:
            print("Provide --corpus or --synthetic N", file=sys.stderr)
            sys.exit(2)
        corpus = _load_jsonl(Path(args.corpus))
        if args.queries:
            queries = _load_jsonl(Path(args.queries))
        else:
            queries = []

    if not corpus:
        print("Corpus empty", file=sys.stderr)
        sys.exit(2)

    k = args.k
    batch_size = args.batch_size
    iterations = max(1, args.iterations)

    # Checksums for reindex demo
    corpus_checksums = [content_checksum(d["text"]) for d in corpus[:3]]
    batch_checksum_demo = embedding_input_checksum([d["text"] for d in corpus[: min(3, len(corpus))]], alias)

    rss_before = _memory_rss_mb()

    # Warmup (one batch)
    try:
        worker.embed(EmbedRequest(job_id="bench_warmup", model_alias=alias, inputs=[corpus[0]["text"]]))
    except Exception as e:
        print(f"Warmup failed: {e}", file=sys.stderr)
        sys.exit(3)

    # Timed corpus embedding
    latencies: list[float] = []
    all_vectors: list[np.ndarray] = []
    id_to_vec: dict[str, np.ndarray] = {}
    total_chunks = 0
    t0 = time.perf_counter()
    for it in range(iterations):
        iter_vecs: list[np.ndarray] = []
        for i in range(0, len(corpus), batch_size):
            batch = corpus[i : i + batch_size]
            texts = [d["text"] for d in batch]
            ids = [d["id"] for d in batch]
            b0 = time.perf_counter()
            resp = worker.embed(EmbedRequest(job_id=f"bench_c_{it}_{i}", model_alias=alias, inputs=texts))
            b1 = time.perf_counter()
            latencies.append((b1 - b0) / len(texts))  # per-chunk latency
            vecs = [np.array(v, dtype=np.float64) for v in resp.vectors]
            # Verify normalization
            for v in vecs:
                n = float(np.linalg.norm(v))
                if abs(n - 1.0) > 1e-3:
                    print(f"WARNING: vector norm {n} not 1.0", file=sys.stderr)
            if it == 0:
                for doc_id, v in zip(ids, vecs):
                    id_to_vec[doc_id] = v
                    iter_vecs.append(v)
            total_chunks += len(texts)
        if it == 0:
            all_vectors = iter_vecs

    t1 = time.perf_counter()
    rss_after = _memory_rss_mb()
    wall = t1 - t0
    throughput_chunks = total_chunks / wall if wall > 0 else float("nan")
    throughput_reqs = (len(corpus) / batch_size * iterations) / wall if wall > 0 else float("nan")

    # Query embedding + recall
    recall_scores: list[float] = []
    query_latencies: list[float] = []
    if queries:
        for q in queries:
            qtext = q["text"]
            qid = q.get("id", "q")
            b0 = time.perf_counter()
            resp = worker.embed(EmbedRequest(job_id=f"bench_q_{qid}", model_alias=alias, inputs=[qtext]))
            b1 = time.perf_counter()
            query_latencies.append(b1 - b0)
            qvec = np.array(resp.vectors[0], dtype=np.float64)
            # Rank corpus
            sims: list[tuple[float, str]] = []
            for doc in corpus:
                dv = id_to_vec.get(doc["id"])
                if dv is None:
                    continue
                sims.append((_cosine_sim(qvec, dv), doc["id"]))
            sims.sort(reverse=True, key=lambda x: x[0])
            ranked = [doc_id for _, doc_id in sims]
            relevant = set(q.get("relevant_ids") or [])
            if relevant:
                rec = _recall_at_k(ranked, relevant, k)
                if not math.isnan(rec):
                    recall_scores.append(rec)

    mean_recall = float(np.mean(recall_scores)) if recall_scores else float("nan")
    rss_delta = (rss_after - rss_before) if (rss_after is not None and rss_before is not None) else None

    # Build report
    report: dict[str, Any] = {
        "version": __version__,
        "model_alias": alias,
        "model_id": cfg.models.get(alias).model_id if alias in cfg.models else alias,
        "corpus_size": len(corpus),
        "queries": len(queries),
        "batch_size": batch_size,
        "iterations": iterations,
        "k": k,
        "latency_ms": {
            "p50": _percentile([x * 1000 for x in latencies], 50),
            "p95": _percentile([x * 1000 for x in latencies], 95),
            "p99": _percentile([x * 1000 for x in latencies], 99),
            "mean": float(np.mean([x * 1000 for x in latencies])) if latencies else float("nan"),
            "min": min([x * 1000 for x in latencies]) if latencies else float("nan"),
            "max": max([x * 1000 for x in latencies]) if latencies else float("nan"),
        },
        "query_latency_ms": {
            "p50": _percentile([x * 1000 for x in query_latencies], 50) if query_latencies else float("nan"),
            "mean": float(np.mean([x * 1000 for x in query_latencies])) if query_latencies else float("nan"),
        },
        "throughput": {
            "chunks_per_sec": throughput_chunks,
            "requests_per_sec": throughput_reqs,
            "wall_seconds": wall,
        },
        "memory_mb": {
            "rss_before": rss_before,
            "rss_after": rss_after,
            "delta": rss_delta,
        },
        "recall_at_k": {
            "k": k,
            "mean": mean_recall,
            "scores": recall_scores[:10],
            "num_scored": len(recall_scores),
        },
        "checksums": {
            "content_sha256_sample": corpus_checksums,
            "batch_checksum_sample": batch_checksum_demo,
            "reindex_note": "Store content_checksum + model_id per chunk; reindex when chunk_checksum changes.",
        },
    }

    if args.json_out:
        json.dump(report, sys.stdout, indent=2, ensure_ascii=False)
        sys.stdout.write("\n")
    else:
        # Human table
        print(f"Omahab embedding benchmark v{__version__}")
        print(f"Model alias: {alias}  model_id: {report['model_id']}")
        print(f"Corpus: {len(corpus)} docs  Queries: {len(queries)}  k={k}  batch={batch_size}  iter={iterations}")
        print("")
        lm = report["latency_ms"]
        print(f"Chunk latency ms: p50 {lm['p50']:.2f}  p95 {lm['p95']:.2f}  p99 {lm['p99']:.2f}  mean {lm['mean']:.2f}")
        if query_latencies:
            qm = report["query_latency_ms"]
            print(f"Query latency ms: p50 {qm['p50']:.2f}  mean {qm['mean']:.2f}")
        print(f"Throughput: {throughput_chunks:.1f} chunks/s  wall {wall:.2f}s")
        if rss_before is not None:
            print(f"Memory RSS MB: before {rss_before:.1f}  after {rss_after:.1f}  delta {rss_delta:+.1f}" if rss_delta is not None else f"Memory RSS MB: {rss_before:.1f} -> {rss_after:.1f}")
        if recall_scores:
            print(f"Recall@{k}: mean {mean_recall:.3f} over {len(recall_scores)} queries")
        else:
            print(f"Recall@{k}: no relevance judgments (use queries with relevant_ids or synthetic)")
        print(f"Checksum sample: batch {batch_checksum_demo[:16]}...  doc0 {corpus_checksums[0][:16]}...")
        print("Reindex: store chunk_checksum(text, model_id); reindex when it changes.")

    worker.close()


if __name__ == "__main__":
    main()
