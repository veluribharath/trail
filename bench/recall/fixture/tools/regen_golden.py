"""Rewrite tests/golden/help.txt from the current parser. Run after changing ledger/options.py."""
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))
from ledger.cli import build_parser  # noqa: E402

path = os.path.join(os.path.dirname(__file__), "..", "tests", "golden", "help.txt")
with open(path, "w") as f:
    f.write(build_parser().format_help())
print("wrote", os.path.normpath(path))
