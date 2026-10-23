import argparse
import datetime
import sys

from .invoice import Invoice, Line
from .money import fmt
from .options import OPTIONS


def build_parser():
    p = argparse.ArgumentParser(
        prog="ledger",
        description="Print an invoice.",
        formatter_class=lambda prog: argparse.HelpFormatter(prog, width=80),
    )
    for o in OPTIONS:
        o = dict(o)
        p.add_argument(*o.pop("flags"), **o)
    return p


def parse_item(s):
    desc, qty, price = s.rsplit(":", 2)
    return Line(desc, int(qty), price)


def main(argv=None, out=sys.stdout):
    args = build_parser().parse_args(argv)
    date = datetime.date.fromisoformat(args.date) if args.date else datetime.date.today()
    inv = Invoice([parse_item(i) for i in args.item], tax_rate=args.tax, discount_pct=args.discount)
    print("Invoice date: %s" % date.isoformat(), file=out)
    for l in inv.lines:
        print("  %-30s %3d x %s" % (l.description, l.quantity, fmt(l.unit_price, args.currency)), file=out)
    print("Subtotal: %s" % fmt(inv.subtotal(), args.currency), file=out)
    if inv.discount():
        print("Discount: -%s" % fmt(inv.discount(), args.currency), file=out)
    print("Tax: %s" % fmt(inv.tax(), args.currency), file=out)
    print("Total: %s" % fmt(inv.total(), args.currency), file=out)
    return 0


if __name__ == "__main__":
    sys.exit(main())
