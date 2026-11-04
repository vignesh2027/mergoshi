use crate::claim::{self, NONE_HASH};
use crate::rounds::{CommitItem, RevealItem};
use crate::*;
use near_contract_standards::fungible_token::FungibleTokenCore;
use near_sdk::test_utils::VMContextBuilder;
use near_sdk::testing_env;

const E18: u128 = 1_000_000_000_000_000_000;
const T0: u64 = 1_793_000_000; // 2026-10-25, a Sunday in UTC
const NEAR: u128 = 1_000_000_000_000_000_000_000_000;

fn acc(s: &str) -> AccountId {
    s.parse().unwrap()
}

fn params() -> Params {
    Params {
        attesters: vec![acc("a1.near"), acc("a2.near"), acc("a3.near")],
        quorum: 2,
        bond: U128(NEAR),
        commit_window_secs: 60,
        reveal_window_secs: 120,
        exit_delay_secs: 600,
        slash_bps: 2000,
        base_reward: U128(100 * E18),
        halving_every: 1000,
        author_bps: 7000,
        review_bps: 2000,
        attester_bps: 500,
        feast_bps: 500,
        max_paid_reviewers: 3,
        streak_step_bps: 1000,
        streak_max_bonus_bps: 4000,
        repo_daily_cap: 12,
        repo_floor: 2,
        authors_per_slot: 3,
        author_daily_cap: 5,
        max_merge_age_secs: 7 * 86_400,
        min_github_age_days: 90,
        min_repo_stars: 100,
        min_repo_age_days: 180,
        repo_enroll_burn: U128(50 * E18),
        exile_min: U128(50 * E18),
        feast_period_secs: 86_400,
        feast_burn_bps: 5000,
        feast_max_guests: 10,
        escrow_ttl_secs: 30 * 86_400,
        request_deposit: U128(NEAR / 100),
        genesis_repos: vec![
            "prometheus/client_golang".into(),
            "helm/chart-testing".into(),
        ],
    }
}

struct T {
    c: Mergoshi,
    now: u64,
}

fn ctx(who: &str, now: u64, deposit: u128) {
    let mut b = VMContextBuilder::new();
    b.current_account_id(acc("mergoshi.near"))
        .predecessor_account_id(acc(who))
        .signer_account_id(acc(who))
        .block_timestamp(now * 1_000_000_000)
        .attached_deposit(NearToken::from_yoctonear(deposit))
        .account_balance(NearToken::from_near(100));
    testing_env!(b.build());
}

impl T {
    fn new() -> Self {
        Self::with(params())
    }
    fn with(p: Params) -> Self {
        ctx("deployer.near", T0, 0);
        let mut c = Mergoshi::new(p);
        for a in ["a1.near", "a2.near", "a3.near"] {
            ctx(a, T0, NEAR);
            c.bond();
        }
        T { c, now: T0 }
    }
    fn at(&mut self, who: &str, deposit: u128) -> &mut Mergoshi {
        ctx(who, self.now, deposit);
        &mut self.c
    }
    fn salt(who: &str) -> [u8; 32] {
        let mut s = [0u8; 32];
        s[..who.len().min(32)].copy_from_slice(&who.as_bytes()[..who.len().min(32)]);
        s
    }
    fn commit(&mut self, who: &str, key: &str, text: Option<&str>) -> String {
        let h = claim::claim_hash(text);
        let cm = claim::commitment(who, key, &h, &Self::salt(who));
        self.at(who, 0)
            .commit(vec![CommitItem {
                key: key.into(),
                commitment: hex::encode(cm),
            }])
            .remove(0)
    }
    fn reveal(&mut self, who: &str, key: &str, text: Option<&str>) -> String {
        let salt = hex::encode(Self::salt(who));
        self.at(who, 0)
            .reveal(vec![RevealItem {
                key: key.into(),
                claim: text.map(String::from),
                salt,
            }])
            .remove(0)
    }
    /// All three attesters commit and reveal the same text; returns the settling status.
    fn attest(&mut self, key: &str, text: Option<&str>) -> String {
        for a in ["a1.near", "a2.near", "a3.near"] {
            assert_eq!(self.commit(a, key, text), "ok");
        }
        assert_eq!(self.reveal("a1.near", key, text), "revealed");
        let s = self.reveal("a2.near", key, text);
        assert_eq!(self.reveal("a3.near", key, text), "agreed");
        s
    }
    fn bal(&mut self, who: &str) -> u128 {
        self.at("anyone.near", 0).ft_balance_of(acc(who)).0
    }
    /// Supply is everything minted minus everything burned, and the contract's
    /// own balance is exactly escrow plus the feast pot.
    fn check_books(&mut self) {
        let s = self.c.stats();
        assert_eq!(self.c.total_supply().0, s.total_minted.0 - s.total_burned.0);
        let own = self.bal("mergoshi.near");
        assert_eq!(own, s.escrow_outstanding.0 + s.feast_pot.0);
    }
}

fn merge_text(repo: &str, n: u64, at: u64, author: u64, login: &str, approvers: &str) -> String {
    merge_text_aged(repo, n, at, author, login, 1_400_000_000, approvers)
}

fn merge_text_aged(
    repo: &str,
    n: u64,
    at: u64,
    author: u64,
    login: &str,
    created: u64,
    approvers: &str,
) -> String {
    format!(
        "merge\n{repo}\n{n}\n{}\n{at}\n{author}\n{login}\n{created}\n{approvers}",
        "ab".repeat(20)
    )
}

#[test]
#[should_panic(expected = "strict majority")]
fn quorum_must_be_a_majority() {
    ctx("deployer.near", T0, 0);
    let mut p = params();
    p.quorum = 1;
    Mergoshi::new(p);
}

#[test]
#[should_panic(expected = "add up to 10000")]
fn split_must_add_up() {
    ctx("deployer.near", T0, 0);
    let mut p = params();
    p.feast_bps = 600;
    Mergoshi::new(p);
}

#[test]
fn merge_mints_and_splits_into_escrow() {
    let mut t = T::new();
    let text = merge_text(
        "prometheus/client_golang",
        2182,
        T0 - 3600,
        101,
        "vignesh2027",
        "7:beorn7,99:kakkoyun",
    );
    let key = "merge:prometheus/client_golang#2182";
    assert_eq!(t.attest(key, Some(&text)), "minted");

    let s = t.c.stats();
    assert_eq!(s.merges_minted, 1);
    // 70 to the author, 10 to each approver (all in escrow), 5 shared by the two
    // attesters that settled, 5 to the pot.
    assert_eq!(t.c.escrow_of(101).unwrap().amount.0, 70 * E18);
    assert_eq!(t.c.escrow_of(7).unwrap().amount.0, 10 * E18);
    assert_eq!(t.bal("a1.near"), 25 * E18 / 10);
    assert_eq!(t.bal("a2.near"), 25 * E18 / 10);
    assert_eq!(
        t.bal("a3.near"),
        0,
        "the late but agreeing attester earns nothing"
    );
    assert_eq!(s.feast_pot.0, 5 * E18);
    assert_eq!(t.c.total_supply().0, 100 * E18);
    assert!(t.c.is_done(key.into()));
    t.check_books();

    let log = t.c.log(0, None);
    assert_eq!(log.last().unwrap().kind, "merge");
    assert_eq!(log.last().unwrap().minted.0, 100 * E18);

    // Settled keys cannot be minted twice.
    assert_eq!(t.commit("a1.near", key, Some(&text)), "already settled");
}

#[test]
fn unreviewed_merge_withholds_the_review_share() {
    let mut t = T::new();
    let text = merge_text("helm/chart-testing", 873, T0 - 60, 101, "vignesh2027", "");
    assert_eq!(
        t.attest("merge:helm/chart-testing#873", Some(&text)),
        "minted"
    );
    assert_eq!(t.c.total_supply().0, 80 * E18);
    assert_eq!(t.c.stats().withheld.0, 20 * E18);
    t.check_books();
}

#[test]
fn dissenting_reveal_is_slashed_and_paid_to_the_quorum() {
    let mut t = T::new();
    let key = "merge:prometheus/client_golang#1";
    let good = merge_text("prometheus/client_golang", 1, T0 - 60, 5, "alice", "");
    let bad = merge_text("prometheus/client_golang", 1, T0 - 60, 666, "mallory", "");
    t.commit("a1.near", key, Some(&good));
    t.commit("a2.near", key, Some(&good));
    t.commit("a3.near", key, Some(&bad));
    assert_eq!(t.reveal("a3.near", key, Some(&bad)), "revealed");
    assert_eq!(t.reveal("a1.near", key, Some(&good)), "revealed");
    assert_eq!(t.reveal("a2.near", key, Some(&good)), "minted");
    let a3 =
        t.c.attesters()
            .into_iter()
            .find(|a| a.0.as_str() == "a3.near")
            .unwrap()
            .1;
    assert_eq!(a3.slashes, 1);
    assert_eq!(a3.bond.0, NEAR * 8 / 10);
    assert!(
        !a3.active,
        "a slashed attester below the bond must top up before committing again"
    );
    assert!(t.c.escrow_of(5).is_some() && t.c.escrow_of(666).is_none());
    assert_eq!(t.c.log(0, None).last().unwrap().kind, "slash");
}

#[test]
fn quorum_of_none_rejects_a_fake_merge_without_burning_the_key() {
    let mut t = T::new();
    let key = "merge:prometheus/client_golang#77";
    let fake = merge_text("prometheus/client_golang", 77, T0 - 60, 666, "mallory", "");
    t.commit("a1.near", key, Some(&fake));
    t.commit("a2.near", key, None);
    t.commit("a3.near", key, None);
    t.reveal("a1.near", key, Some(&fake));
    t.reveal("a2.near", key, None);
    assert_eq!(t.reveal("a3.near", key, None), "rejected");
    assert_eq!(t.c.total_supply().0, 0);
    assert_eq!(t.c.stats().rounds_rejected, 1);
    let a1 =
        t.c.attesters()
            .into_iter()
            .find(|a| a.0.as_str() == "a1.near")
            .unwrap()
            .1;
    assert_eq!(a1.slashes, 1);
    // The pull request may still be merged for real later, so the key stays open.
    assert!(!t.c.is_done(key.into()));
    t.now += 400;
    assert_eq!(
        t.at("anyone.near", 0).sweep(vec![key.into()]),
        vec!["closed".to_string()]
    );
}

#[test]
fn reveal_waits_for_the_window_unless_everyone_committed() {
    let mut t = T::new();
    let key = "merge:prometheus/client_golang#5";
    let text = merge_text("prometheus/client_golang", 5, T0 - 60, 5, "alice", "");
    t.commit("a1.near", key, Some(&text));
    t.commit("a2.near", key, Some(&text));
    assert_eq!(
        t.reveal("a1.near", key, Some(&text)),
        "reveal opens when the commit window closes"
    );
    t.now += 61;
    assert_eq!(
        t.commit("a3.near", key, Some(&text)),
        "commit window closed"
    );
    assert_eq!(t.reveal("a1.near", key, Some(&text)), "revealed");
    assert_eq!(t.reveal("a2.near", key, Some(&text)), "minted");
}

#[test]
fn commitments_are_checked() {
    let mut t = T::new();
    let key = "merge:prometheus/client_golang#6";
    let text = merge_text("prometheus/client_golang", 6, T0 - 60, 5, "alice", "");
    for a in ["a1.near", "a2.near", "a3.near"] {
        t.commit(a, key, Some(&text));
    }
    // Revealing other text than was committed fails.
    let other = merge_text("prometheus/client_golang", 6, T0 - 60, 6, "bob", "");
    assert_eq!(
        t.reveal("a1.near", key, Some(&other)),
        "reveal does not match the commitment"
    );
    // Claims about another key are refused even if committed.
    let wrong_key = "merge:prometheus/client_golang#7";
    assert_eq!(t.commit("a1.near", wrong_key, Some(&text)), "ok");
    t.now += 61;
    assert_eq!(
        t.reveal("a1.near", wrong_key, Some(&text)),
        "claim is about a different key"
    );
    assert_eq!(
        t.commit("a1.near", "merge:someone/else#1", Some(&text)),
        "repository is not enrolled"
    );
    assert_eq!(
        t.commit("a1.near", "link:1:x.near", None),
        "no such link request"
    );
}

#[test]
#[should_panic(expected = "only a bonded, active attester can commit")]
fn strangers_cannot_commit() {
    let mut t = T::new();
    t.commit("mallory.near", "merge:prometheus/client_golang#1", None);
}

#[test]
fn caps_grow_with_distinct_authors() {
    let mut p = params();
    p.repo_floor = 1;
    p.authors_per_slot = 3;
    let mut t = T::with(p);
    // Author 11 opens the day; 12 is capped (one slot); 13 is the third distinct
    // author, which opens a second slot; 11 again is capped.
    let authors = [11u64, 12, 13, 11];
    let want = ["minted", "capped", "minted", "capped"];
    for (i, a) in authors.iter().enumerate() {
        let n = i as u64 + 1;
        let text = merge_text("prometheus/client_golang", n, T0 - 60, *a, "dev", "");
        assert_eq!(
            t.attest(&format!("merge:prometheus/client_golang#{n}"), Some(&text)),
            want[i]
        );
    }
    assert_eq!(t.c.repo_cap("prometheus/client_golang".into(), T0 - 60), 2);
}

#[test]
fn author_cap_age_and_freshness() {
    let mut p = params();
    p.author_daily_cap = 1;
    let mut t = T::with(p);
    let a = merge_text("prometheus/client_golang", 1, T0 - 60, 5, "alice", "");
    let b = merge_text("prometheus/client_golang", 2, T0 - 50, 5, "alice", "");
    assert_eq!(
        t.attest("merge:prometheus/client_golang#1", Some(&a)),
        "minted"
    );
    assert_eq!(
        t.attest("merge:prometheus/client_golang#2", Some(&b)),
        "capped"
    );
    let old = merge_text("helm/chart-testing", 1, T0 - 8 * 86_400, 6, "bob", "");
    assert_eq!(t.attest("merge:helm/chart-testing#1", Some(&old)), "stale");
    let future = merge_text("helm/chart-testing", 2, T0 + 3600, 6, "bob", "");
    assert_eq!(
        t.attest("merge:helm/chart-testing#2", Some(&future)),
        "stale"
    );
    let young = merge_text_aged(
        "helm/chart-testing",
        3,
        T0 - 60,
        7,
        "fresh",
        T0 - 30 * 86_400,
        "",
    );
    assert_eq!(
        t.attest("merge:helm/chart-testing#3", Some(&young)),
        "young"
    );
    let s = t.c.stats();
    assert_eq!(
        (
            s.merges_minted,
            s.merges_capped,
            s.merges_stale,
            s.merges_young
        ),
        (1, 1, 2, 1)
    );
}

#[test]
fn streaks_raise_the_reward() {
    let mut p = params();
    p.max_merge_age_secs = 30 * 86_400;
    let mut t = T::with(p);
    // Three merges by the same author in three consecutive weeks.
    for (i, wk) in [2u64, 1, 0].iter().enumerate() {
        let at = T0 - wk * 7 * 86_400 - 60;
        let text = merge_text("helm/chart-testing", i as u64 + 1, at, 5, "alice", "");
        t.attest(&format!("merge:helm/chart-testing#{}", i + 1), Some(&text));
    }
    // Rewards 100, 110, 120 with the review share withheld: author gets 70, 77, 84.
    assert_eq!(t.c.escrow_of(5).unwrap().amount.0, 231 * E18);
    assert_eq!(t.c.streak_of(5).unwrap().1, 3);
}

fn link(t: &mut T, gid: u64, login: &str, who: &str, created: u64) -> String {
    t.at(who, NEAR / 100).request_link(gid, login.into());
    let text = format!("link\n{gid}\n{login}\n{who}\n{created}");
    t.attest(&format!("link:{gid}:{who}"), Some(&text))
}

#[test]
fn linking_releases_escrow_and_later_merges_mint_directly() {
    let mut t = T::new();
    let text = merge_text(
        "prometheus/client_golang",
        1,
        T0 - 60,
        101,
        "vignesh2027",
        "7:beorn7",
    );
    t.attest("merge:prometheus/client_golang#1", Some(&text));
    assert_eq!(
        link(
            &mut t,
            101,
            "vignesh2027",
            "vignesh.near",
            T0 - 400 * 86_400
        ),
        "linked"
    );
    assert_eq!(t.bal("vignesh.near"), 70 * E18);
    assert!(t.c.escrow_of(101).is_none());
    assert_eq!(t.c.github_of(acc("vignesh.near")), Some(101));
    t.check_books();

    let text = merge_text(
        "prometheus/client_golang",
        2,
        T0 - 30,
        101,
        "vignesh2027",
        "",
    );
    t.attest("merge:prometheus/client_golang#2", Some(&text));
    assert_eq!(t.bal("vignesh.near"), 140 * E18);
    t.check_books();
}

#[test]
fn young_github_accounts_cannot_link() {
    let mut t = T::new();
    assert_eq!(
        link(&mut t, 9, "fresh", "fresh.near", T0 - 10 * 86_400),
        "too young"
    );
    assert!(t.c.link_of(9).is_none());
    assert!(t.c.pending_links().is_empty());
}

#[test]
fn link_must_match_the_requesting_account() {
    let mut t = T::new();
    t.at("alice.near", NEAR / 100)
        .request_link(9, "alice".into());
    // A claim naming another account answers another key, so it cannot settle this one.
    let text = "link\n9\nalice\nmallory.near\n1000";
    assert_eq!(t.commit("a1.near", "link:9:alice.near", Some(text)), "ok");
    t.now += 61;
    assert_eq!(
        t.reveal("a1.near", "link:9:alice.near", Some(text)),
        "claim is about a different key"
    );
}

#[test]
fn proposing_a_repo_burns_the_offering() {
    let mut t = T::new();
    for n in 1..=2u64 {
        let text = merge_text(
            "prometheus/client_golang",
            n,
            T0 - 60,
            101,
            "vignesh2027",
            "",
        );
        t.attest(&format!("merge:prometheus/client_golang#{n}"), Some(&text));
    }
    link(&mut t, 101, "vignesh2027", "vignesh.near", 1);
    let before = t.c.total_supply().0;
    t.at("vignesh.near", NEAR / 100)
        .propose_repo("datadog/orchestrion".into());
    assert_eq!(t.c.total_supply().0, before - 50 * E18);
    let ok = format!(
        "repo\ndatadog/orchestrion\n42\n625\n{}\n0",
        T0 - 400 * 86_400
    );
    assert_eq!(t.attest("repo:datadog/orchestrion", Some(&ok)), "enrolled");
    assert!(t.c.repos().iter().any(|r| r.0 == "datadog/orchestrion"));

    t.at("vignesh.near", NEAR / 100)
        .propose_repo("tiny/new".into());
    let small = format!("repo\ntiny/new\n43\n3\n{}\n0", T0 - 10 * 86_400);
    assert_eq!(t.attest("repo:tiny/new", Some(&small)), "not eligible");
    assert!(!t.c.repos().iter().any(|r| r.0 == "tiny/new"));
    t.check_books();
}

#[test]
fn feast_burns_half_and_feeds_reviewers() {
    let mut t = T::new();
    link(&mut t, 7, "beorn7", "beorn.near", 1);
    for n in 1..=2u64 {
        let text = merge_text(
            "prometheus/client_golang",
            n,
            T0 - 60,
            101,
            "vignesh2027",
            "7:beorn7,8:other",
        );
        t.attest(&format!("merge:prometheus/client_golang#{n}"), Some(&text));
    }
    let pot = t.c.stats().feast_pot.0;
    assert_eq!(pot, 10 * E18);
    let reviewer_before = t.bal("beorn.near");
    t.now += 86_400;
    let burned = t.at("anyone.near", 0).feast().0;
    // Half burned, half served equally to two guests with two approvals each.
    assert_eq!(burned, 5 * E18);
    assert_eq!(t.bal("beorn.near") - reviewer_before, 25 * E18 / 10);
    assert_eq!(t.c.escrow_of(8).unwrap().amount.0, 20 * E18 + 25 * E18 / 10);
    assert_eq!(t.c.stats().feast_pot.0, 0);
    assert!(t.c.guests().is_empty());
    t.check_books();
}

#[test]
#[should_panic(expected = "the feast is not ready yet")]
fn feast_waits_for_its_period() {
    let mut t = T::new();
    t.at("anyone.near", 0).feast();
}

#[test]
fn unclaimed_escrow_burns_after_its_window() {
    let mut t = T::new();
    let text = merge_text("prometheus/client_golang", 1, T0 - 60, 101, "ghost", "");
    t.attest("merge:prometheus/client_golang#1", Some(&text));
    assert_eq!(t.at("anyone.near", 0).eat_stale_escrow(vec![101]).0, 0);
    // A later coin does not restart the clock: the window runs from the first one.
    t.now += 20 * 86_400;
    let text = merge_text("prometheus/client_golang", 2, t.now - 60, 101, "ghost", "");
    t.attest("merge:prometheus/client_golang#2", Some(&text));
    t.now += 11 * 86_400;
    assert_eq!(
        t.at("anyone.near", 0).eat_stale_escrow(vec![101]).0,
        140 * E18
    );
    // What is left: the attesters' 10 and the feast pot's 10.
    assert_eq!(t.c.total_supply().0, 20 * E18);
    assert_eq!(t.c.stats().feast_pot.0, 10 * E18);
    t.check_books();
}

/// One unreviewed merge in client_golang (80 minted) and two in chart-testing,
/// all by the same author, who then links and holds 210.
fn exile_setup() -> T {
    let mut t = T::new();
    let text = merge_text(
        "prometheus/client_golang",
        1,
        T0 - 60,
        101,
        "vignesh2027",
        "",
    );
    t.attest("merge:prometheus/client_golang#1", Some(&text));
    for n in 1..=2u64 {
        let text = merge_text("helm/chart-testing", n, T0 - 60, 101, "vignesh2027", "");
        t.attest(&format!("merge:helm/chart-testing#{n}"), Some(&text));
    }
    link(&mut t, 101, "vignesh2027", "vignesh.near", 1);
    t
}

#[test]
fn exile_ritual_needs_as_much_as_the_repo_minted() {
    let mut t = exile_setup();
    let repo = "prometheus/client_golang".to_string();
    assert_eq!(t.c.exile_cost(repo.clone()).0, 80 * E18);
    assert!(!t.at("vignesh.near", 1).exile(repo.clone(), U128(50 * E18)));
    assert_eq!(t.c.exile_cost(repo.clone()).0, 30 * E18);
    assert!(t.at("vignesh.near", 1).exile(repo.clone(), U128(30 * E18)));
    assert!(t.c.exiled_at(repo.clone()).is_some());
    assert_eq!(
        t.commit("a1.near", "merge:prometheus/client_golang#3", None),
        "repository is not enrolled"
    );
    assert_eq!(t.c.stats().exiles, 1);
    assert_eq!(t.bal("vignesh.near"), 130 * E18);
    t.check_books();
}

#[test]
#[should_panic(expected = "this repository was exiled")]
fn exiled_repos_cannot_return() {
    let mut t = exile_setup();
    t.at("vignesh.near", 1)
        .exile("prometheus/client_golang".into(), U128(80 * E18));
    t.at("vignesh.near", NEAR / 100)
        .propose_repo("prometheus/client_golang".into());
}

#[test]
#[should_panic(expected = "the exit delay has not passed")]
fn exit_needs_the_delay() {
    let mut t = T::new();
    t.at("a1.near", 0).start_exit();
    t.now += 100;
    let _ = t.at("a1.near", 0).withdraw_bond();
}

#[test]
fn unsettled_rounds_expire() {
    let mut t = T::new();
    let key = "merge:prometheus/client_golang#9";
    t.commit("a1.near", key, None);
    t.now += 200;
    assert_eq!(
        t.at("x.near", 0).sweep(vec![key.into()]),
        vec!["expired".to_string()]
    );
    assert!(t.c.round(key.into()).is_none());
    let _ = NONE_HASH;
}

#[test]
fn transfers_work_like_any_nep141_token() {
    let mut t = T::new();
    let text = merge_text(
        "prometheus/client_golang",
        1,
        T0 - 60,
        101,
        "vignesh2027",
        "",
    );
    t.attest("merge:prometheus/client_golang#1", Some(&text));
    link(&mut t, 101, "vignesh2027", "vignesh.near", 1);
    t.at("bob.near", NEAR / 100).storage_deposit(None, None);
    t.at("vignesh.near", 1)
        .ft_transfer(acc("bob.near"), U128(10 * E18), None);
    assert_eq!(t.bal("bob.near"), 10 * E18);
    assert_eq!(t.c.ft_metadata().symbol, "MRGO");
    t.check_books();
}

/// Replays sim/golden.json through real rounds and checks every total against
/// the Python model, so the simulation and the contract cannot drift apart.
#[test]
fn golden_replay_matches_the_simulation() {
    use near_sdk::serde_json::Value;
    let g: Value = near_sdk::serde_json::from_str(include_str!("../../sim/golden.json")).unwrap();
    let gp = &g["params"];
    let num = |v: &Value| -> u128 { v.as_str().map(|s| s.parse().unwrap()).unwrap_or_else(|| v.as_u64().unwrap() as u128) };
    let mut p = params();
    p.base_reward = U128(num(&gp["base_reward"]));
    p.halving_every = num(&gp["halving_every"]) as u64;
    p.author_bps = num(&gp["author_bps"]) as u16;
    p.review_bps = num(&gp["review_bps"]) as u16;
    p.attester_bps = num(&gp["attester_bps"]) as u16;
    p.feast_bps = num(&gp["feast_bps"]) as u16;
    p.max_paid_reviewers = num(&gp["max_paid_reviewers"]) as u8;
    p.streak_step_bps = num(&gp["streak_step_bps"]) as u16;
    p.streak_max_bonus_bps = num(&gp["streak_max_bonus_bps"]) as u16;
    p.repo_daily_cap = num(&gp["repo_daily_cap"]) as u32;
    p.repo_floor = num(&gp["repo_floor"]) as u32;
    p.authors_per_slot = num(&gp["authors_per_slot"]) as u32;
    p.author_daily_cap = num(&gp["author_daily_cap"]) as u32;
    p.min_github_age_days = num(&gp["min_github_age_days"]) as u32;
    p.max_merge_age_secs = num(&gp["max_merge_age_secs"]) as u64;
    p.feast_period_secs = num(&gp["feast_period_secs"]) as u64;
    p.feast_burn_bps = num(&gp["feast_burn_bps"]) as u16;
    p.feast_max_guests = num(&gp["feast_max_guests"]) as u8;
    p.escrow_ttl_secs = num(&gp["escrow_ttl_secs"]) as u64;
    p.genesis_repos = g["repos"].as_array().unwrap().iter().map(|r| r.as_str().unwrap().to_string()).collect();
    assert_eq!(g["t0"].as_u64().unwrap(), T0);
    let mut t = T::with(p);

    for e in g["events"].as_array().unwrap() {
        t.now = e["now"].as_u64().unwrap();
        match e["kind"].as_str().unwrap() {
            "link" => {
                let gid = e["gid"].as_u64().unwrap();
                let who = format!("g{gid}.near");
                assert_eq!(link(&mut t, gid, &format!("u{gid}"), &who, T0 - 400 * 86_400), "linked");
            }
            "merge" => {
                let repo = e["repo"].as_str().unwrap();
                let n = e["number"].as_u64().unwrap();
                let author = e["author"].as_u64().unwrap();
                let approvers: Vec<String> = e["approvers"].as_array().unwrap().iter().map(|a| format!("{0}:u{0}", a.as_u64().unwrap())).collect();
                let text = merge_text_aged(repo, n, e["merged_at"].as_u64().unwrap(), author, &format!("u{author}"), e["author_created_at"].as_u64().unwrap(), &approvers.join(","));
                let got = t.attest(&format!("merge:{repo}#{n}"), Some(&text));
                assert_eq!(got, e["status"].as_str().unwrap(), "merge {repo}#{n}");
            }
            "eat" => {
                let ids = e["ids"].as_array().unwrap().iter().map(|v| v.as_u64().unwrap()).collect();
                t.at("keeper.near", 0).eat_stale_escrow(ids);
            }
            "feast" => {
                t.at("keeper.near", 0).feast();
            }
            k => panic!("unknown event {k}"),
        }
    }

    let x = &g["expect"];
    let s = t.c.stats();
    assert_eq!(t.c.total_supply().0, num(&x["supply"]), "supply");
    assert_eq!(s.total_minted.0, num(&x["minted"]), "minted");
    assert_eq!(s.total_burned.0, num(&x["burned"]), "burned");
    assert_eq!(s.withheld.0, num(&x["withheld"]), "withheld");
    assert_eq!(s.feast_pot.0, num(&x["pot"]), "pot");
    assert_eq!(s.merges_minted, x["merges_minted"].as_u64().unwrap());
    assert_eq!(s.merges_capped, x["merges_capped"].as_u64().unwrap());
    assert_eq!(s.merges_stale, x["merges_stale"].as_u64().unwrap());
    assert_eq!(s.merges_young, x["merges_young"].as_u64().unwrap());
    for (gid, amount) in x["escrow"].as_object().unwrap() {
        assert_eq!(t.c.escrow_of(gid.parse().unwrap()).map(|e| e.amount.0).unwrap_or(0), num(amount), "escrow {gid}");
    }
    assert_eq!(t.c.escrows(None, Some(200)).len(), x["escrow"].as_object().unwrap().len());
    for (gid, amount) in x["linked_balances"].as_object().unwrap() {
        assert_eq!(t.bal(&format!("g{gid}.near")), num(amount), "balance of {gid}");
    }
    assert_eq!(t.bal("a1.near"), num(&x["attester_balance"]));
    t.check_books();
}
