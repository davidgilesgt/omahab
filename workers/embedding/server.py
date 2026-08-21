"""Bounded HTTP + Unix socket server for the embedding worker."""

from __future__ import annotations

import argparse
import json
import logging
import os
import signal
import socket
import socketserver
import sys
import threading
import time
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from urllib.parse import urlparse

from . import __version__
from .config import WorkerConfig, load_config
from .limits import DEFAULT_HOST, DEFAULT_PORT, MAX_REQUEST_BYTES
from .protocol import error_envelope, parse_embed_request, ProtocolError
from .worker import EmbeddingWorker

log = logging.getLogger("omahab.embedding")

# --- HTTP handler ---


class EmbeddingHTTPHandler(BaseHTTPRequestHandler):
    """Shared handler for both TCP loopback and UDS."""

    # Class-level shared state
    worker: EmbeddingWorker | None = None
    worker_config: WorkerConfig | None = None
    start_time: float = 0.0

    def log_message(self, fmt: str, *args) -> None:
        # Structured log without secrets; never log inputs/vectors
        # UDS client_address is a string (""), TCP is (host, port)
        if isinstance(self.client_address, (list, tuple)) and len(self.client_address) > 0:
            client = str(self.client_address[0])
        else:
            client = str(self.client_address) if self.client_address else "uds"
            if not client or client == "''":
                client = "uds"
        log.info("%s - - [%s] %s", client, self.log_date_time_string(), fmt % args)
    def do_GET(self) -> None:
        parsed = urlparse(self.path)
        path = parsed.path.rstrip("/") or "/"
        if path in ("/health", "/up", "/api/v1/health", "/healthz"):
            self._handle_health()
        else:
            self._send_json(HTTPStatus.NOT_FOUND, error_envelope("not_found", f"unknown path {path}"))

    def do_POST(self) -> None:
        parsed = urlparse(self.path)
        path = parsed.path.rstrip("/") or "/"
        if path in ("/embed", "/v1/embed", "/api/v1/embed"):
            self._handle_embed()
        else:
            self._send_json(HTTPStatus.NOT_FOUND, error_envelope("not_found", f"unknown path {path}"))

    def _handle_health(self) -> None:
        assert self.worker is not None
        snap = self.worker.health_snapshot()
        body = {
            "status": snap["status"],
            "version": __version__,
            "uptime_seconds": snap["uptime_seconds"],
            "models": snap["models"],
        }
        # Always 200 for health; degraded is still 200 with status field
        self._send_json(HTTPStatus.OK, body)

    def _handle_embed(self) -> None:
        # Enforce Content-Type for mutating requests (per contract: application/json required)
        ctype = self.headers.get("Content-Type", "")
        if "application/json" not in ctype:
            self._send_json(
                HTTPStatus.UNSUPPORTED_MEDIA_TYPE,
                error_envelope(
                    "invalid_content_type", "Content-Type must be application/json for POST /embed"
                ),
            )
            return

        length = int(self.headers.get("Content-Length", "0"))
        if length > MAX_REQUEST_BYTES:
            self._send_json(
                HTTPStatus.REQUEST_ENTITY_TOO_LARGE,
                error_envelope("request_too_large", f"body {length} exceeds {MAX_REQUEST_BYTES}"),
            )
            return
        if length == 0:
            self._send_json(HTTPStatus.BAD_REQUEST, error_envelope("invalid_json", "empty body"))
            return
        try:
            raw = self.rfile.read(length)
        except Exception as e:
            self._send_json(HTTPStatus.BAD_REQUEST, error_envelope("read_failed", str(e)))
            return
        try:
            data = json.loads(raw)
        except Exception as e:
            self._send_json(HTTPStatus.BAD_REQUEST, error_envelope("invalid_json", f"invalid JSON: {e}"))
            return

        try:
            req = parse_embed_request(data)
        except ProtocolError as e:
            self._send_json(HTTPStatus(e.http_status), error_envelope(e.code, e.message))
            return
        except Exception as e:
            self._send_json(HTTPStatus.BAD_REQUEST, error_envelope("invalid_request", str(e)))
            return

        # Delegate to worker — isolated, one failure does not crash service
        assert self.worker is not None
        try:
            resp = self.worker.embed(req)
        except ProtocolError as e:
            http_status = HTTPStatus(e.http_status)
            # inference_failed is 500; bounds are 400
            log.warning("embed failed job=%s alias=%s code=%s: %s", req.job_id, req.model_alias, e.code, e.message)
            self._send_json(http_status, error_envelope(e.code, e.message))
            return
        except Exception as e:
            log.exception("unexpected embed failure job=%s", req.job_id)
            self._send_json(
                HTTPStatus.INTERNAL_SERVER_ERROR, error_envelope("internal_error", str(e))
            )
            return

        out = {
            "job_id": resp.job_id,
            "model_alias": resp.model_alias,
            "model_id": resp.model_id,
            "dimensions": resp.dimensions,
            "vectors": resp.vectors,
            "checksums": resp.checksums,
            "usage": resp.usage,
        }
        self._send_json(HTTPStatus.OK, out)

    def _send_json(self, status: HTTPStatus, obj: dict) -> None:
        body = json.dumps(obj, ensure_ascii=False).encode("utf-8")
        self.send_response(int(status))
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        # No caching for embedding responses
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        try:
            self.wfile.write(body)
        except BrokenPipeError:
            pass

    # Silence noisy default error handling
    def handle_one_request(self) -> None:
        try:
            super().handle_one_request()
        except Exception:
            log.exception("handle_one_request failed")


class ThreadedHTTPServer(socketserver.ThreadingMixIn, HTTPServer):
    daemon_threads = True
    allow_reuse_address = True


class UnixStreamHTTPServer(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, socket_path: str, handler: type[BaseHTTPRequestHandler]):
        p = Path(socket_path)
        p.parent.mkdir(parents=True, exist_ok=True)
        try:
            p.parent.chmod(0o700)
        except Exception:
            pass
        try:
            if p.exists():
                p.unlink()
        except Exception:
            pass
        super().__init__(socket_path, handler)
        try:
            os.chmod(socket_path, 0o600)
        except Exception:
            pass


def run_servers(
    host: str | None,
    port: int | None,
    socket_path: str | None,
    config: WorkerConfig,
    worker: EmbeddingWorker,
) -> list[socketserver.BaseServer]:
    # Bind shared state
    EmbeddingHTTPHandler.worker = worker
    EmbeddingHTTPHandler.worker_config = config
    EmbeddingHTTPHandler.start_time = time.monotonic()

    servers: list[socketserver.BaseServer] = []
    threads: list[threading.Thread] = []

    if host is not None and port is not None:
        # Validate loopback only
        if host not in ("127.0.0.1", "localhost", "::1"):
            print(
                f"FATAL: embedding worker only binds loopback (got host={host!r}); refusing to bind 0.0.0.0",
                file=sys.stderr,
            )
            raise SystemExit(5)
        srv = ThreadedHTTPServer((host, port), EmbeddingHTTPHandler)
        servers.append(srv)
        t = threading.Thread(target=srv.serve_forever, name=f"http-{host}:{port}", daemon=True)
        t.start()
        threads.append(t)
        log.info("listening on http://%s:%d (loopback)", host, port)

    if socket_path:
        srv2 = UnixStreamHTTPServer(socket_path, EmbeddingHTTPHandler)
        servers.append(srv2)
        t2 = threading.Thread(target=srv2.serve_forever, name=f"uds-{socket_path}", daemon=True)
        t2.start()
        threads.append(t2)
        log.info("listening on unix://%s", socket_path)

    if not servers:
        print("FATAL: no transport configured (need --host/--port or --socket)", file=sys.stderr)
        raise SystemExit(5)

    return servers


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        prog="omahab-embedding-worker",
        description="Omahab isolated embedding worker — loopback/UDS, bounded, no credentials",
    )
    p.add_argument("--config", help="path to pinned_models.json (or set EMBEDDING_WORKER_CONFIG)")
    p.add_argument("--host", default=None, help="loopback host (default 127.0.0.1; only loopback allowed)")
    p.add_argument("--port", type=int, default=None, help="loopback port (default 7700)")
    p.add_argument("--socket", dest="socket_path", default=None, help="Unix socket path (e.g., /run/omahab/embedding.sock)")
    p.add_argument(
        "--transport",
        choices=["http", "uds", "both"],
        default=None,
        help="transport selector (default: http if host/port given, else uds if socket given, else http)",
    )
    p.add_argument("--allow-test-adapter", action="store_true", help="allow deterministic test adapter (dev/CI only)")
    p.add_argument("--log-level", default="INFO", choices=["DEBUG", "INFO", "WARNING", "ERROR"])
    p.add_argument("--version", action="store_true", help="print version and exit")
    return p.parse_args(argv)


def main(argv: list[str] | None = None) -> None:
    args = parse_args(argv)
    if args.version:
        print(__version__)
        return

    logging.basicConfig(
        level=getattr(logging, args.log_level),
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )

    # Allow CLI flag to override env/config
    if args.allow_test_adapter:
        os.environ["EMBEDDING_WORKER_ALLOW_TEST_ADAPTER"] = "1"

    # Load config — fails fast with clear message and non-zero exit if pinned artifacts missing
    try:
        cfg = load_config(args.config)
    except SystemExit as e:
        # load_config already printed FATAL to stderr
        sys.exit(e.code if isinstance(e.code, int) else 1)

    # Effective allow_test_adapter may have been overridden, re-log
    log.info(
        "config loaded %s allow_test_adapter=%s models=%s base=%s",
        cfg.config_path,
        cfg.allow_test_adapter,
        list(cfg.models.keys()),
        cfg.models_base_dir,
    )
    # Security: affirm no provider credentials anywhere
    for forbidden_env in ("OPENAI_API_KEY", "ANTHROPIC_API_KEY", "PROVIDER_TOKEN", "HF_TOKEN"):
        if os.environ.get(forbidden_env):
            log.warning("environment contains %s but embedding worker never uses provider credentials", forbidden_env)

    # Instantiate worker (loads models; test adapter vs onnx)
    try:
        worker = EmbeddingWorker(cfg)
    except SystemExit:
        raise
    except Exception as e:
        print(f"FATAL: failed to initialize embedding models: {e}", file=sys.stderr)
        log.exception("model init failed")
        sys.exit(6)

    # Resolve transport defaults
    transport = args.transport
    host = args.host
    port = args.port
    sock = args.socket_path

    env_transport = os.environ.get("EMBEDDING_WORKER_TRANSPORT", "").strip().lower()
    if transport is None and env_transport in ("http", "uds", "both"):
        transport = env_transport

    if transport is None:
        if sock and not host:
            transport = "uds"
        elif host or port:
            transport = "http"
        else:
            # Default to http loopback
            transport = "http"

    if transport in ("http", "both"):
        if host is None:
            host = os.environ.get("EMBEDDING_WORKER_HOST", DEFAULT_HOST)
        if port is None:
            port_s = os.environ.get("EMBEDDING_WORKER_PORT", str(DEFAULT_PORT))
            try:
                port = int(port_s)
            except ValueError:
                print(f"FATAL: invalid port {port_s!r}", file=sys.stderr)
                sys.exit(5)
    else:
        host = None
        port = None

    if transport in ("uds", "both"):
        if not sock:
            sock = os.environ.get("EMBEDDING_WORKER_SOCKET", "")
            if not sock and transport == "uds":
                # default socket path for uds-only
                sock = "/tmp/omahab-embedding.sock"
            elif not sock:
                sock = None  # both but no socket env -> skip uds
        if sock == "":
            sock = None
    else:
        sock = None

    # Final validation: if http requested, ensure loopback
    if host is not None and host not in ("127.0.0.1", "localhost", "::1"):
        print(f"FATAL: host must be loopback (got {host!r})", file=sys.stderr)
        sys.exit(5)

    servers = run_servers(host, port, sock, cfg, worker)

    # Signal handling
    stop = threading.Event()

    def _handle_sig(signum, frame):  # type: ignore
        log.info("received signal %s, shutting down", signum)
        stop.set()
        for s in servers:
            try:
                s.shutdown()
            except Exception:
                pass

    signal.signal(signal.SIGTERM, _handle_sig)
    signal.signal(signal.SIGINT, _handle_sig)

    log.info("embedding worker ready (version=%s)", __version__)
    # Block until signal
    try:
        while not stop.is_set():
            time.sleep(0.5)
    except KeyboardInterrupt:
        pass
    finally:
        log.info("shutting down")
        for s in servers:
            try:
                s.shutdown()
                s.server_close()
            except Exception:
                pass
        # Cleanup socket file if we created it
        if sock:
            try:
                Path(sock).unlink(missing_ok=True)
            except Exception:
                pass
        worker.close()
        log.info("shutdown complete")


if __name__ == "__main__":
    main()
