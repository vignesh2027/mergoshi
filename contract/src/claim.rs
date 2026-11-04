//! Canonical claims. An attester reads GitHub, writes one of these texts,
//! hashes it, and commits to the hash. The Go attester builds the same bytes;
//! `services/internal/claim` has the matching code and shared test vectors.
//!
//! Each claim is a list of fields joined by a single `\n`, with no trailing newline:
//!
//! ```text
//! merge\n<owner/repo>\n<number>\n<merge sha>\n<merged_at>\n<author id>\n<author login>\n<author created_at>\n<approvers>
//! link\n<github id>\n<login>\n<near account>\n<github created_at>
//! repo\n<owner/repo>\n<repo id>\n<stars at or above min_repo_stars 0|1>\n<created_at>\n<fork 0|1>
//! ```
//!
//! Repository names are lower case. Times are unix seconds. `approvers` is
//! `id:login` pairs joined by `,`, sorted by id, without the author; it is empty
//! when nobody approved.

use near_sdk::{AccountId, env};

/// The hash an attester reveals to say "this did not happen".
pub const NONE_HASH: [u8; 32] = [0u8; 32];

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Approver {
    pub id: u64,
    pub login: String,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct MergeClaim {
    pub repo: String,
    pub number: u64,
    pub merge_sha: String,
    pub merged_at: u64,
    pub author_id: u64,
    pub author_login: String,
    pub author_created_at: u64,
    pub approvers: Vec<Approver>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LinkClaim {
    pub github_id: u64,
    pub login: String,
    pub account: AccountId,
    pub github_created_at: u64,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RepoClaim {
    pub repo: String,
    pub repo_id: u64,
    /// Whether the repository has at least `min_repo_stars` stars. A flag, not a
    /// count, so two attesters reading a few seconds apart still agree.
    pub stars_ok: bool,
    pub created_at: u64,
    pub fork: bool,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Claim {
    Merge(MergeClaim),
    Link(LinkClaim),
    Repo(RepoClaim),
}

impl Claim {
    /// The round key this claim answers.
    pub fn key(&self) -> String {
        match self {
            Claim::Merge(m) => merge_key(&m.repo, m.number),
            Claim::Link(l) => link_key(l.github_id, &l.account),
            Claim::Repo(r) => repo_key(&r.repo),
        }
    }
}

pub fn merge_key(repo: &str, number: u64) -> String {
    format!("merge:{repo}#{number}")
}
pub fn link_key(github_id: u64, account: &AccountId) -> String {
    format!("link:{github_id}:{account}")
}
pub fn repo_key(repo: &str) -> String {
    format!("repo:{repo}")
}

/// What a round key is about.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Kind {
    Merge,
    Link,
    Repo,
}

pub fn key_kind(key: &str) -> Option<Kind> {
    if key.len() > 160 {
        return None;
    }
    if key.starts_with("merge:") {
        Some(Kind::Merge)
    } else if key.starts_with("link:") {
        Some(Kind::Link)
    } else if key.starts_with("repo:") {
        Some(Kind::Repo)
    } else {
        None
    }
}

/// The repository named in a merge key.
pub fn merge_key_repo(key: &str) -> Option<&str> {
    let rest = key.strip_prefix("merge:")?;
    let (repo, num) = rest.rsplit_once('#')?;
    num.parse::<u64>().ok()?;
    Some(repo)
}

/// A valid lower case `owner/name` repository string.
pub fn valid_repo(repo: &str) -> bool {
    let Some((owner, name)) = repo.split_once('/') else {
        return false;
    };
    let ok = |s: &str, extra: &str| {
        !s.is_empty()
            && s.len() <= 100
            && s.chars().all(|c| {
                c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-' || extra.contains(c)
            })
    };
    ok(owner, "") && ok(name, "._") && !name.contains('/')
}

fn valid_login(s: &str) -> bool {
    !s.is_empty() && s.len() <= 39 && s.chars().all(|c| c.is_ascii_alphanumeric() || c == '-')
}

fn num(s: &str) -> Result<u64, String> {
    if s.is_empty() || (s.len() > 1 && s.starts_with('0')) {
        return Err(format!("bad number {s:?}"));
    }
    s.parse::<u64>().map_err(|_| format!("bad number {s:?}"))
}

/// Parses a canonical claim. Anything that is not exactly canonical is refused,
/// so two attesters can only agree on a hash if they wrote identical text.
pub fn parse(text: &str) -> Result<Claim, String> {
    let f: Vec<&str> = text.split('\n').collect();
    match f.first().copied() {
        Some("merge") if f.len() == 9 => {
            let repo = f[1].to_string();
            if !valid_repo(&repo) {
                return Err("bad repo".into());
            }
            let sha = f[3];
            if sha.len() != 40
                || !sha
                    .chars()
                    .all(|c| c.is_ascii_digit() || ('a'..='f').contains(&c))
            {
                return Err("bad merge sha".into());
            }
            let author_id = num(f[5])?;
            if !valid_login(f[6]) {
                return Err("bad author login".into());
            }
            let mut approvers = Vec::new();
            if !f[8].is_empty() {
                for pair in f[8].split(',') {
                    let (id, login) = pair.split_once(':').ok_or("bad approver")?;
                    let id = num(id)?;
                    if !valid_login(login) || id == author_id {
                        return Err("bad approver".into());
                    }
                    if approvers.last().is_some_and(|a: &Approver| a.id >= id) {
                        return Err("approvers must be sorted by id without repeats".into());
                    }
                    approvers.push(Approver {
                        id,
                        login: login.to_string(),
                    });
                }
            }
            if approvers.len() > 20 {
                return Err("too many approvers".into());
            }
            Ok(Claim::Merge(MergeClaim {
                repo,
                number: num(f[2])?,
                merge_sha: sha.to_string(),
                merged_at: num(f[4])?,
                author_id,
                author_login: f[6].to_string(),
                author_created_at: num(f[7])?,
                approvers,
            }))
        }
        Some("link") if f.len() == 5 => {
            if !valid_login(f[2]) {
                return Err("bad login".into());
            }
            let account: AccountId = f[3].parse().map_err(|_| "bad account")?;
            Ok(Claim::Link(LinkClaim {
                github_id: num(f[1])?,
                login: f[2].to_string(),
                account,
                github_created_at: num(f[4])?,
            }))
        }
        Some("repo") if f.len() == 6 => {
            if !valid_repo(f[1]) {
                return Err("bad repo".into());
            }
            let flag = |v: &str| match v {
                "0" => Ok(false),
                "1" => Ok(true),
                _ => Err("bad flag".to_string()),
            };
            let fork = flag(f[5])?;
            Ok(Claim::Repo(RepoClaim {
                repo: f[1].to_string(),
                repo_id: num(f[2])?,
                stars_ok: flag(f[3])?,
                created_at: num(f[4])?,
                fork,
            }))
        }
        _ => Err("unknown claim".into()),
    }
}

/// sha256 of the claim text, or the NONE hash for "this did not happen".
pub fn claim_hash(text: Option<&str>) -> [u8; 32] {
    match text {
        Some(t) => env::sha256_array(t.as_bytes()),
        None => NONE_HASH,
    }
}

/// The commitment an attester posts before revealing:
/// sha256(attester || "\n" || key || "\n" || claim_hash || salt).
/// Binding the attester id stops anyone from copying another attester's commitment.
pub fn commitment(attester: &str, key: &str, hash: &[u8; 32], salt: &[u8; 32]) -> [u8; 32] {
    let mut buf = Vec::with_capacity(attester.len() + key.len() + 66);
    buf.extend_from_slice(attester.as_bytes());
    buf.push(b'\n');
    buf.extend_from_slice(key.as_bytes());
    buf.push(b'\n');
    buf.extend_from_slice(hash);
    buf.extend_from_slice(salt);
    env::sha256_array(&buf)
}

pub fn parse_hex32(s: &str) -> Result<[u8; 32], String> {
    let v = hex::decode(s).map_err(|_| "bad hex".to_string())?;
    v.try_into().map_err(|_| "expected 32 bytes".to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    pub const MERGE: &str = "merge\nprometheus/client_golang\n2182\n0123456789abcdef0123456789abcdef01234567\n1793010367\n101\nvignesh2027\n1500000000\n7:beorn7,99:kakkoyun";

    #[test]
    fn parses_merge() {
        let Claim::Merge(m) = parse(MERGE).unwrap() else {
            panic!()
        };
        assert_eq!(m.number, 2182);
        assert_eq!(m.approvers.len(), 2);
        assert_eq!(
            parse(MERGE).unwrap().key(),
            "merge:prometheus/client_golang#2182"
        );
    }

    #[test]
    fn refuses_non_canonical() {
        assert!(parse(&MERGE.replace("prometheus", "Prometheus")).is_err());
        assert!(parse(&MERGE.replace("7:beorn7,99:kakkoyun", "99:kakkoyun,7:beorn7")).is_err());
        assert!(parse(&MERGE.replace("7:beorn7", "101:vignesh2027")).is_err());
        assert!(parse(&format!("{MERGE}\n")).is_err());
        assert!(parse(&MERGE.replace("\n2182\n", "\n02182\n")).is_err());
        assert!(parse("link\n5\nbob\nnot an account\n1").is_err());
    }

    #[test]
    fn parses_link_and_repo() {
        let l = parse("link\n42\nalice\nalice.testnet\n1500000000").unwrap();
        assert_eq!(l.key(), "link:42:alice.testnet");
        let r = parse("repo\nhelm/chart-testing\n12345\n1\n1500000000\n0").unwrap();
        assert_eq!(r.key(), "repo:helm/chart-testing");
        assert_eq!(
            merge_key_repo("merge:helm/chart-testing#873"),
            Some("helm/chart-testing")
        );
        assert!(valid_repo("open-telemetry/opentelemetry-go.contrib_x"));
        assert!(!valid_repo("Open/x"));
    }

    /// Shared with services/internal/claim/claim_test.go: both sides must print these.
    #[test]
    fn vectors() {
        let h = claim_hash(Some(MERGE));
        assert_eq!(
            hex::encode(h),
            "e5b2e5ad992365a0f71895aac591225acf747ded72ecdcdc1961c6e02551e79f"
        );
        let c = commitment(
            "attester-1.testnet",
            "merge:prometheus/client_golang#2182",
            &h,
            &[7u8; 32],
        );
        assert_eq!(
            hex::encode(c),
            "79da1e975b4e0a0c7f4b4a79ac927dd1858d6d870c4a02a657d1f90b6458a68d"
        );
    }
}
