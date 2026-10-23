# Every command-line option, in the order --help lists them. cli.py builds
# its parser from this table, and tests/golden/help.txt holds the --help
# output it must match.
OPTIONS = [
    {"flags": ["--item"], "action": "append", "default": [], "metavar": "DESC:QTY:PRICE", "help": "a line item; repeat for more"},
    {"flags": ["--tax"], "default": "0", "metavar": "PCT", "help": "tax rate in percent"},
    {"flags": ["--discount"], "default": "0", "metavar": "PCT", "help": "discount in percent, before tax"},
    {"flags": ["--currency"], "default": "EUR", "help": "currency code to print"},
    {"flags": ["--date"], "default": None, "metavar": "YYYY-MM-DD", "help": "invoice date (default: today)"},
]
