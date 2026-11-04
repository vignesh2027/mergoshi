"""Run with: python3 -m unittest discover -s sim/tests -t sim   (or pytest sim/tests)"""

import json
import sys
import unittest
from pathlib import Path

SIM = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(SIM))

import golden  # noqa: E402
from mechanic import DAY, E18, Ledger, Params, feast_split, next_streak, payout  # noqa: E402
from model import SCENARIOS, Scenario, load_snapshot, run  # noqa: E402


class MechanicTest(unittest.TestCase):
    def test_payout_matches_contract_unit_test(self):
        p = Params()
        pay = payout(p, 0, 1, 2, 3)
        self.assertEqual(pay.author, 70 * E18)
        self.assertEqual(pay.per_reviewer, 10 * E18)
        self.assertEqual(pay.minted(2, 3) + pay.withheld, pay.reward)
        odd = payout(p.replace(base_reward=100), 0, 4, 3, 3)
        self.assertEqual(odd.reward, 130)
        self.assertEqual(odd.minted(3, 3) + odd.withheld, odd.reward)

    def test_halving_and_streaks(self):
        p = Params(halving_every=10)
        self.assertEqual(payout(p, 10, 1, 1, 2).reward, 50 * E18)
        self.assertEqual(next_streak((6, 2), 7), (7, 3))
        self.assertEqual(next_streak((6, 2), 9), (9, 1))
        self.assertEqual(next_streak((6, 2), 4), (6, 2))

    def test_feast_split_burns_remainder(self):
        self.assertEqual(feast_split(1000, 5000, [(1, 2), (2, 1)]), (501, [333, 166]))

    def test_diversity_cap(self):
        led = Ledger(Params(repo_floor=1, authors_per_slot=3))
        led.repo_enrolled["a/b"] = (0, True)
        t = 400 * DAY
        got = [led.settle_merge("a/b", t, a, [], ["x", "y"], t + 60) for a in (11, 12, 13, 11)]
        self.assertEqual(got, ["minted", "capped", "minted", "capped"])

    def test_young_authors_and_stale_merges(self):
        led = Ledger(Params())
        led.repo_enrolled["a/b"] = (0, True)
        t = 400 * DAY
        self.assertEqual(led.settle_merge("a/b", t, 1, [], ["x"], t + 60, author_created_at=t - 10 * DAY), "young")
        self.assertEqual(led.settle_merge("a/b", t, 1, [], ["x"], t + 8 * DAY), "stale")

    def test_exile_needs_what_the_repo_minted(self):
        led = Ledger(Params(exile_min=E18))
        led.repo_enrolled["a/b"] = (0, True)
        t = 400 * DAY
        led.settle_merge("a/b", t, 1, [], ["x", "y"], t + 60)
        self.assertEqual(led.exile_cost("a/b", t + 60), 80 * E18)
        self.assertFalse(led.exile("a/b", 50 * E18, t + 60))
        self.assertTrue(led.exile("a/b", 30 * E18, t + 60))
        self.assertNotIn("a/b", led.repo_enrolled)


class GoldenTest(unittest.TestCase):
    def test_golden_file_is_current(self):
        on_disk = json.loads((SIM / "golden.json").read_text())
        self.assertEqual(on_disk, json.loads(json.dumps(golden.build())))


class ScenarioTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.data = load_snapshot()
        cls.res = {k: run(Scenario(**{**sc.__dict__, "days": 365}), Params(), cls.data) for k, sc in SCENARIOS.items()}

    def test_books_balance(self):
        for r in self.res.values():
            self.assertEqual(r.supply[-1], r.minted[-1] - r.burned[-1])

    def test_honest_merges_mostly_mint(self):
        self.assertGreater(self.res["steady"].coverage(), 0.85)

    def test_drought_shrinks_supply_growth(self):
        self.assertLess(self.res["drought"].supply[-1], self.res["steady"].supply[-1])

    def test_exile_contains_the_farm(self):
        farm = self.res["farm"]
        ex = self.res["farm_exiled"]
        self.assertLess(ex.attacker[-1] / ex.supply[-1], farm.attacker[-1] / farm.supply[-1] / 2)


if __name__ == "__main__":
    unittest.main()
