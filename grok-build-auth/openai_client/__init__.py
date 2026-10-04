# -*- coding: utf-8 -*-
import sys
from pathlib import Path

_PKG_ROOT = str(Path(__file__).resolve().parent)
if _PKG_ROOT not in sys.path:
    sys.path.insert(0, _PKG_ROOT)
