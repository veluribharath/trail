from dataclasses import dataclass, field
from decimal import Decimal

from .money import round_money, to_decimal


@dataclass
class Line:
    description: str
    quantity: int
    unit_price: str

    def amount(self):
        return round_money(to_decimal(self.unit_price) * self.quantity)


@dataclass
class Invoice:
    lines: list = field(default_factory=list)
    tax_rate: str = "0"  # percent
    discount_pct: str = "0"  # percent, taken off the subtotal before tax

    def subtotal(self):
        return round_money(sum((l.amount() for l in self.lines), Decimal(0)))

    def discount(self):
        return Decimal(str(round(float(self.subtotal()) * float(self.discount_pct) / 100, 2)))

    def tax(self):
        return round_money((self.subtotal() - self.discount()) * to_decimal(self.tax_rate) / 100)

    def total(self):
        return self.subtotal() - self.discount() + self.tax()
