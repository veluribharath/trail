"""Write the made-up past sessions of the fixture project as Claude Code logs.

usage: python3 history.py PROJECT_DIR CLAUDE_DIR

Two sessions solved things the repeat tasks need (h1: rounding money, h2:
adding a CLI option). The rest are the usual noise, some of it touching the
same files as the new tasks.
"""
import datetime
import json
import os
import sys

TESTS = "python -m unittest discover -s tests -t ."

SESSIONS = [
    ("h01", "Invoice tax is a cent off for amounts like 0.25 at 10%, fix it",
     ["ledger/invoice.py", "tests/test_invoice.py"], [TESTS],
     "tax() used round(float(...), 2). Floats can't hold 0.025 exactly, so it rounded down to 0.02. "
     "I switched it to money.round_money, which uses Decimal with ROUND_HALF_UP, and added test_tax_half_cent_rounds_up. "
     "Rule for this repo: never use float or round() for money; go through money.to_decimal and money.round_money."),
    ("h02", "Add a --currency option to the CLI so invoices can be printed in other currencies",
     ["ledger/options.py", "ledger/cli.py", "tests/golden/help.txt"], ["python tools/regen_golden.py", TESTS],
     "Options live in the OPTIONS table in ledger/options.py and cli.py builds the parser from it, so a new option is one entry there "
     "plus its use in main(). tests/test_help.py compares --help with tests/golden/help.txt, so after changing options run "
     "python tools/regen_golden.py, then the tests: " + TESTS + "."),
    ("h03", "Set up GitHub Actions to run the unit tests on every push",
     [".github/workflows/test.yml"], [], "The workflow runs " + TESTS + " on Python 3.11 and 3.12."),
    ("h04", "Update the README with installation instructions",
     ["README.md"], [], "The README now covers installing with pip and running the CLI."),
    ("h05", "Write a Dockerfile for the CLI and keep the image small",
     ["Dockerfile", ".dockerignore"], ["docker build -t ledger ."], "A python:3.12-slim image with only the package copied in, 61 MB."),
    ("h06", "Configure logging to write JSON lines to logs/ledger.log",
     ["ledger/logging_setup.py"], [TESTS], "logging_setup.py adds a JSON formatter and a rotating file handler; cli.py calls it at start."),
    ("h07", "Rename Line.qty to quantity everywhere",
     ["ledger/invoice.py", "ledger/cli.py", "tests/test_invoice.py"], [TESTS], "Renamed the field and its uses; tests pass."),
    ("h08", "The revenue report is slow on big exports, speed it up",
     ["ledger/reports.py"], [TESTS], "revenue() was building a list first; it sums a generator now and skips unpaid invoices early."),
    ("h09", "Add truncate(s, n) to ledger/text.py for long line item descriptions",
     ["ledger/text.py"], [TESTS], "truncate cuts to n characters and ends with an ellipsis when the text was longer."),
    ("h10", "Pin the dev dependencies and add a requirements-dev.txt",
     ["requirements-dev.txt"], [], "Pinned black, ruff and coverage."),
]


def main(project, claude_dir):
    project = os.path.realpath(project)
    proj = os.path.join(claude_dir, "projects", project.replace("/", "-"))
    os.makedirs(proj, exist_ok=True)
    start = datetime.datetime(2026, 9, 1, 9, 0, 0)
    for n, (sid, prompt, files, cmds, reply) in enumerate(SESSIONS):
        t = start + datetime.timedelta(days=n)
        lines = []

        def add(kind, content):
            stamp = (t + datetime.timedelta(minutes=len(lines))).strftime("%Y-%m-%dT%H:%M:%SZ")
            lines.append({"type": kind, "cwd": project, "sessionId": sid, "timestamp": stamp,
                          "message": {"role": kind, "content": content}})

        add("user", prompt)
        for i, f in enumerate(files):
            tid = "%s-e%d" % (sid, i)
            add("assistant", [{"type": "tool_use", "id": tid, "name": "Edit",
                               "input": {"file_path": os.path.join(project, f), "old_string": "x", "new_string": "y"}}])
            add("user", [{"type": "tool_result", "tool_use_id": tid, "content": "ok"}])
        for i, c in enumerate(cmds):
            tid = "%s-b%d" % (sid, i)
            add("assistant", [{"type": "tool_use", "id": tid, "name": "Bash", "input": {"command": c}}])
            add("user", [{"type": "tool_result", "tool_use_id": tid, "content": "OK"}])
        add("assistant", [{"type": "text", "text": reply}])
        with open(os.path.join(proj, sid + ".jsonl"), "w") as fh:
            for l in lines:
                fh.write(json.dumps(l) + "\n")


if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2])
