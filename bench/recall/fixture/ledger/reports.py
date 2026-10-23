from decimal import Decimal


def revenue(invoices):
    """Sum of the totals of paid invoices."""
    return sum((i["total"] for i in invoices if i.get("paid")), Decimal(0))
