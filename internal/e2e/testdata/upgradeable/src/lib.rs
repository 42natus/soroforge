#![no_std]
//! A deliberately minimal upgradeable contract for SoroForge's end-to-end test.
//!
//! `upgrade` has NO authorization check: anyone may replace the bytecode. That
//! is what lets the test upgrade the contract "outside SoroForge" to provoke
//! drift. Never deploy this anywhere that matters.
use soroban_sdk::{contract, contractimpl, BytesN, Env};

#[contract]
pub struct Upgradeable;

#[contractimpl]
impl Upgradeable {
    /// Distinguishes the two bytecode versions the test moves between.
    pub fn version(_env: Env) -> u32 {
        if cfg!(feature = "v2") {
            2
        } else {
            1
        }
    }

    /// The entrypoint `soroforge upgrade` invokes (config default: `upgrade`).
    pub fn upgrade(env: Env, new_wasm_hash: BytesN<32>) {
        env.deployer().update_current_contract_wasm(new_wasm_hash);
    }
}
