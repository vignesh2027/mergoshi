"""Runs every scenario and the parameter sweeps, writes charts and results.

    python3 sim/run.py            # full run, about a minute
    python3 sim/run.py --quick    # one year, used by the tests
"""

from __future__ import annotations

import argparse
import json
from pathlib import Path

import svg
from mechanic import E18, Params
from model import SCENARIOS, Scenario, load_snapshot, run

HERE = Path(__file__).resolve().parent
OUT = HERE / "out"

# The launch parameters chosen from the sweeps below (see DESIGN.md).
CHOSEN = Params()


def tokens(xs):
    return [x / E18 for x in xs]


def sweep_caps(data, days):
    """Honest coverage and farmer share for a range of authors_per_slot values."""
    rows = []
    for per in [1, 2, 3, 4, 6, 8]:
        p = CHOSEN.replace(authors_per_slot=per)
        steady = run(Scenario("steady", "", days=days), p, data)
        farm = run(Scenario("farm", "", days=days, attack_from=30), p, data)
        rows.append({
            "authors_per_slot": per,
            "honest_coverage": round(steady.coverage(), 4),
            "attacker_share": round(farm.attacker[-1] / max(farm.supply[-1], 1), 4),
        })
    return rows


def sweep_author_caps(data, days):
    rows = []
    for cap in [1, 2, 3, 4, 6, 1000]:
        p = CHOSEN.replace(author_daily_cap=cap)
        steady = run(Scenario("steady", "", days=days), p, data)
        rows.append({"author_daily_cap": cap, "honest_coverage": round(steady.coverage(), 4)})
    return rows


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--quick", action="store_true", help="one simulated year and no sweeps")
    ap.add_argument("--out", default=str(OUT))
    args = ap.parse_args()
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    days = 365 if args.quick else 730
    data = load_snapshot()

    results = {}
    for name, sc in SCENARIOS.items():
        sc = Scenario(**{**sc.__dict__, "days": days})
        results[name] = run(sc, CHOSEN, data)
    no_caps = run(Scenario(**{**SCENARIOS["farm"].__dict__, "days": days}), CHOSEN.replace(repo_daily_cap=10**6, author_daily_cap=10**6, repo_floor=10**6, exile_min=10**40), data)

    summary = {
        "params": {k: (str(v) if isinstance(v, int) and v >= 10**15 else v) for k, v in CHOSEN.__dict__.items()},
        "days": days,
        "scenarios": {k: v.summary() for k, v in results.items()},
        "farm_without_caps": no_caps.summary(),
    }

    (out / "supply_by_scenario.svg").write_text(svg.line_chart(
        [(SCENARIOS[k].name.replace("_", ", "), tokens(v.supply)) for k, v in results.items()],
        "MERGED supply over two years, five scenarios",
        "Real merge rates from 10 repositories, chosen launch parameters, one seed",
        ylabel="tokens in circulation",
    ))
    st = results["steady"]
    (out / "flows_steady.svg").write_text(svg.line_chart(
        [("minted", tokens(st.minted)), ("supply", tokens(st.supply)), ("burned", tokens(st.burned)),
         ("in escrow", tokens(st.escrow)), ("withheld", tokens(st.withheld))],
        "Where the coins go in the steady scenario",
        "Cumulative minted and burned, escrow waiting for contributors to link, review share never minted",
        ylabel="tokens",
    ))
    farm = results["farm"]
    ex = results["farm_exiled"]
    (out / "farm_attacker_share.svg").write_text(svg.line_chart(
        [("no caps", [a / max(s, 1) for a, s in zip(no_caps.attacker, no_caps.supply)]),
         ("caps only", [a / max(s, 1) for a, s in zip(farm.attacker, farm.supply)]),
         ("caps and exile", [a / max(s, 1) for a, s in zip(ex.attacker, ex.supply)])],
        "Share of supply held by a repository farmer",
        "Six aged, linked accounts and an eligible repository from day 30; exiled farms come back 60 days later",
        ylabel="attacker share of supply", ymax=1.0, percent=True,
    ))
    dr = results["drought"]
    (out / "drought_flows.svg").write_text(svg.line_chart(
        [("supply", tokens(dr.supply)), ("burned", tokens(dr.burned)), ("in escrow", tokens(dr.escrow))],
        "Drought: activity falls to 15 percent on day 180",
        "Unclaimed escrow is eaten by the feast and half of every feast burns",
        ylabel="tokens",
    ))

    if not args.quick:
        caps = sweep_caps(data, 365)
        summary["sweep_repo_cap"] = caps
        summary["sweep_author_cap"] = sweep_author_caps(data, 365)
        (out / "cap_sweep.svg").write_text(svg.line_chart(
            [("honest minted", [r["honest_coverage"] for r in caps]),
             ("farmer share", [r["attacker_share"] for r in caps])],
            "Choosing how many distinct authors open one daily slot",
            "One simulated year per point, steady and farm scenarios, no exile",
            xlabel="distinct authors per extra daily slot", xs=[r["authors_per_slot"] for r in caps],
            ymax=1.0, percent=True,
        ))

    (out / "results.json").write_text(json.dumps(summary, indent=1) + "\n")
    for k, v in summary["scenarios"].items():
        print(k, v)
    print("farm without caps", summary["farm_without_caps"])
    for r in summary.get("sweep_repo_cap", []):
        print(r)
    for r in summary.get("sweep_author_cap", []):
        print(r)


if __name__ == "__main__":
    main()
