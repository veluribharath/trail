# Repeat task: a new CLI option. The earlier session added --currency and
# learned the options table and golden help file convention.
import io
import unittest

from ledger.cli import main


class DueDays(unittest.TestCase):
    def run_cli(self, *args):
        out = io.StringIO()
        main(["--item", "Widget:1:10.00", "--date", "2026-01-01", *args], out=out)
        return out.getvalue()

    def test_due_days(self):
        self.assertIn("Due: 2026-01-11", self.run_cli("--due-days", "10"))

    def test_default(self):
        self.assertIn("Due: 2026-01-31", self.run_cli())
