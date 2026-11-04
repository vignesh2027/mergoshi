//! What happens when a round settles, plus the feast and the escrow rules.

use crate::claim::{Claim, LinkClaim, MergeClaim, RepoClaim};
use crate::mechanic::{self, DAY, Split, WEEK};
use crate::{Escrow, Link, Merged, MergedExt, Part, RepoInfo, now_secs};
use near_sdk::json_types::U128;
use near_sdk::{AccountId, env, near, require};

/// A merge may claim a merge time up to this far in the future (clock skew).
const FUTURE_SLACK: u64 = 600;

#[near]
impl Merged {
    /// The maintainers' feast. Once per feast period anyone may call it: the
    /// pot is split, `feast_burn_bps` of it is burned and the rest is served to
    /// the period's most active reviewers in proportion to their paid approvals.
    pub fn feast(&mut self) -> U128 {
        let now = now_secs();
        require!(
            now >= self.stats.last_feast_at + self.params.feast_period_secs,
            "the feast is not ready yet"
        );
        let pot = self.stats.feast_pot.0;
        let mut guests: Vec<(u64, u32)> = self.guests.iter().map(|(k, v)| (*k, *v)).collect();
        guests.sort_by(|a, b| b.1.cmp(&a.1).then(a.0.cmp(&b.0)));
        guests.truncate(self.params.feast_max_guests as usize);
        let (burned, shares) = mechanic::feast_split(pot, self.params.feast_burn_bps, &guests);
        let mut parts = vec![];
        for ((id, _), share) in guests.iter().zip(shares) {
            if share > 0 {
                parts.push(self.serve(*id, share));
            }
        }
        self.burn_own(burned, "feast");
        self.stats.feast_pot = U128(0);
        self.stats.feasts += 1;
        self.stats.last_feast_at = now;
        self.guests.clear();
        let n = parts.len();
        self.push_log(
            "feast",
            "",
            0,
            burned,
            parts,
            format!("feast {}: pot {pot}, {n} guests served", self.stats.feasts),
        );
        U128(burned)
    }

    /// Burns escrow whose first unclaimed coin is older than `escrow_ttl_secs`.
    /// Anyone may call it: link within the window or the coins are gone.
    pub fn eat_stale_escrow(&mut self, github_ids: Vec<u64>) -> U128 {
        require!(github_ids.len() <= 50, "at most 50 ids per call");
        let now = now_secs();
        let mut eaten = 0u128;
        let mut parts = vec![];
        for id in github_ids {
            let Some(e) = self.escrow.get(&id).cloned() else {
                continue;
            };
            if now < e.since + self.params.escrow_ttl_secs {
                continue;
            }
            self.escrow.remove(&id);
            eaten += e.amount.0;
            parts.push(Part {
                role: "unclaimed".into(),
                to: format!("github:{id}"),
                amount: e.amount,
            });
        }
        if eaten > 0 {
            self.stats.escrow_outstanding = U128(self.stats.escrow_outstanding.0 - eaten);
            self.burn_own(eaten, "unclaimed escrow");
            self.push_log(
                "escrow_burned",
                "",
                0,
                eaten,
                parts,
                "escrow nobody claimed in time was burned".into(),
            );
        }
        U128(eaten)
    }

    /// The exile ritual. The caller burns `amount` against an enrolled
    /// repository. Once the burns against it in this 30-day window and the last
    /// reach what the repository minted over the same span (and at least
    /// `exile_min`), the repository is exiled for good. Returns true on exile.
    #[payable]
    pub fn exile(&mut self, repo: String, amount: U128) -> bool {
        near_sdk::assert_one_yocto();
        require!(self.repos.contains_key(&repo), "not an enrolled repository");
        require!(amount.0 > 0, "burn something");
        let who = env::predecessor_account_id();
        let now = now_secs();
        let need = self.exile_need(&repo, now);
        self.token.internal_withdraw(&who, amount.0);
        near_contract_standards::fungible_token::events::FtBurn {
            owner_id: &who,
            amount,
            memo: Some("exile"),
        }
        .emit();
        self.stats.total_burned = U128(self.stats.total_burned.0 + amount.0);
        let k = format!("{repo}|{}", now / WINDOW);
        let prev = self.exile_burns.get(&k).copied().unwrap_or(0);
        self.exile_burns.insert(k, prev + amount.0);
        let parts = vec![Part {
            role: "exiler".into(),
            to: who.to_string(),
            amount,
        }];
        let key = format!("repo:{repo}");
        if amount.0 >= need {
            self.repos.remove(&repo);
            self.exiled.insert(repo.clone(), now);
            self.stats.exiles += 1;
            self.stats.repos -= 1;
            self.push_log(
                "exile",
                &key,
                0,
                amount.0,
                parts,
                format!("{repo} is exiled and can never mint again"),
            );
            true
        } else {
            self.push_log(
                "exile_burn",
                &key,
                0,
                amount.0,
                parts,
                format!("{} more to exile {repo}", need - amount.0),
            );
            false
        }
    }
}

/// Length of the windows used by the diversity cap and the exile ritual.
pub(crate) const WINDOW: u64 = 30 * DAY;

impl Merged {
    pub(crate) fn settle(&mut self, key: &str, claim: Claim, quorum: &[AccountId]) -> String {
        match claim {
            Claim::Merge(m) => self.settle_merge(key, m, quorum),
            Claim::Link(l) => self.settle_link(key, l),
            Claim::Repo(r) => self.settle_repo(key, r),
        }
    }

    pub(crate) fn reject(&mut self, key: &str) {
        self.stats.rounds_rejected += 1;
        if key.starts_with("link:") {
            self.pending_links.remove(key);
        } else if let Some(repo) = key.strip_prefix("repo:") {
            self.pending_repos.remove(repo);
        }
        self.push_log(
            "rejected",
            key,
            0,
            0,
            vec![],
            "a quorum of attesters said this did not happen".into(),
        );
    }

    fn settle_merge(&mut self, key: &str, m: MergeClaim, quorum: &[AccountId]) -> String {
        let now = now_secs();
        let p = self.params.clone();
        let Some(info) = self.repos.get(&m.repo).cloned() else {
            self.push_log(
                "stale",
                key,
                0,
                0,
                vec![],
                "the repository was exiled before this settled".into(),
            );
            return "not enrolled".into();
        };
        let earliest = if info.genesis {
            info.enrolled_at.saturating_sub(p.max_merge_age_secs)
        } else {
            info.enrolled_at
        };
        if m.merged_at + p.max_merge_age_secs < now
            || m.merged_at > now + FUTURE_SLACK
            || m.merged_at < earliest
        {
            self.stats.merges_stale += 1;
            self.push_log(
                "stale",
                key,
                0,
                0,
                vec![],
                format!("merged at {} is outside the accepted age", m.merged_at),
            );
            return "stale".into();
        }
        if m.author_created_at + p.min_github_age_days as u64 * DAY > m.merged_at {
            self.stats.merges_young += 1;
            self.push_log(
                "young",
                key,
                0,
                0,
                vec![],
                format!(
                    "{} has a GitHub account younger than {} days",
                    m.author_login, p.min_github_age_days
                ),
            );
            return "young".into();
        }
        // Count the author toward the repository's diversity for this window.
        let w = m.merged_at / WINDOW;
        let seen_key = format!("{}|{w}|{}", m.repo, m.author_id);
        if !self.seen.contains_key(&seen_key) {
            self.seen.insert(seen_key, ());
            let k = format!("{}|{w}", m.repo);
            let n = self.repo_authors.get(&k).copied().unwrap_or(0);
            self.repo_authors.insert(k, n + 1);
        }
        let day = m.merged_at / DAY;
        let repo_day_key = format!("{}|{day}", m.repo);
        let author_day_key = format!("{}|{day}", m.author_id);
        let repo_n = self.repo_day.get(&repo_day_key).copied().unwrap_or(0);
        let author_n = self.author_day.get(&author_day_key).copied().unwrap_or(0);
        let repo_cap = self.repo_cap_at(&m.repo, m.merged_at);
        if repo_n >= repo_cap || author_n >= p.author_daily_cap {
            self.stats.merges_capped += 1;
            let which = if repo_n >= repo_cap {
                format!("repository daily cap of {repo_cap}")
            } else {
                "author daily cap".to_string()
            };
            self.push_log("capped", key, 0, 0, vec![], format!("{which} reached"));
            return "capped".into();
        }
        self.repo_day.insert(repo_day_key, repo_n + 1);
        self.author_day.insert(author_day_key, author_n + 1);

        let (last_week, streak) =
            mechanic::next_streak(self.streaks.get(&m.author_id).copied(), m.merged_at / WEEK);
        self.streaks.insert(m.author_id, (last_week, streak));

        let reviewers: Vec<_> = m
            .approvers
            .iter()
            .take(p.max_paid_reviewers as usize)
            .cloned()
            .collect();
        let split = Split {
            author_bps: p.author_bps,
            review_bps: p.review_bps,
            attester_bps: p.attester_bps,
            feast_bps: p.feast_bps,
        };
        let pay = mechanic::payout(
            p.base_reward.0,
            p.halving_every,
            self.stats.merges_minted,
            streak,
            p.streak_step_bps,
            p.streak_max_bonus_bps,
            split,
            reviewers.len() as u32,
            quorum.len() as u32,
        );
        self.stats.merges_minted += 1;

        let mut parts = vec![self.credit(m.author_id, &m.author_login, pay.author, "author", key)];
        for r in &reviewers {
            parts.push(self.credit(r.id, &r.login, pay.per_reviewer, "reviewer", key));
            let n = self.guests.get(&r.id).copied().unwrap_or(0);
            self.guests.insert(r.id, n + 1);
        }
        for a in quorum {
            self.mint_to(a, pay.per_attester, key);
            if let Some(mut st) = self.attesters.get(a).cloned() {
                st.earned = U128(st.earned.0 + pay.per_attester);
                self.attesters.insert(a.clone(), st);
            }
            parts.push(Part {
                role: "attester".into(),
                to: a.to_string(),
                amount: U128(pay.per_attester),
            });
        }
        let me = env::current_account_id();
        self.mint_to(&me, pay.feast, "feast pot");
        self.stats.feast_pot = U128(self.stats.feast_pot.0 + pay.feast);
        parts.push(Part {
            role: "feast pot".into(),
            to: me.to_string(),
            amount: U128(pay.feast),
        });
        self.stats.withheld = U128(self.stats.withheld.0 + pay.withheld);

        let minted = pay.minted(reviewers.len() as u32, quorum.len() as u32);
        let mk = format!("{}|{}", m.repo, now / WINDOW);
        let prev = self.repo_minted.get(&mk).copied().unwrap_or(0);
        self.repo_minted.insert(mk, prev + minted);
        let mut note = format!("{} merged {}#{}", m.author_login, m.repo, m.number);
        if streak > 1 {
            note.push_str(&format!(", week {streak} of a streak"));
        }
        if reviewers.is_empty() {
            note.push_str(", nobody approved so the review share was not minted");
        }
        self.push_log("merge", key, minted, 0, parts, note);
        "minted".into()
    }

    /// Pays a GitHub account: to its linked NEAR account, or into escrow held by the contract.
    fn credit(
        &mut self,
        github_id: u64,
        login: &str,
        amount: u128,
        role: &str,
        memo: &str,
    ) -> Part {
        if let Some(link) = self.links.get(&github_id).cloned() {
            self.mint_to(&link.account, amount, memo);
            return Part {
                role: role.into(),
                to: link.account.to_string(),
                amount: U128(amount),
            };
        }
        let me = env::current_account_id();
        self.mint_to(&me, amount, memo);
        self.add_escrow(github_id, login, amount);
        Part {
            role: role.into(),
            to: format!("github:{github_id}"),
            amount: U128(amount),
        }
    }

    fn add_escrow(&mut self, github_id: u64, login: &str, amount: u128) {
        let mut e = self.escrow.get(&github_id).cloned().unwrap_or(Escrow {
            login: login.to_string(),
            amount: U128(0),
            since: now_secs(),
        });
        e.amount = U128(e.amount.0 + amount);
        self.escrow.insert(github_id, e);
        self.stats.escrow_outstanding = U128(self.stats.escrow_outstanding.0 + amount);
    }

    /// Feast share for a guest: moved out of the pot to the linked account, or
    /// kept by the contract as that account's escrow.
    fn serve(&mut self, github_id: u64, amount: u128) -> Part {
        let me = env::current_account_id();
        if let Some(link) = self.links.get(&github_id).cloned() {
            self.token
                .internal_transfer(&me, &link.account, amount, Some("feast".into()));
            return Part {
                role: "guest".into(),
                to: link.account.to_string(),
                amount: U128(amount),
            };
        }
        let login = self
            .escrow
            .get(&github_id)
            .map(|e| e.login.clone())
            .unwrap_or_default();
        self.add_escrow(github_id, &login, amount);
        Part {
            role: "guest".into(),
            to: format!("github:{github_id}"),
            amount: U128(amount),
        }
    }

    /// Diversity cap: a floor plus one slot per `authors_per_slot` distinct aged
    /// authors in the current or previous window, never above `repo_daily_cap`.
    pub(crate) fn repo_cap_at(&self, repo: &str, at: u64) -> u32 {
        let w = at / WINDOW;
        let cur = self
            .repo_authors
            .get(&format!("{repo}|{w}"))
            .copied()
            .unwrap_or(0);
        let prev = if w > 0 {
            self.repo_authors
                .get(&format!("{repo}|{}", w - 1))
                .copied()
                .unwrap_or(0)
        } else {
            0
        };
        let p = &self.params;
        (p.repo_floor + cur.max(prev) / p.authors_per_slot).min(p.repo_daily_cap)
    }

    /// Tokens still to burn before `repo` is exiled.
    pub(crate) fn exile_need(&self, repo: &str, now: u64) -> u128 {
        let w = now / WINDOW;
        let get = |m: &near_sdk::store::LookupMap<String, u128>, w: u64| {
            m.get(&format!("{repo}|{w}")).copied().unwrap_or(0)
        };
        let minted = get(&self.repo_minted, w)
            + if w > 0 {
                get(&self.repo_minted, w - 1)
            } else {
                0
            };
        let burned = get(&self.exile_burns, w)
            + if w > 0 {
                get(&self.exile_burns, w - 1)
            } else {
                0
            };
        minted.max(self.params.exile_min.0).saturating_sub(burned)
    }

    fn settle_link(&mut self, key: &str, l: LinkClaim) -> String {
        let now = now_secs();
        let Some(req) = self.pending_links.remove(key) else {
            return "no request".into();
        };
        if req.by != l.account {
            return "request was made by another account".into();
        }
        if l.github_created_at + self.params.min_github_age_days as u64 * DAY > now {
            self.push_log(
                "rejected",
                key,
                0,
                0,
                vec![],
                format!(
                    "github account {} is younger than {} days",
                    l.login, self.params.min_github_age_days
                ),
            );
            return "too young".into();
        }
        if self.links.contains_key(&l.github_id) || self.account_links.contains_key(&l.account) {
            return "already linked".into();
        }
        self.links.insert(
            l.github_id,
            Link {
                account: l.account.clone(),
                login: l.login.clone(),
                linked_at: now,
            },
        );
        self.account_links.insert(l.account.clone(), l.github_id);
        self.stats.links += 1;
        let mut parts = vec![];
        if let Some(e) = self.escrow.remove(&l.github_id)
            && e.amount.0 > 0
        {
            let me = env::current_account_id();
            self.token.internal_transfer(
                &me,
                &l.account,
                e.amount.0,
                Some("escrow released".into()),
            );
            self.stats.escrow_outstanding = U128(self.stats.escrow_outstanding.0 - e.amount.0);
            parts.push(Part {
                role: "escrow released".into(),
                to: l.account.to_string(),
                amount: e.amount,
            });
        }
        self.push_log(
            "link",
            key,
            0,
            0,
            parts,
            format!("github {} is now {}", l.login, l.account),
        );
        "linked".into()
    }

    fn settle_repo(&mut self, key: &str, r: RepoClaim) -> String {
        let now = now_secs();
        if self.pending_repos.remove(&r.repo).is_none() {
            return "no proposal".into();
        }
        let p = &self.params;
        let old_enough = r.created_at + p.min_repo_age_days as u64 * DAY <= now;
        if r.fork || r.stars < p.min_repo_stars as u64 || !old_enough {
            let why = format!(
                "{} has {} stars, created {}, fork {}: below the bar",
                r.repo, r.stars, r.created_at, r.fork
            );
            self.push_log("rejected", key, 0, 0, vec![], why);
            return "not eligible".into();
        }
        self.repos.insert(
            r.repo.clone(),
            RepoInfo {
                enrolled_at: now,
                stars: r.stars,
                genesis: false,
            },
        );
        self.stats.repos += 1;
        self.push_log(
            "enroll",
            key,
            0,
            0,
            vec![],
            format!("{} joined with {} stars", r.repo, r.stars),
        );
        "enrolled".into()
    }
}
