//! The minting arithmetic, kept free of contract state so the simulation in
//! `sim/` can mirror it line by line and a golden test can compare the two.

pub const BPS: u128 = 10_000;
pub const DAY: u64 = 86_400;
pub const WEEK: u64 = 7 * DAY;

/// How one merge's reward is cut up. Every amount is in the token's smallest unit.
#[derive(Debug, Clone, PartialEq, Eq, Default)]
pub struct Payout {
    /// Reward for this merge after the halving era and the streak bonus.
    pub reward: u128,
    pub author: u128,
    /// Paid to each counted approver.
    pub per_reviewer: u128,
    /// Paid to each attester in the quorum that settled the merge.
    pub per_attester: u128,
    /// Goes to the feast pot, including every rounding remainder.
    pub feast: u128,
    /// The review share of an unreviewed merge. It is never minted.
    pub withheld: u128,
}

impl Payout {
    /// Total amount minted for the merge.
    pub fn minted(&self, reviewers: u32, attesters: u32) -> u128 {
        self.author
            + self.per_reviewer * reviewers as u128
            + self.per_attester * attesters as u128
            + self.feast
    }
}

/// The split of a reward, in basis points that add up to 10 000.
#[derive(Debug, Clone, Copy)]
pub struct Split {
    pub author_bps: u16,
    pub review_bps: u16,
    pub attester_bps: u16,
    pub feast_bps: u16,
}

/// Reward before the streak bonus: `base` halves after every `halving_every` minted merges.
pub fn era_reward(base: u128, halving_every: u64, merges_before: u64) -> u128 {
    let era = merges_before / halving_every.max(1);
    if era >= 128 { 0 } else { base >> era }
}

/// Streak bonus in basis points for an author on their `streak_weeks`-th
/// consecutive week of merged work.
pub fn streak_bonus_bps(streak_weeks: u32, step_bps: u16, max_bps: u16) -> u128 {
    let extra = streak_weeks.saturating_sub(1) as u128 * step_bps as u128;
    extra.min(max_bps as u128)
}

/// Updates an author's streak with a merge landed in `week`.
/// Returns the new (last_week, streak). A merge in an older week than the last
/// one recorded (an attestation that arrived late) leaves the streak alone.
pub fn next_streak(prev: Option<(u64, u32)>, week: u64) -> (u64, u32) {
    match prev {
        None => (week, 1),
        Some((last, n)) if week == last => (last, n),
        Some((last, n)) if week == last + 1 => (week, n.saturating_add(1)),
        Some((last, n)) if week < last => (last, n),
        Some(_) => (week, 1),
    }
}

/// Cuts one merge's reward.
#[allow(clippy::too_many_arguments)]
pub fn payout(
    base: u128,
    halving_every: u64,
    merges_before: u64,
    streak_weeks: u32,
    step_bps: u16,
    max_bonus_bps: u16,
    split: Split,
    reviewers: u32,
    attesters: u32,
) -> Payout {
    let r0 = era_reward(base, halving_every, merges_before);
    let reward = r0 * (BPS + streak_bonus_bps(streak_weeks, step_bps, max_bonus_bps)) / BPS;
    let author = reward * split.author_bps as u128 / BPS;
    let review_pool = reward * split.review_bps as u128 / BPS;
    let attest_pool = reward * split.attester_bps as u128 / BPS;
    let mut feast = reward - author - review_pool - attest_pool;

    let (per_reviewer, withheld) = if reviewers == 0 {
        (0, review_pool)
    } else {
        let each = review_pool / reviewers as u128;
        feast += review_pool - each * reviewers as u128;
        (each, 0)
    };
    let per_attester = if attesters == 0 {
        feast += attest_pool;
        0
    } else {
        let each = attest_pool / attesters as u128;
        feast += attest_pool - each * attesters as u128;
        each
    };
    Payout {
        reward,
        author,
        per_reviewer,
        per_attester,
        feast,
        withheld,
    }
}

/// Splits the feast pot. `guests` are (id, approvals this period), already
/// sorted and truncated to the guest limit. Returns (burned, shares) where the
/// shares line up with `guests` and every remainder is burned.
pub fn feast_split(pot: u128, burn_bps: u16, guests: &[(u64, u32)]) -> (u128, Vec<u128>) {
    let total: u128 = guests.iter().map(|g| g.1 as u128).sum();
    if total == 0 {
        return (pot, vec![0; guests.len()]);
    }
    let serve = pot - pot * burn_bps as u128 / BPS;
    let shares: Vec<u128> = guests.iter().map(|g| serve * g.1 as u128 / total).collect();
    let served: u128 = shares.iter().sum();
    (pot - served, shares)
}

#[cfg(test)]
mod tests {
    use super::*;

    const SPLIT: Split = Split {
        author_bps: 7000,
        review_bps: 2000,
        attester_bps: 500,
        feast_bps: 500,
    };
    const E18: u128 = 1_000_000_000_000_000_000;

    #[test]
    fn halving() {
        assert_eq!(era_reward(100, 10, 0), 100);
        assert_eq!(era_reward(100, 10, 9), 100);
        assert_eq!(era_reward(100, 10, 10), 50);
        assert_eq!(era_reward(100, 10, 35), 12);
        assert_eq!(era_reward(100, 10, 10_000), 0);
    }

    #[test]
    fn streaks() {
        assert_eq!(next_streak(None, 5), (5, 1));
        assert_eq!(next_streak(Some((5, 1)), 5), (5, 1));
        assert_eq!(next_streak(Some((5, 1)), 6), (6, 2));
        assert_eq!(next_streak(Some((6, 2)), 8), (8, 1));
        assert_eq!(next_streak(Some((6, 2)), 4), (6, 2));
        assert_eq!(streak_bonus_bps(1, 1000, 4000), 0);
        assert_eq!(streak_bonus_bps(3, 1000, 4000), 2000);
        assert_eq!(streak_bonus_bps(30, 1000, 4000), 4000);
    }

    #[test]
    fn payout_adds_up_and_withholds_unreviewed_share() {
        let p = payout(100 * E18, 50_000, 0, 1, 1000, 4000, SPLIT, 2, 3);
        assert_eq!(p.reward, 100 * E18);
        assert_eq!(p.author, 70 * E18);
        assert_eq!(p.per_reviewer, 10 * E18);
        assert_eq!(p.withheld, 0);
        assert_eq!(p.minted(2, 3) + p.withheld, p.reward);

        let solo = payout(100 * E18, 50_000, 0, 1, 1000, 4000, SPLIT, 0, 3);
        assert_eq!(solo.withheld, 20 * E18);
        assert_eq!(solo.minted(0, 3), 80 * E18);

        // Odd sizes leave remainders; they go to the feast, nothing is lost.
        let odd = payout(100, 50_000, 0, 4, 1000, 4000, SPLIT, 3, 3);
        assert_eq!(odd.reward, 130);
        assert_eq!(odd.minted(3, 3) + odd.withheld, odd.reward);
    }

    #[test]
    fn feast_burns_remainders() {
        let (burned, shares) = feast_split(1000, 5000, &[(1, 2), (2, 1)]);
        assert_eq!(shares, vec![333, 166]);
        assert_eq!(burned, 501);
        let (burned, shares) = feast_split(1000, 5000, &[]);
        assert_eq!((burned, shares.len()), (1000, 0));
    }
}
