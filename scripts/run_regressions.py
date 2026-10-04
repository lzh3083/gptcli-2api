#!/usr/bin/env python3
"""Compatibility / contract checks after Python public main removal.

Legacy scripts/_test_*.py regressions were deleted with the Python public API.
This runner keeps CI green by validating the remaining contract artifacts and
version pin files that release automation still depends on.
"""

from __future__ import annotations

import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parents[1]


def _fail(msg: str) -> int:
    print(f"FAIL: {msg}", file=sys.stderr)
    return 1


def main() -> int:
    if sys.flags.optimize:
        print("regressions require assertions; do not use python -O", file=sys.stderr)
        return 2

    # Contract schema
    try:
        import jsonschema  # type: ignore
    except ImportError:
        return _fail("jsonschema not installed (pip install jsonschema)")

    root = ROOT / "contracts"
    schema = json.loads((root / "manifest.schema.json").read_text(encoding="utf-8"))
    manifest = json.loads((root / "manifest.json").read_text(encoding="utf-8"))
    jsonschema.validate(manifest, schema)
    print("ok contracts/manifest.json schema")

    env_manifest = json.loads((root / "env-manifest.json").read_text(encoding="utf-8"))
    runtime = next(
        (e for e in env_manifest.get("env", env_manifest.get("variables", [])) if e.get("name") == "GROK2API_RUNTIME"),
        None,
    )
    # Support both top-level shapes used historically.
    if runtime is None and isinstance(env_manifest.get("variables"), list):
        runtime = next((e for e in env_manifest["variables"] if e.get("name") == "GROK2API_RUNTIME"), None)
    if runtime is None and isinstance(env_manifest.get("env"), list):
        runtime = next((e for e in env_manifest["env"] if e.get("name") == "GROK2API_RUNTIME"), None)
    # Fall back: scan any list of dicts under common keys.
    if runtime is None:
        for key, val in env_manifest.items():
            if isinstance(val, list):
                for item in val:
                    if isinstance(item, dict) and item.get("name") == "GROK2API_RUNTIME":
                        runtime = item
                        break
            if runtime is not None:
                break
    if runtime is None:
        return _fail("GROK2API_RUNTIME missing from env-manifest.json")
    values = runtime.get("values") or []
    if values != ["go"]:
        return _fail(f"GROK2API_RUNTIME values must be ['go'], got {values!r}")
    print("ok GROK2API_RUNTIME is go-only")

    # Version pins: grok2api/app.py APP_VERSION == internal/buildinfo.Version
    py = (ROOT / "grok2api" / "app.py").read_text(encoding="utf-8")
    go = (ROOT / "internal" / "buildinfo" / "buildinfo.go").read_text(encoding="utf-8")
    pv = re.search(r'APP_VERSION\s*=\s*"([^"]+)"', py)
    gv = re.search(r'Version\s*=\s*"([^"]+)"', go)
    if not pv or not gv:
        return _fail("could not parse APP_VERSION / Version")
    if pv.group(1) != gv.group(1):
        return _fail(f"version mismatch python={pv.group(1)} go={gv.group(1)}")
    print(f"ok version pin {pv.group(1)}")

    # Sidecar modules still importable (no full FastAPI public app required)
    sys.path.insert(0, str(ROOT))
    try:
        from grok2api.admin import sso_import  # noqa: F401
        from grok2api.upstream import grok_build_adapter  # noqa: F401
        import scripts.registration_service as regsvc  # noqa: F401
    except Exception as exc:  # pragma: no cover
        return _fail(f"sidecar import failed: {exc}")
    print("ok sidecar imports (sso_import, grok_build_adapter, registration_service)")

    print("\nall contract regressions passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
