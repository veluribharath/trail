from decimal import ROUND_HALF_UP, Decimal

CENT = Decimal("0.01")


def to_decimal(x):
    """Turn a str, int or Decimal into a Decimal. Floats are refused."""
    if isinstance(x, float):
        raise TypeError("money must not be a float: %r" % x)
    return Decimal(str(x))


def round_money(x):
    """Round to the cent, halves away from zero, the way invoices are printed."""
    return to_decimal(x).quantize(CENT, rounding=ROUND_HALF_UP)


def fmt(amount, currency="EUR"):
    return "%s %s" % (round_money(amount), currency)
