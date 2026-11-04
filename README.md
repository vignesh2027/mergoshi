<p align="center"><img src="assets/logo.svg" width="96" alt="Mergoshi logo"></p>

# Mergoshi (MRGO)

A memecoin on NEAR that only mints when code ships. Every MRGO comes from a pull request merged into a participating open source repository, and the whole mechanic lives in an NEP-141 contract with no owner and no admin method.

* **Proof of merge.** Three bonded attesters read GitHub independently, commit to a hash of a canonical claim, then reveal it. When two agree, the contract mints. A dissenting reveal is slashed.
* **The LGTM tax.** Each merge mints 100 MRGO (halving every 10 000 merges): 70 to the author, 20 shared by up to three approvers, 5 to the attesters, 5 to the feast pot. Nobody approved? The review share is never minted.
* **Streaks.** Merge in consecutive weeks for up to +40 percent.
* **Escrow with a fuse.** Contributors who have not linked a NEAR account have their coins held for their GitHub id; link within the fuse or the coins burn.
* **Maintainers' feast.** On schedule, anyone can call the feast: half the pot burns, half feeds the busiest reviewers.
* **Sybil limits, written plainly.** Daily caps that grow with a repository's number of distinct aged authors, a 90 day account age gate, a burned offering to enroll a repository, and an exile ritual that lets holders banish a farm by burning as much as it minted.

Track: **Memecoins With Mechanics**. The design, threat model, parameter choices and the simulation behind them are in [DESIGN.md](DESIGN.md).

## Layout

```
contract/   Rust token contract (near-sdk 5, near-contract-standards): NEP-141, NEP-145, NEP-148
            src/claim.rs     canonical claims and commitments
            src/rounds.rs    commit, reveal, settle, slash, sweep
            src/effects.rs   minting split, caps, links, feast, escrow fuse, exile
            src/mechanic.rs  the arithmetic, mirrored by sim/mechanic.py
            res/             a committed build of the wasm
services/   Go (standard library only): NEAR signing and RPC, GitHub reader, attesters,
            dashboard (html/template, no JavaScript), local sandbox mode, fake GitHub
sim/        Python (standard library only): supply simulation driven by real merge history
assets/     logo (SVG and 512 px PNG)
```

## Requirements

* Go 1.23 or later
* Python 3.10 or later (simulation and its tests only)
* For the contract tests and for rebuilding the wasm: Rust with the `wasm32-unknown-unknown` target (the toolchain is pinned in `contract/rust-toolchain.toml`; rustup installs it on first use) and [cargo-near](https://github.com/near/cargo-near) 0.21 or later
* Network access on first run to download the near-sandbox node (about 70 MB, cached in `.localnet/bin`)

Supported platforms for the local node: macOS on Apple silicon, Linux x86_64 and Linux arm64.

## Run it locally (one command)

```sh
make local
```

This builds `bin/mergoshi`, starts a near-sandbox node, deploys the token from `contract/target/near` (or the committed build in `contract/res` if you have not run `make contract`), creates and bonds three attesters, and serves:

* http://localhost:8080 the live page: supply, attesters, open rounds, repositories with today's caps, and the ledger of every mint and burn
* http://localhost:8080/fake a fake GitHub. Pick a repository, an author and approvers, press **Merge pull request** and watch the attesters commit, reveal and mint within a few seconds. The same page has buttons for the rituals and attacks: link a GitHub account (releases its escrow), send a lying attester (rejected and slashed), propose a repository (burns the offering), call the feast, exile a repository.

Local mode uses short windows (feast every 3 minutes, escrow fuse 10 minutes) so every ritual can be seen in one sitting. Stop with Ctrl+C; `make local` always starts a fresh chain.

## Tests

```sh
make test-unit   # contract unit tests, Go tests, simulation tests; no network
make test        # also the sandbox integration test (downloads near-sandbox on first run)
```

* `contract/src/tests.rs`: 33 tests including `golden_replay_matches_the_simulation`, which replays the 195 events in `sim/golden.json` through real commit and reveal rounds and requires the same supply, burns, escrow and balances as the Python model.
* `contract/tests/sandbox.rs`: deploys the wasm to a sandbox node and runs a mint, a slash, a link and a transfer.
* `services/...`: claim encoding shared test vectors (the same hashes are asserted in Rust), attester behaviour against the fake GitHub, NEAR transaction encoding, templates.
* `sim/tests`: the rules, the golden file and the scenarios.

## Simulation

```sh
make sim         # python3 sim/run.py, about five seconds
```

Writes the charts and `results.json` to `sim/out`. The model is driven by `sim/data/snapshot.json`, 90 days of public merged pull requests in the ten genesis repositories. To refresh it from the public GitHub API (no token needed, about ten minutes because of the search rate limit):

```sh
./bin/mergoshi snapshot
```

## Testnet

The live deployment:

* token `token.mergoshi-8104ad.testnet`
* attesters `attester-1.mergoshi-8104ad.testnet`, `attester-2.mergoshi-8104ad.testnet`, `attester-3.mergoshi-8104ad.testnet`

To deploy your own copy:

```sh
make contract    # optional, otherwise contract/res is used
make bootstrap   # one root account from the public testnet helper, attesters, token, bonds
make serve       # dashboard on :8080 plus the three attesters
make status
```

`bootstrap` writes the account ids and keys it creates to `.env`, which is gitignored, and is safe to run again. The attesters read the public GitHub API without a token; set `GITHUB_TOKEN` in `.env` only if you want a higher rate limit.

## Claiming coins as a contributor

Publish a public gist containing `mergoshi-link:<your NEAR account>`, then call `request_link(github_id, login)` from that account with 0.01 NEAR attached. The attesters confirm the gist and the contract releases your escrow. The dashboard's **Claim** page has the exact command.

## Limits

Read the threat model in [DESIGN.md](DESIGN.md#4-threat-model-for-minting). In short: safety assumes fewer than a quorum of attesters collude; an attacker who controls an eligible repository and several aged GitHub accounts can farm up to that repository's cap until holders exile it; no parameter can be changed after launch, by design.

## License

MIT
