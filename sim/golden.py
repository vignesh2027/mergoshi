"""Writes sim/golden.json: a fixed sequence of merges, links, feasts and escrow
burns with the totals the Python rules produce. The contract's golden test
(contract/src/golden.rs) replays the same events through real commit and reveal
rounds and must reach exactly the same numbers.

    python3 sim/golden.py           # rewrite the file
    python3 sim/golden.py --check   # fail if the file is out of date
"""

from __future__ import annotations

import json
import random
import sys
from pathlib import Path

from mechanic import DAY, E18, Ledger, Params

HERE = Path(__file__).resolve().parent
OUT = HERE / "golden.json"
T0 = 1_793_000_000
REPOS = ["prometheus/client_golang", "helm/chart-testing"]

PARAMS = Params(
    base_reward=100 * E18,
    halving_every=15,
    max_paid_reviewers=3,
    repo_daily_cap=4,
    repo_floor=1,
    authors_per_slot=2,
    author_daily_cap=2,
    min_github_age_days=90,
    max_merge_age_secs=7 * DAY,
    feast_period_secs=30 * DAY,
    feast_burn_bps=5000,
    feast_max_guests=3,
    escrow_ttl_secs=20 * DAY,
    quorum=2,
)


def build():
    rng = random.Random(2026)
    people = list(range(1, 13))
    created = {g: (T0 - 400 * DAY if g != 12 else T0 - 20 * DAY) for g in people}
    numbers = {r: 0 for r in REPOS}
    raw = []
    for day in range(70):
        base = T0 + day * DAY
        for g in {3: [1, 2], 15: [5], 33: [7, 9]}.get(day, []):
            raw.append({"kind": "link", "now": base, "gid": g})
        for _ in range(rng.choice([0, 1, 2, 3, 5])):
            repo = rng.choice(REPOS)
            numbers[repo] += 1
            author = rng.choice(people)
            k = rng.choice([0, 1, 1, 2, 4])
            approvers = sorted(rng.sample([g for g in people if g != author], k))
            merged_at = base + rng.randrange(0, 20 * 3600)
            late = rng.random() < 0.08
            raw.append({
                "kind": "merge", "now": merged_at + (9 * DAY if late else 900), "repo": repo,
                "number": numbers[repo], "merged_at": merged_at, "author": author,
                "author_created_at": created[author], "approvers": approvers,
            })
        if day % 10 == 9:
            raw.append({"kind": "eat", "now": base + DAY - 1})
    for n in (1, 2):
        raw.append({"kind": "feast", "now": T0 + n * PARAMS.feast_period_secs})
    raw.sort(key=lambda e: e["now"])

    led = Ledger(PARAMS)
    led.last_feast_at = T0
    for r in REPOS:
        led.repo_enrolled[r] = (T0, True)
    for e in raw:
        if e["kind"] == "link":
            led.link(e["gid"], e["now"])
        elif e["kind"] == "merge":
            e["status"] = led.settle_merge(e["repo"], e["merged_at"], e["author"], e["approvers"], ["a1", "a2"], e["now"], e["author_created_at"])
        elif e["kind"] == "eat":
            e["ids"] = sorted(led.escrow)
            led.eat_stale_escrow(e["now"])
        elif e["kind"] == "feast":
            led.feast(e["now"])
    expect = {
        "supply": str(led.supply),
        "minted": str(led.minted),
        "burned": str(led.burned),
        "withheld": str(led.withheld),
        "pot": str(led.pot),
        "merges_minted": led.merges_minted,
        "merges_capped": led.merges_capped,
        "merges_stale": led.merges_stale,
        "merges_young": led.merges_young,
        "escrow": {str(g): str(e[0]) for g, e in sorted(led.escrow.items())},
        "linked_balances": {str(g[1]): str(v) for g, v in sorted(led.balances.items(), key=str) if g[0] == "gh"},
        "attester_balance": str(led.balances.get(("attester", "a1"), 0)),
    }
    params = {k: (str(v) if isinstance(v, int) and v >= 2**53 else v) for k, v in PARAMS.__dict__.items()}
    return {"t0": T0, "repos": REPOS, "params": params, "events": raw, "expect": expect}


def main():
    text = json.dumps(build(), indent=1) + "\n"
    if "--check" in sys.argv:
        if OUT.read_text() != text:
            sys.exit("sim/golden.json is out of date: run python3 sim/golden.py")
        print("golden.json is current")
        return
    OUT.write_text(text)
    print("wrote", OUT)


if __name__ == "__main__":
    main()
