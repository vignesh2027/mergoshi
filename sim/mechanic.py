"""The minting rules, mirrored from contract/src/mechanic.rs and contract/src/effects.rs.

Integer arithmetic in the token's smallest unit, exactly as the contract does
it. tests/test_golden.py and the contract's golden test replay the same events
through both implementations and compare every total.
"""

from __future__ import annotations

from dataclasses import dataclass, field

BPS = 10_000
DAY = 86_400
WEEK = 7 * DAY
E18 = 10**18


@dataclass(frozen=True)
class Params:
    base_reward: int = 100 * E18
    halving_every: int = 10_000
    author_bps: int = 7000
    review_bps: int = 2000
    attester_bps: int = 500
    feast_bps: int = 500
    max_paid_reviewers: int = 3
    streak_step_bps: int = 1000
    streak_max_bonus_bps: int = 4000
    repo_daily_cap: int = 12
    repo_floor: int = 1
    authors_per_slot: int = 3
    author_daily_cap: int = 4
    min_github_age_days: int = 90
    exile_min: int = 1_000 * E18
    max_merge_age_secs: int = 7 * DAY
    feast_period_secs: int = 30 * DAY
    feast_burn_bps: int = 5000
    feast_max_guests: int = 20
    escrow_ttl_secs: int = 60 * DAY
    repo_enroll_burn: int = 500 * E18
    quorum: int = 2

    def replace(self, **kw) -> "Params":
        d = self.__dict__.copy()
        d.update(kw)
        return Params(**d)


def era_reward(base: int, halving_every: int, merges_before: int) -> int:
    era = merges_before // max(halving_every, 1)
    return 0 if era >= 128 else base >> era


def streak_bonus_bps(weeks: int, step: int, max_bps: int) -> int:
    return min(max(weeks - 1, 0) * step, max_bps)


def next_streak(prev, week):
    if prev is None:
        return (week, 1)
    last, n = prev
    if week == last or week < last:
        return (last, n)
    if week == last + 1:
        return (week, n + 1)
    return (week, 1)


@dataclass
class Payout:
    reward: int
    author: int
    per_reviewer: int
    per_attester: int
    feast: int
    withheld: int

    def minted(self, reviewers: int, attesters: int) -> int:
        return self.author + self.per_reviewer * reviewers + self.per_attester * attesters + self.feast


def payout(p: Params, merges_before: int, streak: int, reviewers: int, attesters: int) -> Payout:
    r0 = era_reward(p.base_reward, p.halving_every, merges_before)
    reward = r0 * (BPS + streak_bonus_bps(streak, p.streak_step_bps, p.streak_max_bonus_bps)) // BPS
    author = reward * p.author_bps // BPS
    review_pool = reward * p.review_bps // BPS
    attest_pool = reward * p.attester_bps // BPS
    feast = reward - author - review_pool - attest_pool
    if reviewers == 0:
        per_reviewer, withheld = 0, review_pool
    else:
        per_reviewer = review_pool // reviewers
        feast += review_pool - per_reviewer * reviewers
        withheld = 0
    if attesters == 0:
        per_attester = 0
        feast += attest_pool
    else:
        per_attester = attest_pool // attesters
        feast += attest_pool - per_attester * attesters
    return Payout(reward, author, per_reviewer, per_attester, feast, withheld)


def feast_split(pot: int, burn_bps: int, guests):
    total = sum(c for _, c in guests)
    if total == 0:
        return pot, [0] * len(guests)
    serve = pot - pot * burn_bps // BPS
    shares = [serve * c // total for _, c in guests]
    return pot - sum(shares), shares


@dataclass
class Ledger:
    """Token state the rules touch. Balances are keyed by GitHub id for linked
    accounts and attesters by name; escrow is held for unlinked GitHub ids."""

    p: Params
    supply: int = 0
    minted: int = 0
    burned: int = 0
    withheld: int = 0
    pot: int = 0
    merges_minted: int = 0
    merges_capped: int = 0
    merges_stale: int = 0
    merges_young: int = 0
    balances: dict = field(default_factory=dict)  # holder -> amount
    escrow: dict = field(default_factory=dict)  # github id -> [amount, since] (since = first unclaimed coin)
    linked: set = field(default_factory=set)
    streaks: dict = field(default_factory=dict)
    repo_day: dict = field(default_factory=dict)
    repo_authors: dict = field(default_factory=dict)  # (repo, window) -> distinct aged authors
    seen: set = field(default_factory=set)
    repo_minted: dict = field(default_factory=dict)  # (repo, window) -> tokens minted for its merges
    exile_burns: dict = field(default_factory=dict)  # (repo, window) -> tokens burned against it
    exiled: set = field(default_factory=set)
    author_day: dict = field(default_factory=dict)
    guests: dict = field(default_factory=dict)
    repo_enrolled: dict = field(default_factory=dict)  # repo -> (enrolled_at, genesis)
    last_feast_at: int = 0

    def _mint(self, holder, amount):
        if amount:
            self.balances[holder] = self.balances.get(holder, 0) + amount
            self.supply += amount
            self.minted += amount

    def _credit(self, gid, amount, now):
        if gid in self.linked:
            self._mint(("gh", gid), amount)
        else:
            self.supply += amount
            self.minted += amount
            e = self.escrow.setdefault(gid, [0, now])
            e[0] += amount

    def repo_cap(self, repo, merged_at) -> int:
        """Merges a repository may mint on one day: a floor plus one slot for every
        `authors_per_slot` distinct aged authors it had this 30-day window or the last."""
        w = merged_at // (30 * DAY)
        diversity = max(self.repo_authors.get((repo, w), 0), self.repo_authors.get((repo, w - 1), 0))
        return min(self.p.repo_daily_cap, self.p.repo_floor + diversity // self.p.authors_per_slot)

    def settle_merge(self, repo, merged_at, author, approvers, attesters, now, author_created_at=0) -> str:
        """approvers: list of GitHub ids sorted ascending, author excluded."""
        p = self.p
        if repo not in self.repo_enrolled:
            return "not enrolled"
        enrolled_at, genesis = self.repo_enrolled[repo]
        earliest = enrolled_at - p.max_merge_age_secs if genesis else enrolled_at
        if merged_at + p.max_merge_age_secs < now or merged_at > now + 600 or merged_at < earliest:
            self.merges_stale += 1
            return "stale"
        if author_created_at + p.min_github_age_days * DAY > merged_at:
            self.merges_young += 1
            return "young"
        w = merged_at // (30 * DAY)
        if (repo, w, author) not in self.seen:
            self.seen.add((repo, w, author))
            self.repo_authors[(repo, w)] = self.repo_authors.get((repo, w), 0) + 1
        day = merged_at // DAY
        rk, ak = (repo, day), (author, day)
        if self.repo_day.get(rk, 0) >= self.repo_cap(repo, merged_at) or self.author_day.get(ak, 0) >= p.author_daily_cap:
            self.merges_capped += 1
            return "capped"
        self.repo_day[rk] = self.repo_day.get(rk, 0) + 1
        self.author_day[ak] = self.author_day.get(ak, 0) + 1
        st = next_streak(self.streaks.get(author), merged_at // WEEK)
        self.streaks[author] = st
        reviewers = approvers[: p.max_paid_reviewers]
        pay = payout(p, self.merges_minted, st[1], len(reviewers), len(attesters))
        self.merges_minted += 1
        self._credit(author, pay.author, now)
        for r in reviewers:
            self._credit(r, pay.per_reviewer, now)
            self.guests[r] = self.guests.get(r, 0) + 1
        for a in attesters:
            self._mint(("attester", a), pay.per_attester)
        self.supply += pay.feast
        self.minted += pay.feast
        self.pot += pay.feast
        self.withheld += pay.withheld
        wk = (repo, now // (30 * DAY))
        self.repo_minted[wk] = self.repo_minted.get(wk, 0) + pay.minted(len(reviewers), len(attesters))
        return "minted"

    def exile_cost(self, repo, now) -> int:
        w = now // (30 * DAY)
        minted = self.repo_minted.get((repo, w), 0) + self.repo_minted.get((repo, w - 1), 0)
        burned = self.exile_burns.get((repo, w), 0) + self.exile_burns.get((repo, w - 1), 0)
        return max(self.p.exile_min, minted) - burned

    def exile(self, repo, amount, now) -> bool:
        """Burns `amount` against a repository. Once the burns of this window and
        the last reach what the repository minted in the same span (and at least
        exile_min), it is exiled for good."""
        need = self.exile_cost(repo, now)
        w = now // (30 * DAY)
        self.exile_burns[(repo, w)] = self.exile_burns.get((repo, w), 0) + amount
        self.supply -= amount
        self.burned += amount
        if amount >= need:
            self.exiled.add(repo)
            self.repo_enrolled.pop(repo, None)
            return True
        return False

    def link(self, gid, now):
        if gid in self.linked:
            return
        self.linked.add(gid)
        e = self.escrow.pop(gid, None)
        if e:
            self.balances[("gh", gid)] = self.balances.get(("gh", gid), 0) + e[0]

    def escrow_total(self) -> int:
        return sum(e[0] for e in self.escrow.values())

    def eat_stale_escrow(self, now) -> int:
        eaten = 0
        for gid in [g for g, e in self.escrow.items() if now >= e[1] + self.p.escrow_ttl_secs]:
            eaten += self.escrow.pop(gid)[0]
        self.supply -= eaten
        self.burned += eaten
        return eaten

    def feast(self, now) -> int:
        guests = sorted(self.guests.items(), key=lambda g: (-g[1], g[0]))[: self.p.feast_max_guests]
        burned, shares = feast_split(self.pot, self.p.feast_burn_bps, guests)
        for (gid, _), share in zip(guests, shares):
            if share == 0:
                continue
            if gid in self.linked:
                self.balances[("gh", gid)] = self.balances.get(("gh", gid), 0) + share
            else:
                e = self.escrow.setdefault(gid, [0, now])
                e[0] += share
        self.supply -= burned
        self.burned += burned
        self.pot = 0
        self.guests = {}
        self.last_feast_at = now
        return burned

    def offering(self, amount) -> None:
        self.supply -= amount
        self.burned += amount
