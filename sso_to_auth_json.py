"""Compatibility shim — implementation lives in scripts/sso_to_auth_json.py."""
from __future__ import annotations

import importlib.util
import sys
from pathlib import Path

_path = Path(__file__).resolve().parent / "scripts" / "sso_to_auth_json.py"
_spec = importlib.util.spec_from_file_location("sso_to_auth_json", _path)
if _spec is None or _spec.loader is None:
    raise ImportError(f"cannot load sso_to_auth_json from {_path}")
_mod = importlib.util.module_from_spec(_spec)
sys.modules[__name__] = _mod
sys.modules["sso_to_auth_json"] = _mod
_spec.loader.exec_module(_mod)
