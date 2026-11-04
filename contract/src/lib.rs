//! Merged: an NEP-141 memecoin whose supply grows only when a pull request is
//! merged into a participating open source repository.
//!
//! A merge is proven by a fixed set of bonded attesters that commit to a hash
//! of what they read on GitHub, then reveal it. When a quorum reveals the same
//! claim, the contract mints and splits the reward. Every parameter is fixed in
//! `new` and there is no owner or admin method. See DESIGN.md.

use near_contract_standards::fungible_token::events::{FtBurn, FtMint};
use near_contract_standards::fungible_token::metadata::{
    FT_METADATA_SPEC, FungibleTokenMetadata, FungibleTokenMetadataProvider,
};
use near_contract_standards::fungible_token::{
    FungibleToken, FungibleTokenCore, FungibleTokenResolver,
};
use near_contract_standards::storage_management::{
    StorageBalance, StorageBalanceBounds, StorageManagement,
};
use near_sdk::json_types::U128;
use near_sdk::store::{IterableMap, LookupMap, Vector};
use near_sdk::{
    AccountId, BorshStorageKey, NearToken, PanicOnDefault, Promise, PromiseOrValue,
    assert_one_yocto, env, near, require,
};

pub mod claim;
mod effects;
pub mod mechanic;
mod rounds;
#[cfg(test)]
mod tests;

use mechanic::BPS;

#[derive(BorshStorageKey)]
#[near]
enum Key {
    Token,
    Attesters,
    Repos,
    PendingRepos,
    Links,
    AccountLinks,
    PendingLinks,
    Rounds,
    Done,
    RepoDay,
    AuthorDay,
    Streaks,
    Escrow,
    Guests,
    Log,
    RepoAuthors,
    Seen,
    RepoMinted,
    ExileBurns,
    Exiled,
}

/// Every rule of the token. Set once in `new`; there is no method that changes it.
#[near(serializers = [borsh, json])]
#[derive(Clone, Debug)]
pub struct Params {
    /// The attester set, fixed for the life of the token.
    pub attesters: Vec<AccountId>,
    /// Matching reveals needed to settle a round. Must be a strict majority.
    pub quorum: u8,
    /// NEAR each attester locks before it may commit (yoctoNEAR).
    pub bond: U128,
    pub commit_window_secs: u64,
    pub reveal_window_secs: u64,
    /// Wait between an attester announcing its exit and withdrawing its bond.
    pub exit_delay_secs: u64,
    /// Share of the bond taken for each reveal that disagrees with a settled round.
    pub slash_bps: u16,

    /// Reward for one merge in the first era (smallest units).
    pub base_reward: U128,
    /// The reward halves after this many minted merges.
    pub halving_every: u64,
    pub author_bps: u16,
    pub review_bps: u16,
    pub attester_bps: u16,
    pub feast_bps: u16,
    /// Approvers paid per merge; extra approvers are not paid.
    pub max_paid_reviewers: u8,
    pub streak_step_bps: u16,
    pub streak_max_bonus_bps: u16,

    /// Most merges a repository can mint in one UTC day (by merge time).
    pub repo_daily_cap: u32,
    /// Daily merges every enrolled repository may mint before diversity counts.
    pub repo_floor: u32,
    /// One more daily merge for every this many distinct aged authors the
    /// repository had in the current or the previous 30-day window.
    pub authors_per_slot: u32,
    /// Merges minted per author per UTC day.
    pub author_daily_cap: u32,
    /// Merges older than this when settled mint nothing.
    pub max_merge_age_secs: u64,
    /// A GitHub account younger than this cannot link, and its merges mint nothing.
    pub min_github_age_days: u32,
    pub min_repo_stars: u32,
    pub min_repo_age_days: u32,
    /// Tokens burned by whoever proposes a new repository.
    pub repo_enroll_burn: U128,
    /// Least amount that must be burned to exile a repository.
    pub exile_min: U128,

    pub feast_period_secs: u64,
    /// Share of the pot burned at each feast; the rest feeds the period's reviewers.
    pub feast_burn_bps: u16,
    pub feast_max_guests: u8,
    /// Escrow is burned when this long has passed since its first unclaimed coin.
    pub escrow_ttl_secs: u64,
    /// NEAR attached to a link request or repo proposal to pay for its storage (yoctoNEAR).
    pub request_deposit: U128,

    /// Repositories enrolled at launch (lower case owner/name).
    pub genesis_repos: Vec<String>,
}

#[near(serializers = [borsh, json])]
#[derive(Clone, Debug, Default)]
pub struct AttesterState {
    pub bond: U128,
    pub active: bool,
    pub exit_at: Option<u64>,
    pub settled: u32,
    pub slashes: u32,
    pub slashed: U128,
    pub earned: U128,
}

#[near(serializers = [borsh, json])]
#[derive(Clone, Debug)]
pub struct RepoInfo {
    pub enrolled_at: u64,
    pub stars: u64,
    pub genesis: bool,
}

#[near(serializers = [borsh, json])]
#[derive(Clone, Debug)]
pub struct Link {
    pub account: AccountId,
    pub login: String,
    pub linked_at: u64,
}

#[near(serializers = [borsh, json])]
#[derive(Clone, Debug)]
pub struct Request {
    pub by: AccountId,
    pub at: u64,
    pub login: String,
}

#[near(serializers = [borsh, json])]
#[derive(Clone, Debug)]
pub struct Escrow {
    pub login: String,
    pub amount: U128,
    /// When the oldest unclaimed coin in this escrow was minted.
    pub since: u64,
}

#[near(serializers = [borsh])]
#[derive(Clone, Debug)]
pub struct Round {
    pub opened_at: u64,
    pub commits: Vec<(AccountId, [u8; 32])>,
    pub reveals: Vec<(AccountId, [u8; 32])>,
    pub outcome: Option<[u8; 32]>,
    pub result: String,
}

/// One line of the public ledger: every mint, burn, slash and link.
#[near(serializers = [borsh, json])]
#[derive(Clone, Debug)]
pub struct Part {
    pub role: String,
    /// A NEAR account, or `github:<id>` for coins held in escrow.
    pub to: String,
    pub amount: U128,
}

#[near(serializers = [borsh, json])]
#[derive(Clone, Debug)]
pub struct LogEntry {
    pub seq: u64,
    pub at: u64,
    /// merge, capped, stale, rejected, link, enroll, offering, feast, escrow_eaten, slash, unregister
    pub kind: String,
    pub key: String,
    pub minted: U128,
    pub burned: U128,
    pub parts: Vec<Part>,
    pub note: String,
}

#[near(serializers = [borsh, json])]
#[derive(Clone, Debug, Default)]
pub struct Stats {
    pub merges_minted: u64,
    pub merges_capped: u64,
    pub merges_stale: u64,
    pub merges_young: u64,
    pub exiles: u64,
    pub rounds_rejected: u64,
    pub rounds_expired: u64,
    pub links: u64,
    pub repos: u64,
    pub total_minted: U128,
    pub total_burned: U128,
    pub withheld: U128,
    pub escrow_outstanding: U128,
    pub feast_pot: U128,
    pub feasts: u64,
    pub last_feast_at: u64,
    pub slashed_near: U128,
}

#[near(contract_state)]
#[derive(PanicOnDefault)]
pub struct Merged {
    token: FungibleToken,
    params: Params,
    genesis_at: u64,
    attesters: IterableMap<AccountId, AttesterState>,
    repos: IterableMap<String, RepoInfo>,
    pending_repos: IterableMap<String, Request>,
    links: LookupMap<u64, Link>,
    account_links: LookupMap<AccountId, u64>,
    pending_links: IterableMap<String, Request>,
    rounds: IterableMap<String, Round>,
    done: LookupMap<String, u8>,
    repo_day: LookupMap<String, u32>,
    author_day: LookupMap<String, u32>,
    streaks: LookupMap<u64, (u64, u32)>,
    escrow: IterableMap<u64, Escrow>,
    guests: IterableMap<u64, u32>,
    log: Vector<LogEntry>,
    repo_authors: LookupMap<String, u32>,
    seen: LookupMap<String, ()>,
    repo_minted: LookupMap<String, u128>,
    exile_burns: LookupMap<String, u128>,
    exiled: LookupMap<String, u64>,
    stats: Stats,
}

pub(crate) fn now_secs() -> u64 {
    env::block_timestamp() / 1_000_000_000
}

fn check_params(p: &Params) {
    let n = p.attesters.len();
    require!((1..=15).contains(&n), "between 1 and 15 attesters");
    let mut sorted = p.attesters.clone();
    sorted.sort();
    sorted.dedup();
    require!(sorted.len() == n, "attesters must be distinct");
    require!(
        (p.quorum as usize) * 2 > n && (p.quorum as usize) <= n,
        "quorum must be a strict majority of the attesters"
    );
    let split =
        p.author_bps as u32 + p.review_bps as u32 + p.attester_bps as u32 + p.feast_bps as u32;
    require!(
        split == BPS as u32,
        "author, review, attester and feast shares must add up to 10000"
    );
    require!(
        p.slash_bps as u128 <= BPS && p.feast_burn_bps as u128 <= BPS,
        "basis points above 10000"
    );
    require!(
        p.commit_window_secs > 0 && p.reveal_window_secs > 0,
        "windows must be positive"
    );
    require!(
        p.exit_delay_secs >= p.commit_window_secs + p.reveal_window_secs,
        "exit delay must cover a whole round"
    );
    require!(
        p.halving_every > 0 && p.feast_period_secs > 0,
        "halving and feast period must be positive"
    );
    require!(
        p.max_paid_reviewers <= 10 && p.feast_max_guests <= 50,
        "too many paid reviewers or guests"
    );
    require!(
        p.base_reward.0 > 0
            && p.repo_daily_cap > 0
            && p.author_daily_cap > 0
            && p.authors_per_slot > 0,
        "reward and caps must be positive"
    );
    require!(
        p.repo_floor <= p.repo_daily_cap,
        "the floor cannot exceed the daily cap"
    );
    for r in &p.genesis_repos {
        require!(
            claim::valid_repo(r),
            "genesis repos must be lower case owner/name"
        );
    }
}

#[near]
impl Merged {
    /// Launches the token. Everything about it is decided here, once.
    #[init]
    pub fn new(params: Params) -> Self {
        check_params(&params);
        let now = now_secs();
        let mut this = Self {
            token: FungibleToken::new(Key::Token),
            params: params.clone(),
            genesis_at: now,
            attesters: IterableMap::new(Key::Attesters),
            repos: IterableMap::new(Key::Repos),
            pending_repos: IterableMap::new(Key::PendingRepos),
            links: LookupMap::new(Key::Links),
            account_links: LookupMap::new(Key::AccountLinks),
            pending_links: IterableMap::new(Key::PendingLinks),
            rounds: IterableMap::new(Key::Rounds),
            done: LookupMap::new(Key::Done),
            repo_day: LookupMap::new(Key::RepoDay),
            author_day: LookupMap::new(Key::AuthorDay),
            streaks: LookupMap::new(Key::Streaks),
            escrow: IterableMap::new(Key::Escrow),
            guests: IterableMap::new(Key::Guests),
            log: Vector::new(Key::Log),
            repo_authors: LookupMap::new(Key::RepoAuthors),
            seen: LookupMap::new(Key::Seen),
            repo_minted: LookupMap::new(Key::RepoMinted),
            exile_burns: LookupMap::new(Key::ExileBurns),
            exiled: LookupMap::new(Key::Exiled),
            stats: Stats {
                last_feast_at: now,
                ..Default::default()
            },
        };
        this.token
            .internal_register_account(&env::current_account_id());
        for a in &params.attesters {
            this.attesters.insert(a.clone(), AttesterState::default());
        }
        for r in &params.genesis_repos {
            this.repos.insert(
                r.clone(),
                RepoInfo {
                    enrolled_at: now,
                    stars: 0,
                    genesis: true,
                },
            );
        }
        this.stats.repos = params.genesis_repos.len() as u64;
        this
    }

    // ---- attesters -------------------------------------------------------

    /// Locks the attester bond. Also registers the attester to receive tokens.
    #[payable]
    pub fn bond(&mut self) {
        let who = env::predecessor_account_id();
        let mut st = self
            .attesters
            .get(&who)
            .cloned()
            .unwrap_or_else(|| env::panic_str("not an attester"));
        require!(st.exit_at.is_none(), "this attester has exited");
        st.bond = U128(st.bond.0 + env::attached_deposit().as_yoctonear());
        require!(st.bond.0 >= self.params.bond.0, "attach at least the bond");
        st.active = true;
        if self.token.accounts.get(&who).is_none() {
            self.token.internal_register_account(&who);
        }
        self.attesters.insert(who, st);
    }

    /// Announces an exit. The attester stops committing at once and can withdraw
    /// its bond after the exit delay, so a late wrong reveal can still be slashed.
    pub fn start_exit(&mut self) {
        let who = env::predecessor_account_id();
        let mut st = self
            .attesters
            .get(&who)
            .cloned()
            .unwrap_or_else(|| env::panic_str("not an attester"));
        require!(st.exit_at.is_none(), "already exiting");
        st.exit_at = Some(now_secs());
        st.active = false;
        self.attesters.insert(who, st);
    }

    pub fn withdraw_bond(&mut self) -> Promise {
        let who = env::predecessor_account_id();
        let mut st = self
            .attesters
            .get(&who)
            .cloned()
            .unwrap_or_else(|| env::panic_str("not an attester"));
        let exit_at = st
            .exit_at
            .unwrap_or_else(|| env::panic_str("call start_exit first"));
        require!(
            now_secs() >= exit_at + self.params.exit_delay_secs,
            "the exit delay has not passed"
        );
        let amount = st.bond.0;
        require!(amount > 0, "nothing to withdraw");
        st.bond = U128(0);
        self.attesters.insert(who.clone(), st);
        Promise::new(who).transfer(NearToken::from_yoctonear(amount))
    }

    // ---- community -------------------------------------------------------

    /// Asks the attesters to link a GitHub account to the caller. The attesters
    /// look for a public gist by that account containing `merged-link:<caller>`.
    #[payable]
    pub fn request_link(&mut self, github_id: u64, login: String) {
        let who = env::predecessor_account_id();
        require!(
            env::attached_deposit().as_yoctonear() >= self.params.request_deposit.0,
            "attach the request deposit"
        );
        require!(
            !self.links.contains_key(&github_id),
            "this GitHub account is already linked"
        );
        require!(
            !self.account_links.contains_key(&who),
            "this NEAR account is already linked"
        );
        require!(
            !login.is_empty()
                && login.len() <= 39
                && login.chars().all(|c| c.is_ascii_alphanumeric() || c == '-'),
            "bad login"
        );
        if self.token.accounts.get(&who).is_none() {
            self.token.internal_register_account(&who);
        }
        let key = claim::link_key(github_id, &who);
        require!(!self.pending_links.contains_key(&key), "already requested");
        self.pending_links.insert(
            key,
            Request {
                by: who,
                at: now_secs(),
                login,
            },
        );
    }

    /// Proposes a repository. The caller burns `repo_enroll_burn` tokens as an
    /// offering whether or not the attesters find it eligible.
    #[payable]
    pub fn propose_repo(&mut self, repo: String) {
        let who = env::predecessor_account_id();
        require!(
            env::attached_deposit().as_yoctonear() >= self.params.request_deposit.0,
            "attach the request deposit"
        );
        require!(
            claim::valid_repo(&repo),
            "repo must be lower case owner/name"
        );
        require!(!self.repos.contains_key(&repo), "already enrolled");
        require!(
            !self.exiled.contains_key(&repo),
            "this repository was exiled"
        );
        require!(!self.pending_repos.contains_key(&repo), "already proposed");
        let fee = self.params.repo_enroll_burn.0;
        if fee > 0 {
            self.token.internal_withdraw(&who, fee);
            FtBurn {
                owner_id: &who,
                amount: U128(fee),
                memo: Some("repo offering"),
            }
            .emit();
            self.stats.total_burned = U128(self.stats.total_burned.0 + fee);
        }
        self.push_log(
            "offering",
            &claim::repo_key(&repo),
            0,
            fee,
            vec![Part {
                role: "proposer".into(),
                to: who.to_string(),
                amount: U128(fee),
            }],
            String::new(),
        );
        self.pending_repos.insert(
            repo,
            Request {
                by: who,
                at: now_secs(),
                login: String::new(),
            },
        );
    }

    // ---- views -----------------------------------------------------------

    pub fn params(&self) -> &Params {
        &self.params
    }
    pub fn genesis_at(&self) -> u64 {
        self.genesis_at
    }
    pub fn stats(&self) -> Stats {
        self.stats.clone()
    }
    pub fn total_supply(&self) -> U128 {
        U128(self.token.total_supply)
    }
    pub fn attesters(&self) -> Vec<(AccountId, AttesterState)> {
        self.attesters
            .iter()
            .map(|(a, s)| (a.clone(), s.clone()))
            .collect()
    }
    pub fn repos(&self) -> Vec<(String, RepoInfo)> {
        self.repos
            .iter()
            .map(|(a, s)| (a.clone(), s.clone()))
            .collect()
    }
    pub fn pending_repos(&self) -> Vec<(String, Request)> {
        self.pending_repos
            .iter()
            .map(|(a, s)| (a.clone(), s.clone()))
            .collect()
    }
    pub fn pending_links(&self) -> Vec<(String, Request)> {
        self.pending_links
            .iter()
            .map(|(a, s)| (a.clone(), s.clone()))
            .collect()
    }
    pub fn link_of(&self, github_id: u64) -> Option<Link> {
        self.links.get(&github_id).cloned()
    }
    pub fn github_of(&self, account_id: AccountId) -> Option<u64> {
        self.account_links.get(&account_id).copied()
    }
    pub fn escrow_of(&self, github_id: u64) -> Option<Escrow> {
        self.escrow.get(&github_id).cloned()
    }
    pub fn escrows(&self, from_index: Option<u32>, limit: Option<u32>) -> Vec<(u64, Escrow)> {
        let from = from_index.unwrap_or(0) as usize;
        let limit = limit.unwrap_or(50).min(200) as usize;
        self.escrow
            .iter()
            .skip(from)
            .take(limit)
            .map(|(k, v)| (*k, v.clone()))
            .collect()
    }
    /// (last week index, consecutive weeks) for a GitHub account.
    pub fn streak_of(&self, github_id: u64) -> Option<(u64, u32)> {
        self.streaks.get(&github_id).copied()
    }
    pub fn is_done(&self, key: String) -> bool {
        self.done.contains_key(&key)
    }
    pub fn repo_day_count(&self, repo: String, day: u64) -> u32 {
        self.repo_day
            .get(&format!("{repo}|{day}"))
            .copied()
            .unwrap_or(0)
    }
    /// Merges `repo` may mint on the UTC day containing `at` (unix seconds).
    pub fn repo_cap(&self, repo: String, at: u64) -> u32 {
        self.repo_cap_at(&repo, at)
    }
    /// Tokens still to burn to exile `repo` right now.
    pub fn exile_cost(&self, repo: String) -> U128 {
        U128(self.exile_need(&repo, now_secs()))
    }
    pub fn exiled_at(&self, repo: String) -> Option<u64> {
        self.exiled.get(&repo).copied()
    }
    pub fn guests(&self) -> Vec<(u64, u32)> {
        self.guests.iter().map(|(k, v)| (*k, *v)).collect()
    }
    pub fn log_len(&self) -> u64 {
        self.log.len() as u64
    }
    /// Ledger entries starting at `from_index`, oldest first.
    pub fn log(&self, from_index: u64, limit: Option<u64>) -> Vec<LogEntry> {
        let limit = limit.unwrap_or(50).min(200);
        let end = (from_index + limit).min(self.log.len() as u64);
        (from_index..end)
            .filter_map(|i| self.log.get(i as u32).cloned())
            .collect()
    }
}

// ---- NEP-141, NEP-145, NEP-148 -------------------------------------------

#[near]
impl FungibleTokenCore for Merged {
    #[payable]
    fn ft_transfer(&mut self, receiver_id: AccountId, amount: U128, memo: Option<String>) {
        self.token.ft_transfer(receiver_id, amount, memo)
    }

    #[payable]
    fn ft_transfer_call(
        &mut self,
        receiver_id: AccountId,
        amount: U128,
        memo: Option<String>,
        msg: String,
    ) -> PromiseOrValue<U128> {
        self.token.ft_transfer_call(receiver_id, amount, memo, msg)
    }

    fn ft_total_supply(&self) -> U128 {
        self.token.ft_total_supply()
    }

    fn ft_balance_of(&self, account_id: AccountId) -> U128 {
        self.token.ft_balance_of(account_id)
    }
}

#[near]
impl FungibleTokenResolver for Merged {
    #[private]
    fn ft_resolve_transfer(
        &mut self,
        sender_id: AccountId,
        receiver_id: AccountId,
        amount: U128,
    ) -> U128 {
        let (used, burned) =
            self.token
                .internal_ft_resolve_transfer(&sender_id, receiver_id, amount);
        if burned > 0 {
            self.stats.total_burned = U128(self.stats.total_burned.0 + burned);
            self.push_log(
                "unregister",
                "",
                0,
                burned,
                vec![Part {
                    role: "sender".into(),
                    to: sender_id.to_string(),
                    amount: U128(burned),
                }],
                "sender unregistered during a transfer".into(),
            );
        }
        used.into()
    }
}

#[near]
impl StorageManagement for Merged {
    #[payable]
    fn storage_deposit(
        &mut self,
        account_id: Option<AccountId>,
        registration_only: Option<bool>,
    ) -> StorageBalance {
        self.token.storage_deposit(account_id, registration_only)
    }

    #[payable]
    fn storage_withdraw(&mut self, amount: Option<NearToken>) -> StorageBalance {
        self.token.storage_withdraw(amount)
    }

    #[payable]
    fn storage_unregister(&mut self, force: Option<bool>) -> bool {
        let who = env::predecessor_account_id();
        require!(
            !self.attesters.contains_key(&who) && !self.account_links.contains_key(&who),
            "attesters and linked accounts cannot unregister"
        );
        assert_one_yocto();
        match self.token.internal_storage_unregister(force) {
            Some((account, balance)) => {
                if balance > 0 {
                    FtBurn {
                        owner_id: &account,
                        amount: U128(balance),
                        memo: Some("unregistered"),
                    }
                    .emit();
                    self.stats.total_burned = U128(self.stats.total_burned.0 + balance);
                    self.push_log(
                        "unregister",
                        "",
                        0,
                        balance,
                        vec![Part {
                            role: "holder".into(),
                            to: account.to_string(),
                            amount: U128(balance),
                        }],
                        String::new(),
                    );
                }
                true
            }
            None => false,
        }
    }

    fn storage_balance_bounds(&self) -> StorageBalanceBounds {
        self.token.storage_balance_bounds()
    }

    fn storage_balance_of(&self, account_id: AccountId) -> Option<StorageBalance> {
        self.token.storage_balance_of(account_id)
    }
}

const ICON: &str = "data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 32 32'%3E%3Crect width='32' height='32' rx='8' fill='%232563EB'/%3E%3Cg fill='none' stroke='%23fff' stroke-width='2.6' stroke-linecap='round'%3E%3Ccircle cx='10' cy='8' r='2.6'/%3E%3Ccircle cx='10' cy='24' r='2.6'/%3E%3Ccircle cx='22' cy='17' r='2.6' stroke='%23C4B5FD'/%3E%3Cpath d='M10 10.6v10.8M10 12c0 4 4 5 9.4 5'/%3E%3C/g%3E%3C/svg%3E";

#[near]
impl FungibleTokenMetadataProvider for Merged {
    fn ft_metadata(&self) -> FungibleTokenMetadata {
        FungibleTokenMetadata {
            spec: FT_METADATA_SPEC.to_string(),
            name: "Merged".to_string(),
            symbol: "MERGED".to_string(),
            icon: Some(ICON.to_string()),
            reference: None,
            reference_hash: None,
            decimals: 18,
        }
    }
}

impl Merged {
    pub(crate) fn push_log(
        &mut self,
        kind: &str,
        key: &str,
        minted: u128,
        burned: u128,
        parts: Vec<Part>,
        note: String,
    ) {
        let seq = self.log.len() as u64;
        self.log.push(LogEntry {
            seq,
            at: now_secs(),
            kind: kind.to_string(),
            key: key.to_string(),
            minted: U128(minted),
            burned: U128(burned),
            parts,
            note,
        });
    }

    /// Mints to an account that is known to be registered.
    pub(crate) fn mint_to(&mut self, account: &AccountId, amount: u128, memo: &str) {
        if amount == 0 {
            return;
        }
        self.token.internal_deposit(account, amount);
        FtMint {
            owner_id: account,
            amount: U128(amount),
            memo: Some(memo),
        }
        .emit();
        self.stats.total_minted = U128(self.stats.total_minted.0 + amount);
    }

    /// Burns from the contract's own balance (escrow and feast pot live there).
    pub(crate) fn burn_own(&mut self, amount: u128, memo: &str) {
        if amount == 0 {
            return;
        }
        let me = env::current_account_id();
        self.token.internal_withdraw(&me, amount);
        FtBurn {
            owner_id: &me,
            amount: U128(amount),
            memo: Some(memo),
        }
        .emit();
        self.stats.total_burned = U128(self.stats.total_burned.0 + amount);
    }
}
