#!/usr/bin/env python3
"""Secret-free ISO harness evidence. Serial logs and private state are excluded."""

import hashlib
import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
from datetime import datetime, timezone


def now():
    return datetime.now(timezone.utc).isoformat()


def digest(path):
    value = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            value.update(chunk)
    return value.hexdigest()


def source_identity():
    commit = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
    files = subprocess.check_output(["git", "ls-files", "-z"]).split(b"\0")
    value = hashlib.sha256()
    for name in sorted(filter(None, files)):
        path = Path(os.fsdecode(name))
        value.update(name + b"\0")
        if path.is_symlink():
            value.update(b"link\0" + os.fsencode(os.readlink(path)))
        elif path.is_file():
            value.update(str(path.stat().st_mode & 0o777).encode() + b"\0")
            value.update(bytes.fromhex(digest(path)))
        else:
            value.update(b"missing")
    return {"commit": commit, "source_sha256": value.hexdigest()}


def host_memory():
    fields = {}
    for line in Path("/proc/meminfo").read_text().splitlines():
        name, rest = line.split(":", 1)
        if name in ("MemTotal", "MemAvailable", "SwapTotal", "SwapFree"):
            fields[name + "_kib"] = int(rest.split()[0])
    return fields


def save(path, value):
    temporary = path.with_suffix(".tmp")
    temporary.write_text(json.dumps(value, indent=2) + "\n")
    temporary.replace(path)


def main():
    command = sys.argv[1]
    if command == "identity":
        identity = source_identity()
        print(identity["commit"] + ":" + identity["source_sha256"])
        return
    workdir = Path(sys.argv[2])
    evidence = workdir / "evidence"
    report_path = evidence / "report.json"
    if command == "init":
        evidence.mkdir(parents=True, exist_ok=True)
        # Refuse stale evidence rather than silently mix independent campaigns.
        if report_path.exists():
            raise SystemExit("evidence already exists; use a fresh --workdir")
        settings = json.loads(sys.argv[3])
        save(report_path, {
            "schema_version": 1, "started_at": now(), "status": "RUNNING",
            "source": source_identity(), "settings": settings,
            "repeat_command": sys.argv[4], "host_memory_start": host_memory(),
            "checks": [], "memory": [],
            "limits": ["External-provider and full application workloads require a separate campaign."],
        })
        return
    report = json.loads(report_path.read_text())
    if command == "iso":
        iso = Path(sys.argv[3]).resolve()
        report["iso"] = {"store_path": str(iso), "sha256": digest(iso)}
    elif command == "check":
        report["checks"].append({"name": sys.argv[3], "status": "PASS", "at": now()})
    elif command == "generation":
        report.setdefault("installed_generations", []).append(sys.argv[3])
    elif command == "memory":
        guest = {}
        for line in sys.stdin:
            name, value = line.strip().split("=", 1)
            guest[name] = int(value)
        report["memory"].append({"stage": sys.argv[3], "at": now(),
                                 "host": host_memory(), "guest_kib": guest})
    elif command == "finish":
        code = int(sys.argv[3])
        report.update(status="PASS" if code == 0 else "FAIL", exit_code=code,
                      finished_at=now(), host_memory_end=host_memory())
        if sys.argv[4] == "interactive" and code == 0:
            report["status"] = "INTERACTIVE_ENDED"
        report["source_end"] = source_identity()
        report["source_changed_during_run"] = report["source_end"] != report["source"]
        if report["source_changed_during_run"]:
            report.update(status="FAIL", exit_code=1)
        report["last_stage"] = sys.argv[5]
        save(report_path, report)
        files = sorted(path for path in evidence.rglob("*") if path.is_file()
                       and path.name != "SHA256SUMS")
        (evidence / "SHA256SUMS").write_text("".join(
            f"{digest(path)}  {path.relative_to(evidence)}\n" for path in files))
        return 1 if report["source_changed_during_run"] else 0
    else:
        raise SystemExit(f"unknown command: {shlex.quote(command)}")
    save(report_path, report)


if __name__ == "__main__":
    raise SystemExit(main())
