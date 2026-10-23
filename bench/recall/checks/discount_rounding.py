# Repeat task: a cent off on discounts. The earlier session fixed the same
# bug in tax: float round() instead of money.round_money.
import unittest
from decimal import Decimal

from ledger.invoice import Invoice, Line


class DiscountRounding(unittest.TestCase):
    def test_half_cent(self):
        self.assertEqual(Invoice([Line("x", 1, "10.25")], discount_pct="10").discount(), Decimal("1.03"))
        self.assertEqual(Invoice([Line("x", 1, "0.05")], discount_pct="50").discount(), Decimal("0.03"))

    def test_plain(self):
        self.assertEqual(Invoice([Line("x", 1, "100.00")], discount_pct="10").discount(), Decimal("10.00"))
        self.assertEqual(Invoice([Line("x", 3, "19.99")], discount_pct="15").discount(), Decimal("9.00"))
