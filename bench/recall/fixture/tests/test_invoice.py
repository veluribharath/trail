import io
import unittest
from decimal import Decimal

from ledger.cli import main
from ledger.invoice import Invoice, Line


class InvoiceTest(unittest.TestCase):
    def test_totals(self):
        inv = Invoice([Line("a", 2, "10.00"), Line("b", 1, "5.50")], tax_rate="20")
        self.assertEqual(inv.subtotal(), Decimal("25.50"))
        self.assertEqual(inv.tax(), Decimal("5.10"))
        self.assertEqual(inv.total(), Decimal("30.60"))

    def test_tax_half_cent_rounds_up(self):
        inv = Invoice([Line("a", 1, "0.25")], tax_rate="10")
        self.assertEqual(inv.tax(), Decimal("0.03"))

    def test_discount(self):
        inv = Invoice([Line("a", 1, "100.00")], discount_pct="10")
        self.assertEqual(inv.discount(), Decimal("10.00"))

    def test_cli(self):
        out = io.StringIO()
        main(["--item", "Widget:2:3.50", "--date", "2026-01-01"], out=out)
        self.assertIn("Invoice date: 2026-01-01", out.getvalue())
        self.assertIn("Total: 7.00 EUR", out.getvalue())


if __name__ == "__main__":
    unittest.main()
