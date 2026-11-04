//! Integration test: builds the wasm, deploys it to a local near-sandbox node
//! and runs real commit and reveal rounds, a slash, a link and a transfer.

use near_api::{AccountId, NearGas, NearToken, NetworkConfig, Signer};
use near_sdk::serde_json::{Value, json};
use sha2::{Digest, Sha256};
use std::sync::Arc;
use std::time::{SystemTime, UNIX_EPOCH};

const E18: u128 = 1_000_000_000_000_000_000;

struct World {
    network: NetworkConfig,
    signer: Arc<Signer>,
    token: near_api::Contract,
    _sandbox: near_sandbox::Sandbox,
}

async fn account(sandbox: &near_sandbox::Sandbox, name: &str) -> testresult::TestResult<AccountId> {
    let id: AccountId = name.parse()?;
    sandbox.create_account(id.clone()).initial_balance(NearToken::from_near(20)).send().await?;
    Ok(id)
}

fn sha(b: &[u8]) -> [u8; 32] {
    Sha256::digest(b).into()
}

fn commitment(attester: &str, key: &str, claim: Option<&str>, salt: [u8; 32]) -> String {
    let h = claim.map(|c| sha(c.as_bytes())).unwrap_or([0; 32]);
    let mut buf = Vec::new();
    buf.extend_from_slice(attester.as_bytes());
    buf.push(b'\n');
    buf.extend_from_slice(key.as_bytes());
    buf.push(b'\n');
    buf.extend_from_slice(&h);
    buf.extend_from_slice(&salt);
    hex(&sha(&buf))
}

fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

fn now() -> u64 {
    SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs()
}

impl World {
    async fn call(&self, who: &AccountId, method: &str, args: Value, yocto: u128) -> testresult::TestResult<near_api::types::transaction::result::TransactionResult> {
        Ok(self
            .token
            .call_function(method, args)
            .transaction()
            .gas(NearGas::from_tgas(300))
            .deposit(NearToken::from_yoctonear(yocto))
            .with_signer(who.clone(), self.signer.clone())
            .send_to(&self.network)
            .await?)
    }

    async fn view(&self, method: &str, args: Value) -> testresult::TestResult<Value> {
        Ok(self.token.call_function(method, args).read_only().fetch_from(&self.network).await?.data)
    }

    async fn u128(&self, method: &str, args: Value) -> testresult::TestResult<u128> {
        Ok(self.view(method, args).await?.as_str().unwrap().parse()?)
    }

    /// Each attester commits to its claim, then each reveals. Returns the reveal statuses.
    async fn round(&self, key: &str, votes: &[(&AccountId, Option<&str>)]) -> testresult::TestResult<Vec<String>> {
        for (who, claim) in votes {
            let c = commitment(who.as_str(), key, *claim, [9; 32]);
            let r = self.call(who, "commit", json!({ "items": [{ "key": key, "commitment": c }] }), 0).await?;
            let st: Vec<String> = r.into_result()?.json()?;
            assert_eq!(st, vec!["ok".to_string()], "commit by {who}");
        }
        let mut out = vec![];
        for (who, claim) in votes {
            let r = self.call(who, "reveal", json!({ "items": [{ "key": key, "claim": claim, "salt": hex(&[9; 32]) }] }), 0).await?;
            let st: Vec<String> = r.into_result()?.json()?;
            out.push(st[0].clone());
        }
        Ok(out)
    }
}

#[tokio::test]
async fn sandbox_end_to_end() -> testresult::TestResult<()> {
    let wasm = std::fs::read(cargo_near_build::build_with_cli(Default::default())?)?;
    let sandbox = near_sandbox::Sandbox::start_sandbox().await?;
    let network = NetworkConfig::from_rpc_url("sandbox", sandbox.rpc_addr.parse()?);
    let signer = Signer::from_secret_key(near_sandbox::config::DEFAULT_GENESIS_ACCOUNT_PRIVATE_KEY.parse()?)?;
    let a1 = account(&sandbox, "a1.sandbox").await?;
    let a2 = account(&sandbox, "a2.sandbox").await?;
    let a3 = account(&sandbox, "a3.sandbox").await?;
    let bob = account(&sandbox, "bob.sandbox").await?;
    let carol = account(&sandbox, "carol.sandbox").await?;
    let token_id = account(&sandbox, "token.sandbox").await?;

    let params = json!({
        "attesters": [a1, a2, a3], "quorum": 2, "bond": NearToken::from_near(1).as_yoctonear().to_string(),
        "commit_window_secs": 600, "reveal_window_secs": 600, "exit_delay_secs": 1200, "slash_bps": 2000,
        "base_reward": (100 * E18).to_string(), "halving_every": 10000,
        "author_bps": 7000, "review_bps": 2000, "attester_bps": 500, "feast_bps": 500,
        "max_paid_reviewers": 3, "streak_step_bps": 1000, "streak_max_bonus_bps": 4000,
        "repo_daily_cap": 12, "repo_floor": 2, "authors_per_slot": 3, "author_daily_cap": 4,
        "max_merge_age_secs": 604800, "min_github_age_days": 90, "min_repo_stars": 100, "min_repo_age_days": 180,
        "repo_enroll_burn": (500 * E18).to_string(), "exile_min": (1000 * E18).to_string(),
        "feast_period_secs": 2592000, "feast_burn_bps": 5000, "feast_max_guests": 20,
        "escrow_ttl_secs": 5184000, "request_deposit": NearToken::from_millinear(10).as_yoctonear().to_string(),
        "genesis_repos": ["datadog/go-sqllexer", "prometheus/client_golang"],
    });
    near_api::Contract::deploy(token_id.clone())
        .use_code(wasm)
        .with_init_call("new", json!({ "params": params }))?
        .with_signer(signer.clone())
        .send_to(&network)
        .await?
        .assert_success();
    let w = World { network, signer, token: near_api::Contract(token_id), _sandbox: sandbox };

    assert_eq!(w.view("ft_metadata", json!({})).await?["symbol"], "MRGO");
    for a in [&a1, &a2, &a3] {
        w.call(a, "bond", json!({}), NearToken::from_near(1).as_yoctonear()).await?.assert_success();
    }

    // A real merge: author 101 (unlinked, so escrowed) with one approver.
    let merged_at = now() - 120;
    let claim = format!(
        "merge\ndatadog/go-sqllexer\n120\n{}\n{merged_at}\n101\nvignesh2027\n1500000000\n7:reviewer",
        "ab".repeat(20)
    );
    let key = "merge:datadog/go-sqllexer#120";
    let st = w.round(key, &[(&a1, Some(&claim)), (&a2, Some(&claim)), (&a3, Some(&claim))]).await?;
    assert_eq!(st, vec!["revealed", "minted", "agreed"]);
    assert_eq!(w.u128("ft_total_supply", json!({})).await?, 100 * E18);
    assert_eq!(w.view("escrow_of", json!({ "github_id": 101 })).await?["amount"], (70 * E18).to_string());
    assert_eq!(w.u128("ft_balance_of", json!({ "account_id": a1 })).await?, 25 * E18 / 10);
    let log = w.view("log", json!({ "from_index": 0 })).await?;
    assert_eq!(log[0]["kind"], "merge");

    // A fake merge: the liar is outvoted by NONE and slashed.
    let fake = format!("merge\ndatadog/go-sqllexer\n999\n{}\n{merged_at}\n666\nmallory\n1500000000\n", "cd".repeat(20));
    let st = w.round("merge:datadog/go-sqllexer#999", &[(&a1, Some(&fake)), (&a2, None), (&a3, None)]).await?;
    assert_eq!(st, vec!["revealed", "revealed", "rejected"]);
    let atts = w.view("attesters", json!({})).await?;
    let liar = atts.as_array().unwrap().iter().find(|a| a[0] == a1.as_str()).unwrap();
    assert_eq!(liar[1]["slashes"], 1);
    assert_eq!(w.u128("ft_total_supply", json!({})).await?, 100 * E18);

    // Linking releases the escrow.
    w.call(&bob, "request_link", json!({ "github_id": 101, "login": "vignesh2027" }), NearToken::from_millinear(10).as_yoctonear()).await?.assert_success();
    let link = format!("link\n101\nvignesh2027\n{bob}\n1500000000");
    let st = w.round(&format!("link:101:{bob}"), &[(&a2, Some(&link)), (&a3, Some(&link))]).await?;
    assert_eq!(st, vec!["revealed", "linked"]);
    assert_eq!(w.u128("ft_balance_of", json!({ "account_id": bob })).await?, 70 * E18);

    // And the coins move like any NEP-141 token.
    w.call(&carol, "storage_deposit", json!({}), NearToken::from_millinear(10).as_yoctonear()).await?.assert_success();
    w.call(&bob, "ft_transfer", json!({ "receiver_id": carol, "amount": (5 * E18).to_string() }), 1).await?.assert_success();
    assert_eq!(w.u128("ft_balance_of", json!({ "account_id": carol })).await?, 5 * E18);
    Ok(())
}
