# New task: nothing in the history is about it.
import datetime
import unittest

from ledger.reports import count_overdue


class Overdue(unittest.TestCase):
    def test_count(self):
        today = datetime.date(2026, 3, 1)
        invs = [
            {"due": datetime.date(2026, 2, 1), "paid": False},
            {"due": datetime.date(2026, 2, 1), "paid": True},
            {"due": datetime.date(2026, 3, 1), "paid": False},
            {"due": datetime.date(2026, 4, 1), "paid": False},
            {"due": datetime.date(2025, 12, 31), "paid": False},
        ]
        self.assertEqual(count_overdue(invs, today), 2)
        self.assertEqual(count_overdue([], today), 0)
