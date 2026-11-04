"""A day-by-day model of a community using Merged, driven by real merge history.

Each enrolled repository draws its daily human merges from the same weekday in
the public 90-day snapshot (sim/data/snapshot.json), authors are drawn from that
repository's real author mix, approvals from the sampled review counts. The
ledger applies the contract's rules from mechanic.py to every merge.
"""

from __future__ import annotations

import json
import random
from dataclasses import dataclass, field
from pathlib import Path

from mechanic import DAY, E18, Ledger, Params

HERE = Path(__file__).resolve().parent
SNAPSHOT = HERE / "data" / "snapshot.json"
T0 = 1_793_836_800  # 2026-11-05 00:00 UTC, launch day
ATTESTERS = ["attester-1", "attester-2", "attester-3"]


@dataclass
class Scenario:
    name: str
    title: str
    days: int = 730
    activity: callable = lambda d: 1.0
    newcomer_rate: callable = lambda d: 0.06
    link_rate: float = 0.35
    tourist_link_rate: float = 0.10
    enroll_every: int = 14
    max_repos: int = 30
    attack_from: int | None = None
    attack_repos: int = 1
    attack_puppets: int = 6
    attack_tries: int = 300
    exile_after: int | None = None  # days after the attack starts that holders exile the farm
    reenroll_after: int = 60  # days the attacker needs to find and enroll another eligible repository


SCENARIOS = {
    "steady": Scenario("steady", "Steady growth: real merge rates, a new repository every two weeks up to 30"),
    "hype": Scenario(
        "hype",
        "Hackathon season: three times the merges for three months, mostly newcomers",
        activity=lambda d: 3.0 if 90 <= d < 180 else 1.0,
        newcomer_rate=lambda d: 0.35 if 90 <= d < 180 else 0.06,
    ),
    "drought": Scenario(
        "drought",
        "Drought: after six months activity falls to 15 percent and no repository joins",
        activity=lambda d: 1.0 if d < 180 else 0.15,
    ),
    "farm": Scenario(
        "farm",
        "Farm attack: from day 30 an attacker controls an eligible repository and six aged accounts; nobody reacts",
        attack_from=30,
    ),
    "farm_exiled": Scenario(
        "farm_exiled",
        "Farm attack answered: holders exile each farm 21 days after it starts; the attacker brings a new repository 60 days later",
        attack_from=30,
        exile_after=21,
    ),
}


def _drought_enroll(sc: Scenario, d: int) -> bool:
    return not (sc.name == "drought" and d >= 180)


@dataclass
class Person:
    gid: int
    link_day: int | None
    created_at: int = 0


@dataclass
class Result:
    scenario: str
    params: Params
    days: list = field(default_factory=list)
    supply: list = field(default_factory=list)
    minted: list = field(default_factory=list)
    burned: list = field(default_factory=list)
    escrow: list = field(default_factory=list)
    pot: list = field(default_factory=list)
    withheld: list = field(default_factory=list)
    attacker: list = field(default_factory=list)
    honest_minted: int = 0
    honest_capped: int = 0
    genesis_minted: int = 0
    genesis_capped: int = 0
    attacker_minted: int = 0
    merges_minted: int = 0
    repos: int = 0
    links: int = 0
    exiles: int = 0

    def coverage(self) -> float:
        tot = self.honest_minted + self.honest_capped
        return self.honest_minted / tot if tot else 1.0

    def summary(self) -> dict:
        def tok(x):
            return round(x / E18)

        return {
            "scenario": self.scenario,
            "supply_day_365": tok(self.supply[min(364, len(self.supply) - 1)]),
            "supply_end": tok(self.supply[-1]),
            "minted_end": tok(self.minted[-1]),
            "burned_end": tok(self.burned[-1]),
            "escrow_end": tok(self.escrow[-1]),
            "withheld_end": tok(self.withheld[-1]),
            "merges_minted": self.merges_minted,
            "honest_coverage": round(self.coverage(), 4),
            "genesis_coverage": round(self.genesis_minted / max(self.genesis_minted + self.genesis_capped, 1), 4),
            "attacker_share_day_365": round(self.attacker[min(364, len(self.attacker) - 1)] / max(self.supply[min(364, len(self.supply) - 1)], 1), 4),
            "attacker_share_end": round(self.attacker[-1] / max(self.supply[-1], 1), 4),
            "repos_end": self.repos,
            "links_end": self.links,
            "exiles": self.exiles,
        }


def load_snapshot(path: Path = SNAPSHOT) -> dict:
    snap = json.loads(path.read_text())
    profiles = {}
    for r in snap["repos"]:
        start = int(_ts(snap["from"]))
        ndays = (int(_ts(snap["to"])) - start) // DAY
        counts = [0] * ndays
        authors = {}
        for t, a, bot in r["merges"]:
            if bot:
                continue
            i = (t - start) // DAY
            if 0 <= i < ndays:
                counts[i] += 1
                authors[a] = authors.get(a, 0) + 1
        by_weekday = {w: [] for w in range(7)}
        for i, c in enumerate(counts):
            by_weekday[((start // DAY) + i + 3) % 7].append(c)  # 1970-01-01 was a Thursday
        ranked = sorted(authors.items(), key=lambda kv: -kv[1])
        profiles[r["repo"].lower()] = {"by_weekday": by_weekday, "authors": ranked}
    approvals = [x["approvals"] for x in snap["reviews_sample"]] or [1]
    return {"profiles": profiles, "approvals": approvals}


def _ts(s: str) -> float:
    from datetime import datetime

    return datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()


def run(sc: Scenario, p: Params, data: dict, seed: int = 7) -> Result:
    rng = random.Random(seed)
    led = Ledger(p)
    led.last_feast_at = T0
    res = Result(sc.name, p)
    people: dict[int, Person] = {}
    next_id = [10**9]
    genesis = list(data["profiles"].keys())
    repos = {}  # name -> (profile, author pool [(gid, weight)], maintainers)
    for name in genesis:
        prof = data["profiles"][name]
        repos[name] = prof
        led.repo_enrolled[name] = (T0, True)

    def person(gid: int, d: int, tourist: bool) -> Person:
        if gid not in people:
            rate = sc.tourist_link_rate if tourist else sc.link_rate
            young = tourist and rng.random() < 0.4
            link_day = None
            if rng.random() < rate:
                link_day = d + max(1, int(rng.expovariate(1 / 30)))
                if young:
                    link_day = max(link_day, d + 90)
            created = T0 + d * DAY - rng.randrange(5, 80) * DAY if young else 0
            people[gid] = Person(gid, link_day, created)
        return people[gid]

    def new_id() -> int:
        next_id[0] += 1
        return next_id[0]

    links_by_day: dict[int, list[int]] = {}
    attacker_ids: list[int] = []
    attacker_repos: list[str] = []
    attack_started: dict[str, int] = {}
    pending_farms: dict[int, str] = {}
    approvals = data["approvals"]

    for d in range(sc.days):
        day_start = T0 + d * DAY
        for gid in links_by_day.pop(d, []):
            led.link(gid, day_start)
            res.links += 1

        # New repositories join through the offering.
        if d > 0 and d % sc.enroll_every == 0 and len(repos) < sc.max_repos and _drought_enroll(sc, d):
            base = repos[rng.choice(genesis)]
            name = f"community/repo-{len(repos)}"
            pool = [(new_id(), w) for _, w in base["authors"]]
            repos[name] = {"by_weekday": base["by_weekday"], "authors": pool}
            led.repo_enrolled[name] = (day_start, False)
            if led.supply >= p.repo_enroll_burn:
                led.offering(p.repo_enroll_burn)

        if sc.attack_from is not None and d == sc.attack_from:
            attacker_ids = [new_id() for _ in range(sc.attack_puppets)]
            for gid in attacker_ids:
                led.link(gid, day_start)
            for k in range(sc.attack_repos):
                name = f"attacker/farm-{k}"
                attacker_repos.append(name)
                attack_started[name] = d
                led.repo_enrolled[name] = (day_start, False)
                led.offering(min(p.repo_enroll_burn, led.supply))

        weekday = ((day_start // DAY) + 3) % 7
        for name, prof in list(repos.items()):
            n = rng.choice(prof["by_weekday"][weekday])
            n = int(n * sc.activity(d) + rng.random())
            pool = prof["authors"]
            maintainers = [a for a, _ in pool[:8]]
            weights = [w for _, w in pool]
            for _ in range(n):
                tourist = rng.random() < sc.newcomer_rate(d)
                if tourist or not pool:
                    author = new_id()
                else:
                    author = rng.choices(pool, weights=weights)[0][0]
                pr = person(author, d, tourist)
                if pr.link_day is not None and pr.link_day < sc.days:
                    links_by_day.setdefault(pr.link_day, []).append(author)
                    pr.link_day = None  # scheduled once
                k = rng.choice(approvals) if rng.random() > 0.03 else 0
                revs = sorted({m for m in rng.sample(maintainers, min(len(maintainers), k + 1)) if m != author})[:k]
                for r in revs:
                    rp = person(r, d, False)
                    if rp.link_day is not None and rp.link_day < sc.days:
                        links_by_day.setdefault(rp.link_day, []).append(r)
                        rp.link_day = None
                merged_at = day_start + rng.randrange(DAY)
                status = led.settle_merge(name, merged_at, author, revs, ATTESTERS[: p.quorum], merged_at + 900, pr.created_at)
                genesis_repo = led.repo_enrolled[name][1]
                if status == "minted":
                    res.honest_minted += 1
                    res.genesis_minted += genesis_repo
                elif status == "capped":
                    res.honest_capped += 1
                    res.genesis_capped += genesis_repo

        if sc.exile_after is not None and attacker_repos:
            for name in list(attacker_repos):
                if d - attack_started[name] == sc.exile_after and name in led.repo_enrolled:
                    led.exile(name, max(led.exile_cost(name, day_start), 0), day_start)
                    res.exiles += 1
                    nxt = f"attacker/farm-{len(attack_started)}"
                    pending_farms[d + sc.reenroll_after] = nxt
            if d in pending_farms:
                nxt = pending_farms.pop(d)
                attacker_repos.append(nxt)
                attack_started[nxt] = d
                led.repo_enrolled[nxt] = (day_start, False)
                led.offering(min(p.repo_enroll_burn, led.supply))
        for name in attacker_repos:
            if name not in led.repo_enrolled:
                continue
            for i in range(sc.attack_tries):
                author = attacker_ids[i % len(attacker_ids)]
                others = sorted(g for g in attacker_ids if g != author)[:3]
                merged_at = day_start + (i * DAY) // sc.attack_tries
                if led.settle_merge(name, merged_at, author, others, ATTESTERS[: p.quorum], merged_at + 900) == "minted":
                    res.attacker_minted += 1

        now = day_start + DAY - 1
        if now >= led.last_feast_at + p.feast_period_secs:
            led.eat_stale_escrow(now)
            led.feast(now)

        att = sum(led.balances.get(("gh", g), 0) for g in attacker_ids) + sum(led.escrow.get(g, [0])[0] for g in attacker_ids)
        res.days.append(d)
        res.supply.append(led.supply)
        res.minted.append(led.minted)
        res.burned.append(led.burned)
        res.escrow.append(led.escrow_total())
        res.pot.append(led.pot)
        res.withheld.append(led.withheld)
        res.attacker.append(att)
    res.merges_minted = led.merges_minted
    res.repos = len(repos) + len(attacker_repos)
    return res
