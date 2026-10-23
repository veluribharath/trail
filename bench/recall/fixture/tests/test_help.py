import os
import unittest

from ledger.cli import build_parser

GOLDEN = os.path.join(os.path.dirname(__file__), "golden", "help.txt")


class HelpTest(unittest.TestCase):
    def test_help_matches_golden(self):
        with open(GOLDEN) as f:
            want = f.read()
        self.assertEqual(build_parser().format_help(), want, "run python tools/regen_golden.py after changing options")
