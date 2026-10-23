# Benchmarking trail recall

Two checks for the recall hook (`trail recall`): an offline one that needs no agent, and an A/B run of real agents with the hook off and on. The question is whether adding past-session notes to a prompt helps, and above all whether it makes anything worse.

## Offline relevance check

`go test -run TestRecallRelevance -v` scores labelled prompts against a made-up project history of twenty sessions (`recall_test.go`). 16 prompts are repeats, where an earlier session worked on the same thing in other words. 21 are new work, nine of them written in the project's own vocabulary, which is where false hits come from. A repeat counts as found when the hook would add the right session.

```
cutoff  repeats found  wrong session  new tasks injected
0.25    12/16          0              3/21
0.30    11/16          0              2/21
0.35    10/16          0              0/21
0.40    10/16          0              0/21   <- default
0.45    10/16          0              0/21
0.50     4/16          0              0/21
```

The closest new-work prompt scores 0.32, so 0.4 leaves some margin. Short prompts that name the problem ("database is locked when running the tests") score 0.45 to 0.7. Long prompts written like a task description score 0.15 to 0.3 against the right session even when they're about the same thing, because most of their words aren't in it. At the default they get nothing. Matching is by words, not meaning.

To check your own history instead, `trail recall -eval .` replays every prompt in a project against the sessions before it.

## A/B with headless agents

`run.sh` copies `fixture/` (a small Python invoicing project) for every run, writes it a made-up history with `history.py`, and runs one task with `claude -p`. Then it drops a hidden test from `checks/` into the copy and runs the whole suite. A run passes when every test passes.

- Repeat tasks: `due-days` adds a CLI option, where an earlier session learned the options table and golden help file convention. `discount-rounding` fixes a cent-off discount, where an earlier session fixed the same float rounding bug in tax.
- New tasks: `slugify` and `overdue` add small functions. Nothing in the history is about them, but sessions touching the same files are there.
- Arms: `off` has no hook. `on` is the hook as shipped. `loose` is the hook with `-min-score 0.15`, so it fires on most prompts. That shows what injecting does when it happens.

```
bench/recall/run.sh                         # MODEL=sonnet RUNS=3 PAR=4 ARMS="off on loose"
python3 bench/recall/summarize.py out/<run>/results.jsonl
```

Each run is a real `claude -p` session, so it costs money and appears in your own Claude Code history (and in trail). The hook reads only the run's made-up history, through `-claude-dir`.

### Results, 2026-10-23, Claude Code 2.1.309

Sonnet, 3 runs per cell:

| task | kind | hook | pass | turns | cost $ | input tok | output tok | wall s | injected | hook ms |
|---|---|---|---|---|---|---|---|---|---|---|
| discount-rounding | repeat | off | 3/3 | 4.7 | 0.056 | 168208 | 1092 | 18 | - | - |
| discount-rounding | repeat | on | 3/3 | 5.3 | 0.060 | 181397 | 1184 | 19 | 0/3 | 31 |
| discount-rounding | repeat | loose | 3/3 | 4.3 | 0.056 | 159667 | 921 | 21 | 3/3 | 27 |
| due-days | repeat | off | 3/3 | 4.0 | 0.064 | 148056 | 1124 | 26 | - | - |
| due-days | repeat | on | 3/3 | 4.0 | 0.064 | 148177 | 1253 | 19 | 0/3 | 30 |
| due-days | repeat | loose | 3/3 | 3.0 | 0.050 | 109519 | 828 | 18 | 3/3 | 33 |
| overdue | new | off | 3/3 | 4.0 | 0.052 | 143647 | 1002 | 15 | - | - |
| overdue | new | on | 3/3 | 4.0 | 0.056 | 144269 | 1225 | 22 | 0/3 | 18 |
| overdue | new | loose | 3/3 | 4.3 | 0.060 | 157862 | 1342 | 22 | 0/3 | 16 |
| slugify | new | off | 3/3 | 4.3 | 0.054 | 155457 | 1122 | 21 | - | - |
| slugify | new | on | 3/3 | 4.0 | 0.049 | 142769 | 917 | 17 | 0/3 | 16 |
| slugify | new | loose | 3/3 | 4.7 | 0.051 | 143862 | 988 | 20 | 3/3 | 20 |

| kind | hook | pass | turns | cost $ | input tok | wall s | injected |
|---|---|---|---|---|---|---|---|
| repeat | off | 6/6 | 4.3 | 0.060 | 158132 | 22 | - |
| repeat | on | 6/6 | 4.7 | 0.062 | 164787 | 19 | 0/6 |
| repeat | loose | 6/6 | 3.7 | 0.053 | 134593 | 19 | 6/6 |
| new | off | 6/6 | 4.2 | 0.053 | 149552 | 18 | - |
| new | on | 6/6 | 4.0 | 0.052 | 143519 | 20 | 0/6 |
| new | loose | 6/6 | 4.5 | 0.056 | 150862 | 21 | 3/6 |


Haiku, off and loose only (as shipped, the hook added nothing for any of these prompts, so `on` is the same as `off`):

| task | kind | hook | pass | turns | cost $ | input tok | output tok | wall s | injected | hook ms |
|---|---|---|---|---|---|---|---|---|---|---|
| discount-rounding | repeat | off | 3/3 | 7.3 | 0.005 | 212942 | 2277 | 18 | - | - |
| discount-rounding | repeat | loose | 3/3 | 7.0 | 0.005 | 205721 | 2487 | 21 | 3/3 | 21 |
| due-days | repeat | off | 3/3 | 9.3 | 0.007 | 291161 | 3037 | 24 | - | - |
| due-days | repeat | loose | 3/3 | 9.0 | 0.006 | 239558 | 2823 | 21 | 3/3 | 20 |
| overdue | new | off | 3/3 | 7.3 | 0.006 | 251147 | 2354 | 19 | - | - |
| overdue | new | loose | 3/3 | 6.0 | 0.005 | 211561 | 2140 | 18 | 0/3 | 17 |
| slugify | new | off | 3/3 | 6.3 | 0.005 | 212466 | 2339 | 18 | - | - |
| slugify | new | loose | 3/3 | 5.7 | 0.005 | 199471 | 2293 | 18 | 3/3 | 20 |

| kind | hook | pass | turns | cost $ | input tok | wall s | injected |
|---|---|---|---|---|---|---|---|
| repeat | off | 6/6 | 8.3 | 0.006 | 252052 | 21 | - |
| repeat | loose | 6/6 | 8.0 | 0.006 | 222640 | 21 | 6/6 |
| new | off | 6/6 | 6.8 | 0.005 | 231806 | 19 | - |
| new | loose | 6/6 | 5.8 | 0.005 | 205516 | 18 | 3/6 |


### What it says

- No regressions. All 60 runs passed, in every arm.
- As shipped, the hook added nothing to any of the four task prompts. Over 12 runs it changed nothing but latency: 13 to 45 ms per prompt on a cold start with no cache.
- When it does fire (`loose`), repeat tasks used a little less: with Sonnet, 3.7 turns against 4.3 and `$0.053` against `$0.060`. With Haiku they used 12% fewer input tokens. On new tasks, where it added loosely related notes in half the runs, Sonnet took 4.5 turns against 4.2.
- These are small samples on easy tasks. Run-to-run noise is about one turn: Haiku's `overdue` runs got nothing injected and still took 1.3 fewer turns than `off`. None of the differences above is clearly bigger than that. These numbers say the hook doesn't hurt, not that it helps.
- To show a benefit, it would need harder repeat tasks, where the past session holds something the agent can't find in the repo, and more runs.
