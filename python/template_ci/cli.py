"""Template CI: validate catalogue templates, smoke-test them with Docker Compose, publish tested versions.

Only versions whose smoke test passed get `tested_at` set, and only those are deployable through the API.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import pathlib
import subprocess
import sys
import time
from dataclasses import dataclass

import httpx
import yaml

REQUIRED = ("slug", "name", "category", "licence_spdx", "version", "health")
# Licences we will not host as a managed service without a commercial agreement.
BLOCKED_LICENCES = {"SSPL-1.0", "BUSL-1.1", "Elastic-2.0", "Commons-Clause"}


@dataclass
class Template:
    path: pathlib.Path
    manifest: dict
    compose: str

    @property
    def sha256(self) -> str:
        return hashlib.sha256(self.compose.encode()).hexdigest()


def load(path: pathlib.Path) -> Template:
    manifest = yaml.safe_load((path / "spade.yml").read_text())
    compose = (path / "docker-compose.yml").read_text()
    return Template(path, manifest, compose)


def validate(t: Template) -> list[str]:
    errs = [f"missing field: {k}" for k in REQUIRED if k not in t.manifest]
    if t.manifest.get("licence_spdx") in BLOCKED_LICENCES:
        errs.append(f"licence {t.manifest['licence_spdx']} is not hostable")
    h = t.manifest.get("health") or {}
    if not isinstance(h.get("port"), int) or not str(h.get("path", "")).startswith("/"):
        errs.append("health.port (int) and health.path (/...) are required")
    try:
        compose = yaml.safe_load(t.compose)
    except yaml.YAMLError as e:  # pragma: no cover - message passthrough
        return errs + [f"compose is not YAML: {e}"]
    services = (compose or {}).get("services") or {}
    if not services:
        errs.append("compose defines no services")
    for name, svc in services.items():
        image = str(svc.get("image", ""))
        if not image or ":" not in image or image.endswith(":latest"):
            errs.append(f"service {name}: image must be pinned to an explicit non-latest tag")
        if svc.get("privileged"):
            errs.append(f"service {name}: privileged containers are not allowed")
        for p in svc.get("ports", []) or []:
            if not str(p).startswith("127.0.0.1:"):
                errs.append(f"service {name}: port {p} must bind 127.0.0.1 (the edge proxy fronts it)")
    if t.path.name != t.manifest.get("slug"):
        errs.append("directory name must equal slug")
    return errs


def smoke_test(t: Template, timeout: float = 180.0) -> dict:
    """Bring the stack up, probe health, tear it down. Returns a report; never fakes success."""
    project = f"spadeci-{t.manifest['slug']}"
    compose_file = t.path / "docker-compose.yml"
    base = ["docker", "compose", "-p", project, "-f", str(compose_file)]
    report: dict = {"started_at": time.time(), "passed": False}
    try:
        up = subprocess.run(base + ["up", "-d", "--quiet-pull"], capture_output=True, text=True, timeout=600)
        if up.returncode != 0:
            report["error"] = up.stderr[-2000:]
            return report
        h = t.manifest["health"]
        url = f"http://127.0.0.1:{h['port']}{h['path']}"
        expect = set(h.get("expect_status", [200]))
        deadline = time.time() + timeout
        last = None
        while time.time() < deadline:
            try:
                r = httpx.get(url, timeout=3, follow_redirects=False)
                last = r.status_code
                if r.status_code in expect:
                    report.update(passed=True, status=r.status_code, url=url)
                    return report
            except httpx.HTTPError as e:
                last = str(e)
            time.sleep(2)
        report["error"] = f"health probe {url} did not return {sorted(expect)} (last: {last})"
        return report
    finally:
        subprocess.run(base + ["down", "-v", "--remove-orphans"], capture_output=True, text=True, timeout=300)
        report["finished_at"] = time.time()


def publish(t: Template, dsn: str, report: dict | None) -> None:
    import psycopg

    m = t.manifest
    tested = bool(report and report.get("passed"))
    with psycopg.connect(dsn) as conn, conn.cursor() as cur:
        cur.execute(
            """INSERT INTO template(slug,name,category,licence_spdx,description) VALUES (%s,%s,%s,%s,%s)
               ON CONFLICT (slug) DO UPDATE SET name=EXCLUDED.name, category=EXCLUDED.category,
                 licence_spdx=EXCLUDED.licence_spdx, description=EXCLUDED.description RETURNING id""",
            (m["slug"], m["name"], m["category"], m["licence_spdx"], m.get("description", "")),
        )
        tid = cur.fetchone()[0]
        cur.execute(
            """INSERT INTO template_version(template_id,version,manifest,compose,compose_sha256,min_vcpu,min_ram_mb,tested_at,test_report)
               VALUES (%s,%s,%s,%s,%s,%s,%s, CASE WHEN %s THEN now() END, %s)
               ON CONFLICT (template_id,version) DO UPDATE SET manifest=EXCLUDED.manifest, compose=EXCLUDED.compose,
                 compose_sha256=EXCLUDED.compose_sha256, tested_at=EXCLUDED.tested_at, test_report=EXCLUDED.test_report""",
            (tid, str(m["version"]), json.dumps(m), t.compose, t.sha256, m.get("min_vcpu", 1), m.get("min_ram_mb", 1024),
             tested, json.dumps(report or {"skipped": True})),
        )


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(prog="spade-template-ci")
    ap.add_argument("templates", nargs="*", default=None, help="template dirs (default: all under templates/)")
    ap.add_argument("--root", default=os.environ.get("SPADE_TEMPLATES", "templates"))
    ap.add_argument("--smoke", action="store_true", help="run docker compose smoke test")
    ap.add_argument("--publish", action="store_true", help="write to Postgres (DATABASE_URL)")
    args = ap.parse_args(argv)
    root = pathlib.Path(args.root)
    dirs = [pathlib.Path(d) for d in args.templates] if args.templates else sorted(p for p in root.iterdir() if p.is_dir())
    failed = 0
    for d in dirs:
        t = load(d)
        errs = validate(t)
        report = None
        if not errs and args.smoke:
            report = smoke_test(t)
            if not report["passed"]:
                errs.append(report.get("error", "smoke test failed"))
        print(json.dumps({"template": d.name, "ok": not errs, "errors": errs, "smoke": report}))
        if args.publish and not validate(t):
            dsn = os.environ.get("DATABASE_URL", "postgresql://spade:spade-local-dev@localhost:5433/spadeai")
            publish(t, dsn, report)
        failed += bool(errs)
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
