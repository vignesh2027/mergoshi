//! Commit and reveal rounds. One round per key (a merge, a link request or a
//! repository proposal). Attesters commit a hash bound to their own account,
//! reveal it once the commit window closes or everyone has committed, and the
//! first claim hash revealed by a quorum settles the round.

use crate::claim::{self, Kind, NONE_HASH};
use crate::{Mergoshi, MergoshiExt, Round, now_secs};
use near_sdk::json_types::U128;
use near_sdk::{AccountId, NearToken, Promise, env, near};

#[near(serializers = [json])]
pub struct CommitItem {
    pub key: String,
    /// sha256(attester || "\n" || key || "\n" || claim_hash || salt), hex.
    pub commitment: String,
}

#[near(serializers = [json])]
pub struct RevealItem {
    pub key: String,
    /// The canonical claim text, or null to say the event did not happen.
    pub claim: Option<String>,
    /// 32 bytes, hex.
    pub salt: String,
}

#[near(serializers = [json])]
pub struct RoundView {
    pub key: String,
    pub opened_at: u64,
    pub commit_deadline: u64,
    pub reveal_deadline: u64,
    pub committed: Vec<AccountId>,
    pub revealed: Vec<(AccountId, String)>,
    pub outcome: Option<String>,
    pub result: String,
}

pub(crate) const DONE_SETTLED: u8 = 1;

#[near]
impl Mergoshi {
    /// Commits to claims. Items that cannot be committed are skipped and the
    /// reason is returned in the same position, so one bad item never sinks a batch.
    pub fn commit(&mut self, items: Vec<CommitItem>) -> Vec<String> {
        let who = env::predecessor_account_id();
        let active = self.attesters.get(&who).is_some_and(|a| a.active);
        near_sdk::require!(active, "only a bonded, active attester can commit");
        near_sdk::require!(items.len() <= 60, "at most 60 items per call");
        let now = now_secs();
        items
            .into_iter()
            .map(
                |it| match self.commit_one(&who, &it.key, &it.commitment, now) {
                    Ok(()) => "ok".to_string(),
                    Err(e) => e,
                },
            )
            .collect()
    }

    /// Reveals earlier commitments. Settles a round as soon as a quorum agrees.
    pub fn reveal(&mut self, items: Vec<RevealItem>) -> Vec<String> {
        let who = env::predecessor_account_id();
        near_sdk::require!(
            self.attesters.contains_key(&who),
            "only an attester can reveal"
        );
        near_sdk::require!(items.len() <= 30, "at most 30 items per call");
        let now = now_secs();
        items
            .into_iter()
            .map(
                |it| match self.reveal_one(&who, &it.key, it.claim.as_deref(), &it.salt, now) {
                    Ok(s) => s,
                    Err(e) => e,
                },
            )
            .collect()
    }

    /// Closes rounds that are finished: settled rounds once everyone who
    /// committed has revealed or the reveal window has passed, and unsettled
    /// rounds once the reveal window has passed. Anyone may call it.
    pub fn sweep(&mut self, keys: Vec<String>) -> Vec<String> {
        let now = now_secs();
        keys.into_iter()
            .map(|key| {
                let Some(r) = self.rounds.get(&key).cloned() else {
                    return "no round".to_string();
                };
                let deadline = self.reveal_deadline(&r);
                let all_revealed = r.reveals.len() == r.commits.len();
                if r.outcome.is_some() && (all_revealed || now > deadline) {
                    self.rounds.remove(&key);
                    self.count_missed(&r);
                    "closed".to_string()
                } else if r.outcome.is_none() && now > deadline {
                    self.rounds.remove(&key);
                    self.count_missed(&r);
                    self.stats.rounds_expired += 1;
                    "expired".to_string()
                } else {
                    "still open".to_string()
                }
            })
            .collect()
    }

    pub fn round(&self, key: String) -> Option<RoundView> {
        self.rounds.get(&key).map(|r| self.round_view(&key, r))
    }

    pub fn open_rounds(&self, from_index: Option<u32>, limit: Option<u32>) -> Vec<RoundView> {
        let from = from_index.unwrap_or(0) as usize;
        let limit = limit.unwrap_or(50).min(100) as usize;
        self.rounds
            .iter()
            .skip(from)
            .take(limit)
            .map(|(k, r)| self.round_view(k, r))
            .collect()
    }
}

impl Mergoshi {
    fn commit_deadline(&self, r: &Round) -> u64 {
        r.opened_at + self.params.commit_window_secs
    }
    fn reveal_deadline(&self, r: &Round) -> u64 {
        self.commit_deadline(r) + self.params.reveal_window_secs
    }

    /// Records attesters that committed to a closed round and never revealed.
    fn count_missed(&mut self, r: &Round) {
        for (a, _) in &r.commits {
            if !r.reveals.iter().any(|x| &x.0 == a) {
                if let Some(mut st) = self.attesters.get(a).cloned() {
                    st.missed += 1;
                    self.attesters.insert(a.clone(), st);
                }
            }
        }
    }

    fn active_attesters(&self) -> usize {
        self.attesters.values().filter(|a| a.active).count()
    }

    fn round_view(&self, key: &str, r: &Round) -> RoundView {
        RoundView {
            key: key.to_string(),
            opened_at: r.opened_at,
            commit_deadline: self.commit_deadline(r),
            reveal_deadline: self.reveal_deadline(r),
            committed: r.commits.iter().map(|c| c.0.clone()).collect(),
            revealed: r
                .reveals
                .iter()
                .map(|(a, h)| (a.clone(), hex::encode(h)))
                .collect(),
            outcome: r.outcome.map(hex::encode),
            result: r.result.clone(),
        }
    }

    fn commit_one(
        &mut self,
        who: &AccountId,
        key: &str,
        commitment: &str,
        now: u64,
    ) -> Result<(), String> {
        let commitment = claim::parse_hex32(commitment)?;
        let kind = claim::key_kind(key).ok_or("unknown key")?;
        if self.done.contains_key(key) {
            return Err("already settled".into());
        }
        // A round can only be opened for something the contract is waiting on.
        match kind {
            Kind::Merge => {
                let repo = claim::merge_key_repo(key).ok_or("bad merge key")?;
                if !self.repos.contains_key(repo) {
                    return Err("repository is not enrolled".into());
                }
            }
            Kind::Link => {
                if !self.pending_links.contains_key(key) {
                    return Err("no such link request".into());
                }
            }
            Kind::Repo => {
                let repo = key.trim_start_matches("repo:");
                if !self.pending_repos.contains_key(repo) {
                    return Err("no such repository proposal".into());
                }
            }
        }
        let mut r = self.rounds.get(key).cloned().unwrap_or(Round {
            opened_at: now,
            commits: vec![],
            reveals: vec![],
            outcome: None,
            result: String::new(),
        });
        if now > self.commit_deadline(&r) {
            return Err("commit window closed".into());
        }
        if r.commits.iter().any(|c| &c.0 == who) {
            return Err("already committed".into());
        }
        r.commits.push((who.clone(), commitment));
        self.rounds.insert(key.to_string(), r);
        Ok(())
    }

    fn reveal_one(
        &mut self,
        who: &AccountId,
        key: &str,
        text: Option<&str>,
        salt: &str,
        now: u64,
    ) -> Result<String, String> {
        let salt = claim::parse_hex32(salt)?;
        let mut r = self.rounds.get(key).cloned().ok_or("no round")?;
        let committed = r
            .commits
            .iter()
            .find(|c| &c.0 == who)
            .map(|c| c.1)
            .ok_or("no commitment")?;
        if r.reveals.iter().any(|x| &x.0 == who) {
            return Err("already revealed".into());
        }
        let everyone_in = r.commits.len() >= self.active_attesters();
        if now <= self.commit_deadline(&r) && !everyone_in {
            return Err("reveal opens when the commit window closes".into());
        }
        if now > self.reveal_deadline(&r) {
            return Err("reveal window closed".into());
        }
        let hash = claim::claim_hash(text);
        if claim::commitment(who.as_str(), key, &hash, &salt) != committed {
            return Err("reveal does not match the commitment".into());
        }
        // A positive claim must be canonical and about this key.
        if let Some(t) = text {
            let c = claim::parse(t)?;
            if c.key() != key {
                return Err("claim is about a different key".into());
            }
        }
        r.reveals.push((who.clone(), hash));

        if let Some(outcome) = r.outcome {
            // Settled already: a late reveal either agrees or is slashed.
            let status = if hash == outcome {
                "agreed".to_string()
            } else {
                let winners: Vec<AccountId> = r
                    .reveals
                    .iter()
                    .filter(|x| x.1 == outcome)
                    .map(|x| x.0.clone())
                    .collect();
                self.slash(who, &winners, key);
                "slashed".to_string()
            };
            self.rounds.insert(key.to_string(), r);
            return Ok(status);
        }

        let agreeing: Vec<AccountId> = r
            .reveals
            .iter()
            .filter(|x| x.1 == hash)
            .map(|x| x.0.clone())
            .collect();
        if agreeing.len() < self.params.quorum as usize {
            self.rounds.insert(key.to_string(), r);
            return Ok("revealed".into());
        }

        // Quorum reached: settle, then slash everyone who revealed something else.
        r.outcome = Some(hash);
        let dissent: Vec<AccountId> = r
            .reveals
            .iter()
            .filter(|x| x.1 != hash)
            .map(|x| x.0.clone())
            .collect();
        let result = if hash == NONE_HASH {
            self.reject(key);
            "rejected".to_string()
        } else {
            let c = claim::parse(text.unwrap()).expect("parsed above");
            for a in &agreeing {
                if let Some(mut st) = self.attesters.get(a).cloned() {
                    st.settled += 1;
                    self.attesters.insert(a.clone(), st);
                }
            }
            self.settle(key, c, &agreeing)
        };
        // Only merges are final. A link or repository that failed a check may be
        // asked for again later, once it qualifies.
        if hash != NONE_HASH && key.starts_with("merge:") {
            self.done.insert(key.to_string(), DONE_SETTLED);
        }
        r.result = result.clone();
        self.rounds.insert(key.to_string(), r);
        for d in dissent {
            self.slash(&d, &agreeing, key);
        }
        Ok(result)
    }

    /// Takes `slash_bps` of the attester's bond and pays it to the attesters
    /// whose reveal settled the round.
    fn slash(&mut self, who: &AccountId, winners: &[AccountId], key: &str) {
        let Some(mut st) = self.attesters.get(who).cloned() else {
            return;
        };
        let cut = st.bond.0 * self.params.slash_bps as u128 / crate::mechanic::BPS;
        st.bond = U128(st.bond.0 - cut);
        st.slashes += 1;
        st.slashed = U128(st.slashed.0 + cut);
        if st.bond.0 < self.params.bond.0 {
            st.active = false;
        }
        self.attesters.insert(who.clone(), st);
        self.stats.slashed_near = U128(self.stats.slashed_near.0 + cut);
        let mut parts = vec![];
        if !winners.is_empty() && cut > 0 {
            let each = cut / winners.len() as u128;
            for w in winners {
                if each > 0 {
                    Promise::new(w.clone())
                        .transfer(NearToken::from_yoctonear(each))
                        .detach();
                }
                parts.push(crate::Part {
                    role: "honest attester (yoctoNEAR)".into(),
                    to: w.to_string(),
                    amount: U128(each),
                });
            }
        }
        self.push_log(
            "slash",
            key,
            0,
            0,
            parts,
            format!(
                "{who} lost {cut} yoctoNEAR of bond for a reveal that disagreed with the quorum"
            ),
        );
    }
}
