# SoroForge

[![CI](https://github.com/soroworks/soroforge/actions/workflows/ci.yml/badge.svg)](https://github.com/soroworks/soroforge/actions/workflows/ci.yml)
[![Security](https://github.com/soroworks/soroforge/actions/workflows/security.yml/badge.svg)](https://github.com/soroworks/soroforge/actions/workflows/security.yml)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/soroworks/soroforge/badge)](https://securityscorecards.dev/viewer/?uri=github.com/soroworks/soroforge)
[![Release](https://img.shields.io/github/v/release/soroworks/soroforge)](https://github.com/soroworks/soroforge/releases/latest)
[![Go](https://img.shields.io/github/go-mod/go-version/soroworks/soroforge)](go.mod)
[![License](https://img.shields.io/github/license/soroworks/soroforge)](LICENSE)

Deployment and lifecycle management for Stellar/Soroban smart contracts.

SoroForge turns contract deployment into a tracked, repeatable workflow with a
recorded history — so you manage contracts the way you manage any other deployed
software.

> **Independent project.** SoroForge is an independent, community-built tool. It
> is not affiliated with, endorsed by, or supported by the Stellar Development
> Foundation. "Stellar" and "Soroban" are used only to describe what this tool
> works with. For official tooling, see the
> [Stellar CLI](https://developers.stellar.org/docs/tools/developer-tools).

---

## Why

Deploying Soroban contracts today is a pile of one-off CLI invocations with no
memory. A week later, nobody can answer:

- Which WASM hash is live on testnet? On mainnet? Are they the same build?
- What changed between the version that's live and the one in `main`?
- Who deployed it, when, and which transaction did it?
- Has anyone changed it since?

The answers live in someone's shell history, if they live anywhere. SoroForge
records every deploy and upgrade in a database — Postgres or a SQLite file —
contract ID, WASM hash, network, deployer address, transaction, ledger, and
free-form notes — and can verify that record against the chain on demand.

That last part matters most. A deployment tracker that never checks itself will
confidently report stale information. `soroforge status` compares the WASM hash
the contract is *actually* running against the one SoroForge recorded, and tells
you when they disagree.

---

## Verified SDK and RPC versions

Soroban's deployment flow and RPC surface change. Everything below was verified
against the live API rather than recalled, and the live smoke tests
(`make test-live`) exist so you can re-verify after an upgrade.

| Component | Version | Notes |
|---|---|---|
| [`github.com/stellar/go-stellar-sdk`](https://github.com/stellar/go-stellar-sdk) | **v0.7.1** | The SDK repository was renamed from `github.com/stellar/go` in the October 2025 refactor. The old path is not used. |
| `clients/rpcclient` | (in SDK v0.7.1) | The Soroban RPC client now ships inside the SDK. No separate `stellar-rpc` client module is needed. |
| `protocols/rpc` | (in SDK v0.7.1) | Request/response types, imported as `protocol`. |
| Stellar RPC | Protocol 23 era | Field names verified against the SDK source; several differ from older tutorials. |
| Go | 1.25 | Required by the SDK's own `go.mod`. |

Two details that trip up code written from older examples:

- **Field names changed.** The simulation response uses `TransactionDataXDR`,
  `MinResourceFee`, `Results[].ReturnValueXDR`, and `Results[].AuthXDR`;
  resources use `DiskReadBytes` (formerly `ReadBytes`).
- **There is no `AssembleTransaction` helper in the Go SDK**, unlike the
  JavaScript SDK. SoroForge assembles transactions itself in
  [`internal/stellar/assemble.go`](internal/stellar/assemble.go): it decodes the
  simulated `SorobanTransactionData`, attaches it and the recorded auth entries
  to the operation, and rebuilds the transaction so that `txnbuild` recomputes
  the fee as `BaseFee × operations + ResourceFee`.

Other pinned dependencies: `pgx/v5 v5.11.0`, `golang-migrate/v4 v4.20.1`,
`cobra v1.10.2`, `chi/v5 v5.3.2`, `testify v1.12.1`, `yaml.v3 v3.0.1`,
`modernc.org/sqlite v1.60.1` (pure Go, no CGO).

---

## How deploying works

Worth knowing, because it explains the command output.

**Deploy is two transactions.** Soroban permits one host function per
transaction, so SoroForge first uploads the bytecode — which becomes addressable
by its SHA-256 hash — and then creates a contract instance from that hash. The
instance gets an address; the bytecode is shared by every contract using it. If
the bytecode is already on-chain, the upload is skipped.

**Upgrade is not a protocol operation.** Soroban has no "upgrade" instruction. A
contract upgrades *itself* through a function it chooses to expose, typically:

```rust
pub fn upgrade(env: Env, new_wasm_hash: BytesN<32>) {
    let admin: Address = env.storage().instance().get(&DataKey::Admin).unwrap();
    admin.require_auth();
    env.deployer().update_current_contract_wasm(new_wasm_hash);
}
```

SoroForge uploads the new bytecode and invokes that function. A contract that
doesn't expose one **cannot be upgraded**, and SoroForge will report the
contract's own error. The entrypoint name defaults to `upgrade` and is
configurable per contract via `upgrade_fn`.

A confirmed transaction is not proof the code changed: an upgrade entrypoint
could accept the call and do something else. So after the transaction confirms,
SoroForge reads the contract's on-chain WASM hash back — retrying briefly while
the ledger closes — and only then reports success and records the upgrade. If
the contract still runs something else, the upgrade fails with both hashes and
nothing is recorded.

---

## Quickstart — deploy to testnet

**Prerequisites:** Go 1.26+ (required by `modernc.org/sqlite`, the SQLite
backend's pure-Go driver), a compiled contract `.wasm`, and a funded testnet
account. Postgres via Docker is one storage option; a SQLite file needs neither
(see [Storage backends](#storage-backends-postgres-and-sqlite)).

```bash
git clone https://github.com/soroworks/soroforge.git
cd soroforge
make build
```

**1. Create the schema.** With Postgres:

```bash
docker compose up -d
make migrate-up
```

With SQLite — no server, no Docker:

```bash
DATABASE_URL="sqlite:///soroforge.db" ./bin/soroforge migrate up
```

**2. Configure your project.** From your contract project, after
`stellar contract build`:

```bash
soroforge init
```

`init` finds the release WASM under `target/wasm32v1-none/release` (or the
older `wasm32-unknown-unknown`), preferring a `.optimized.wasm`, and writes a
`soroforge.yaml` with one alias per contract, targeting testnet:

```yaml
contracts:
  counter:
    wasm: "./target/wasm32v1-none/release/counter.wasm"
```

It never replaces an existing file without `--force`.

**3. Provide credentials.**

```bash
cp .env.example .env
```

Set `DATABASE_URL`, and a deployer key via either `SOROFORGE_SECRET_KEY` or
`SOROFORGE_KEYSTORE_PATH`. Fund the account at
<https://friendbot.stellar.org> if you haven't. Read the [security
notes](#security-notes-on-key-handling) first.

**4. Dry run.** This assembles and simulates both transactions, prints the
contract address a real deploy would produce, the estimated fee for each
transaction and in total (in stroops and XLM), and submits and records nothing:

```bash
./bin/soroforge deploy counter --network testnet --dry-run
```

A dry run doesn't need `DATABASE_URL`, so it doubles as a config check.

**5. Deploy for real.**

```bash
./bin/soroforge deploy counter --network testnet --notes "v1.0.0"
```

**6. Confirm it's tracked, and that the record is true.**

```bash
./bin/soroforge list
./bin/soroforge history counter
./bin/soroforge status counter
```

---

## Configuration reference

`soroforge.yaml` holds what's shareable and belongs in version control. Secrets
come from the environment and never go in this file. Unknown keys are rejected,
so a typo is a loud error rather than a silently ignored setting.

```yaml
version: 1                  # schema version; only 1 exists
default_network: testnet    # used when --network is omitted

networks:
  testnet:
    rpc_url: https://soroban-testnet.stellar.org
    passphrase: "Test SDF Network ; September 2015"
    sorovault_url: http://localhost:8080   # optional
    explorer_url: https://stellar.expert/explorer/testnet  # optional

contracts:
  counter:
    wasm: ./target/wasm32-unknown-unknown/release/counter.wasm
    constructor_args: []    # optional
    upgrade_fn: upgrade     # optional, defaults to "upgrade"
    salt: ""                # optional 32-byte hex
    networks:               # optional per-network overrides
      mainnet:
        constructor_args:
          - {type: address, value: GABC...}
        salt: "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
```

**Networks.** Any number, named freely. The passphrase is required rather than
inferred from the name — it's mixed into every signature, so a wrong value
produces a transaction that's invalid on the network you meant to use, and a
silently defaulted one could sign a mainnet deploy with testnet assumptions.

To see what SoroForge actually resolved — file, environment and flags merged —
run `soroforge config show`. It needs neither Postgres nor a signing key, prints
contract WASM paths resolved to absolute paths, and reports secrets only as
`(set)` or `(unset)`. `--json` emits the same view for scripts.

There is **no public SDF-hosted mainnet RPC**. Point `mainnet` at a provider you
trust or at your own node.

**`explorer_url`** is an optional block-explorer base URL. SoroForge appends
`/contract/{contract_id}` and `/tx/{transaction_hash}` for deploy, upgrade, and
history output. For example, set it to
`https://stellar.expert/explorer/testnet`. With no explorer URL, output remains
unchanged.

**`sorovault_url`** registers every confirmed deploy and upgrade with a
[SoroVault](https://github.com/soroworks/sorovault) registry serving that
network, so the contract's functions, types and events are discoverable the
moment it is live — and [SoroProbe](https://github.com/soroworks/soroprobe)
can type its arguments from them. An upgrade records a new interface version
and keeps the old one. Registration happens only after on-chain confirmation
and is best-effort: if SoroVault is down, the deploy still succeeds and is
recorded, and the output (`catalog` in JSON) says registration failed so you
can run `sorovault add` later. Dry runs never register anything.

**`webhooks`** notifies endpoints (Slack, Discord, internal services) whenever
a contract deploy or upgrade is confirmed. Networks can list URLs, or mappings
with `url` and an optional `secret`:

```yaml
networks:
  mainnet:
    rpc_url: https://mainnet.example.org
    passphrase: "Public Global Stellar Network ; September 2015"
    webhooks:
      - https://hooks.slack.com/services/...
      - url: https://internal.example.org/deploy-webhook
        secret: "shared-secret"
```

Each webhook receives a POST request with a JSON payload (`alias`, `network`,
`contract_id`, `wasm_hash`, `action`, `deployer_pubkey`, `tx_hash`, `ledger`,
`notes`). When a `secret` is provided, SoroForge signs the payload with HMAC-SHA256
in the `X-SoroForge-Signature` header (`sha256=<hex>`). Delivery is best-effort with
a timeout: a failing webhook never fails the deploy. Dry runs never trigger webhooks.

**Contracts.** `wasm` paths resolve against the config file's directory, not your
shell's working directory, so `soroforge deploy` behaves the same from anywhere.

**`salt`** makes a contract's address reproducible: addresses derive from
(deployer address, salt). Leave it unset and each deploy uses a random salt, so
deploying the same alias twice produces two distinct contracts. With a fixed
salt the address is known before the upload, so a deploy checks whether a
contract already exists there and stops with an actionable message — naming the
address and suggesting `soroforge upgrade` or a different salt — rather than
paying for an upload and failing later in simulation.

**Stellar Asset Contracts.** Projects using classic Stellar assets can declare `asset: CODE:ISSUER` or `asset: native` instead of `wasm:`. SoroForge deterministically derives the SAC contract ID, deploys the contract (or records the existing one if already live on-chain), and tracks its drift status. Because SACs are protocol-native contracts without uploaded bytecode, `soroforge upgrade` refuses them clearly.

**`networks`** overrides `constructor_args` and `salt` for one network: the
per-network value wins when that network is deployed to, and the contract-level
value is the default everywhere else (`wasm` and `upgrade_fn` are not
overridable). An explicit `constructor_args: []` in an override means "no
arguments on this network" — an empty list clears the contract default rather
than inheriting it — while an empty `salt` inherits the contract's. Both names
must match a network configured under `networks:`; an unknown one is a load
error reported with its full path, e.g.
`contracts.token.networks.mainnet.salt: unknown network (configured: testnet)`.

### Constructor arguments

Soroban takes arguments as XDR `ScVal` values, which nobody wants to hand-write.
Write them typed instead:

```yaml
constructor_args:
  - {type: address, value: GABC...}
  - {type: u32, value: 7}
  - {type: string, value: "Example Token"}
  - {type: i128, value: "170141183460469231731687303715884105727"}
  - {type: vec, value: [{type: u32, value: 1}, {type: u32, value: 2}]}
  - {type: map, value: [{key: {type: symbol, value: decimals},
                         value: {type: u32, value: 7}}]}
```

| Type | Value form |
|---|---|
| `bool` | `true` / `false` |
| `void` | omit `value` |
| `u32` `i32` `u64` `i64` | decimal integer |
| `u128` `i128` | decimal integer **as a quoted string** |
| `u256` `i256` | decimal integer **as a quoted string** |
| `timepoint` `duration` | decimal integer (64-bit unsigned) |
| `string` `symbol` | text (`symbol` is `[a-zA-Z0-9_]`, max 32 chars) |
| `bytes` | hex, with or without `0x` |
| `address` | `G...` account or `C...` contract |
| `vec` | a list of args |
| `map` | a list of `{key, value}` pairs |
| `xdr` | base64 `ScVal` — the escape hatch |

Quote anything beyond float64's exact range, or YAML will round it before
SoroForge sees it. The `xdr` type accepts a pre-encoded `ScVal`, so a type not
listed here is never a hard block.

Adding a type means one case in
[`internal/config/args.go`](internal/config/args.go) and one table entry in its
test.

#### Overriding arguments for one deploy

`deploy` can replace the configured arguments for a single run:

```bash
soroforge deploy counter --arg u32:7 --arg string:"Example Token"
soroforge deploy counter --args-file args.json
```

`--arg TYPE:VALUE` is repeatable and accepts the same types as the config file.
Only the first colon separates the type from the value, so a value may contain
one (`string:https://example.org`). `--args-file` points at a JSON array in the
same `{type, value}` shape — the only form that can express nested `vec` and
`map` arguments:

```json
[
  {"type": "u32", "value": 7},
  {"type": "vec", "value": [{"type": "symbol", "value": "a"}]}
]
```

The two flags are mutually exclusive, and the replacement applies to that one
deploy only: nothing in `soroforge.yaml` changes, and nothing is submitted
until the arguments validate. The typed arguments actually used — after
per-network overrides and any replacement — are recorded with the deployment
in the `constructor_args` column and shown by `history`, so a deploy can be
reproduced after the config has moved on. Neither flag works with `--all`,
which deploys each contract with its configured arguments.

### Initializers (contracts without constructors)

Contracts written before constructors existed on Soroban are initialized by an explicit function call (such as `initialize`) after deployment. SoroForge supports this via `init_fn` and `init_args`:

```yaml
contracts:
  counter:
    wasm: ./counter.wasm
    init_fn: initialize
    init_args:
      - {type: address, value: GABC...}
      - {type: u32, value: 1}
```

> [!WARNING]
> **Front-running risk:** An initializer is submitted as a separate transaction immediately after the contract is created. Between contract creation and initializer confirmation, there is a window where another party could attempt to front-run the initializer call and claim ownership.
>
> **Recommendation:** For all new Soroban smart contracts, use **constructors** (`constructor_args`) instead of initializers. Constructors execute atomically during contract creation, eliminating the front-running window entirely. Use `init_fn` only when deploying legacy contracts that do not support constructors. Note that `constructor_args` and `init_fn` are mutually exclusive.

### Environment variables

| Variable | Purpose |
|---|---|
| `DATABASE_URL` | History database: `postgres://...` or `sqlite:///path/to/file.db`. Required except for `deploy --dry-run`. |
| `SOROFORGE_SECRET_KEY` | Deployer secret seed (`S...`). |
| `SOROFORGE_KEYSTORE_PATH` | Path to a file containing only the seed. |
| `SOROFORGE_SIGNER_COMMAND` | External signing executable, for an HSM or KMS. Mutually exclusive with the seed sources. |
| `SOROFORGE_SIGNER_PUBLIC_KEY` | The `G...` address `SOROFORGE_SIGNER_COMMAND` signs for. |
| `SOROFORGE_MAX_CONTRACT_SIZE` | Override the network's contract WASM size limit, in bytes. |
| `SOROFORGE_API_TOKEN` | Read-write bearer token for `serve`. |
| `SOROFORGE_API_TOKENS` | Scoped bearer tokens for `serve`, as `name:scope:token` entries — see below. |
| `SOROFORGE_API_ADDR` | Listen address (default `127.0.0.1:8080`). |
| `SOROFORGE_LOG_FORMAT` | `text` (default) or `json`; same as `--log-format`. |
| `SOROFORGE_CONFIG` | Config file path (default `./soroforge.yaml`). |
| `TEST_DATABASE_URL` | Enables Postgres integration tests. |
| `SOROFORGE_LIVE` | Enables the live testnet smoke tests. |

---

## CLI reference

Global flags: `--config/-c`, `--network/-n`, `--log-level`, `--log-format`,
`--json`. Output goes to stdout and logs to stderr, so `--json` output stays
pipeable.

`--log-format text|json` (default `text`, or `$SOROFORGE_LOG_FORMAT`) selects
the slog handler. JSON mode writes one JSON object per line, for CI and log
aggregators (Loki, CloudWatch, Datadog) that filter by field rather than
regex:

```bash
soroforge deploy counter --log-format json
{"time":"2026-01-02T15:04:05Z","level":"INFO","msg":"loaded signing key","source":"env:SOROFORGE_SECRET_KEY","address":"G..."}
```

An unknown format is rejected with the accepted values rather than silently
falling back.

**Shell completion.** Generate a script with `soroforge completion
bash|zsh|fish|powershell`. Contract aliases tab-complete for `deploy`,
`upgrade`, `upload`, `history`, and `status`, and `--network` tab-completes the
configured network names — all sourced live from `soroforge.yaml`, using the
same path rules (`--config`, then `$SOROFORGE_CONFIG`, then `./soroforge.yaml`)
as every other command. Completion never calls an RPC endpoint and never opens
the database; a missing or invalid config simply yields no suggestions instead
of an error printed mid-prompt.

| Command | Description |
|---|---|
| `init` | Write a starter `soroforge.yaml` for the contracts built under `target/`. |
| `config show` | Print the resolved configuration (networks, contracts, environment); secrets stay `(set)`/`(unset)`. |
| `config check` | Check config and WASM files; `--offline` skips RPC checks. |
| `deploy <alias>` | Upload WASM, instantiate the contract, record it. `--arg`/`--args-file` override `constructor_args` for this run. |
| `deploy --all` | Deploy every configured contract not yet tracked on the network, in alias order. |
| `upgrade <alias>` | Upload new WASM and invoke the contract's upgrade entrypoint. |
| `upload <alias>` | Install WASM on-chain without deploying or upgrading; prints the hash for a governance proposal. |
| `list` | List tracked contracts; `--network` filters. |
| `history <alias>` | Show a contract's full deploy/upgrade timeline, newest first. |
| `history export` | Write every tracked contract and deployment as versioned JSON (`--out FILE`, else stdout). |
| `history import <file>` | Load an export into an empty database; `--merge` imports into a populated one. |
| `status <alias>` | Compare the on-chain WASM hash against the recorded one. |
| `status --all` | Drift-check every tracked contract on the network, for a single CI gate. |
| `networks` | Health-check every configured RPC endpoint: responds? latency, latest ledger, passphrase match. |
| `serve` | Run the HTTP API. |
| `migrate up\|down\|version` | Manage the schema. |
| `version` | Print the SoroForge version. |

`deploy`, `upgrade`, and `upload` accept `--dry-run` and `--notes`. Deploy and
upgrade also accept `--progress`, `--require-spec`, and `--git-commit`, which
records a source revision supplied by CI instead of detecting the current
checkout. History displays confirmed transaction fees in stroops and XLM, plus
the short source commit (`*` marks a dirty working tree).

**Artifact checks** run before any network call. WASM over the network's
contract size limit is rejected with its size and the limit, plus a hint to run
`stellar contract optimize`; if the network raises its limit, set
`SOROFORGE_MAX_CONTRACT_SIZE`. WASM without a `contractspecv0` section still
deploys, with a warning that SoroVault cannot catalogue it, client bindings
cannot be generated, and SoroProbe cannot type its arguments. The warning is in
`--json` output too; `--require-spec` turns it into an error. A structurally
malformed WASM is reported before anything is simulated.

`config check` checks that each configured WASM file exists and starts with the
WebAssembly magic bytes, and that each RPC endpoint reports the configured
network passphrase. It aggregates failures into one non-zero result. Pass
`--offline` to skip RPC checks while still validating every artifact.

**`--dry-run`** runs everything up to and including simulation and assembly,
then stops. It prints the assembled envelopes, the resulting contract address,
and the estimated fee for each transaction and in total — in stroops and XLM,
from simulation plus the base fee, so the estimate matches what a real
submission would declare. It writes nothing and submits nothing, and is safe to
point at mainnet.

**`--progress`** writes a progress line to stderr (`simulating`, `submitted`,
`waiting for ledger N`, `confirmed`) while a deploy waits for confirmation.
Progress is shown automatically when stderr is a terminal; the flag forces it on
in a pipe or CI log. Stdout, including `--json`, is never touched.

**`deploy --all`** fills in every contract the network doesn't have yet:
already-tracked aliases are skipped with a message, the rest are deployed in
alias order. The first failure stops the run, and the summary reports what
succeeded before it, so a partial run is never a mystery. `--dry-run` works
with `--all`, and `--json` returns an array of per-alias results whose `status`
is one of `deployed`, `dry_run`, `skipped`, or `failed`.

Contracts are processed in alias order, and SoroForge does not infer
dependencies between them: if one contract's constructor needs another
contract's address, deploy them individually in the order you need.

**`status` exit codes** make it usable as a CI gate:

| Code | Meaning |
|---|---|
| `0` | In sync. |
| `1` | The check could not be completed. |
| `2` | Drift, untracked, or missing on-chain. |

The four states it reports:

- **`in_sync`** — the chain matches the record.
- **`drift`** — the contract is running bytecode SoroForge didn't deploy.
  Someone changed it another way.
- **`untracked`** — SoroForge has no record of this contract on this network.
- **`missing`** — SoroForge has a record, but the chain has no such contract.

**`networks` exit codes** make it usable as a CI preflight gate. Every
configured network is probed concurrently, each under `--timeout` (default 5s):

| Code | Meaning |
|---|---|
| `0` | Every endpoint answered and reported the configured passphrase. |
| `1` | At least one endpoint is unreachable, too slow, or serves a different network. |

The report prints each network's latency, latest ledger, and a passphrase
verdict (`match`, `MISMATCH`, or `not verified`). The RPC URL is printed with
any userinfo and query-string credentials removed — provider keys commonly live
in the URL — so the output, including `--json`, is safe to paste into a ticket
or CI log.

**`history export` / `history import`** move the entire recorded history
between databases as a versioned JSON document (`version: 1`; the format is
documented in [`internal/store/history.go`](internal/store/history.go)).
Original ids and timestamps are carried through untouched — a re-imported
history reads like the original, not like it was all written on import day.
Import refuses a non-empty database unless `--merge`, which adds what is
missing and skips records that exactly duplicate what is stored; same id with
different content is always an error. The document contains every contract and
deployment in the database, including aliases that have since left
`soroforge.yaml` — the history outlives the config.

```bash
soroforge history export --out history.json   # or pipe stdout
soroforge history import history.json         # empty database
soroforge history import history.json --merge # existing records kept, duplicates skipped
```

---

## HTTP API reference

Mirrors the CLI, for CI pipelines that can't easily run a binary. Both surfaces
call the same service, so behaviour cannot drift between them.

```bash
soroforge serve
```

Every `/v1` endpoint requires `Authorization: Bearer <token>`. The server
**refuses to start without a token** — see the security notes.

### Scoped tokens

`SOROFORGE_API_TOKEN` is a read-write token, so existing single-token setups
keep working unchanged. `SOROFORGE_API_TOKENS` adds scoped tokens as a
comma-separated list of `name:scope:token` entries:

```
SOROFORGE_API_TOKEN=$(openssl rand -hex 32)             # read-write
SOROFORGE_API_TOKENS="dashboard:read:$DASHBOARD_TOKEN,ci:write:$CI_TOKEN"
```

- `read` tokens may call `GET` routes only. `write` tokens may call everything
  (write implies read).
- The name is a label for access logs (`"token":"dashboard"`), never the value.
- Tokens must be at least 16 characters, and names and values must be unique.
- A token is compared in constant time against every configured token, so
  timing cannot reveal which one matched or how far a guess got.

Use a read-only token for dashboards, monitoring jobs, and anything that only
reads state — it still cannot be used to sign a mainnet transaction if it leaks.
A valid token that lacks the required scope gets **403 Forbidden**, not 401:
the credential is real, the operation simply isn't permitted. A missing or
unknown token is **401 Unauthorized**.

| Method | Path | Scope | Description |
|---|---|---|---|
| `GET` | `/health` | public | Liveness. Process up; dependencies not checked. |
| `GET` | `/ready` | public | Readiness. `200` when the database answers and each configured network's RPC returns a latest ledger; `503` with the failing dependency names otherwise. Checks are cached briefly so probes cannot hammer the RPC. |
| `GET` | `/openapi.json` | public | OpenAPI 3.1 API description. |
| `POST` | `/v1/deploy` | write | `{"alias","network","notes","dry_run","git_commit","require_spec"}` → `201` (`200` for a dry run). Add `?async=true` for a job. |
| `POST` | `/v1/upgrade` | write | Same body → `200`. Add `?async=true` for a job. |
| `GET` | `/v1/jobs/{id}` | read | Async job state and, when finished, its result. |
| `GET` | `/v1/contracts?network=&limit=&offset=` | read | Tracked contracts, paginated (default limit 100, maximum 500). |
| `GET` | `/v1/contracts/{network}/{alias}/history?limit=&offset=` | read | Deployment history, paginated (default limit 100, maximum 500). |
| `GET` | `/v1/contracts/{network}/{alias}/status` | read | Drift check. |
| `GET` | `/v1/contracts/{network}/status` | read | Drift check of every tracked contract; `in_sync` is the verdict. |
| `GET` | `/v1/deployments/{id}` | read | Single deployment record by ID. Returns 404 if not found. |

Every route's scope is declared in one table in
[`internal/api/api.go`](internal/api/api.go), and a test walks the built router
to fail if a route is ever added without one.

The complete request and response schemas are served as OpenAPI 3.1 at
`GET /openapi.json`, which is public and needs no bearer token.

```bash
curl -X POST http://localhost:8080/v1/deploy \
  -H "Authorization: Bearer $SOROFORGE_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"alias":"counter","network":"testnet","dry_run":true}'
```

Unknown JSON fields are rejected, so a misspelled `dry_run` fails loudly instead
of deploying for real. Failures return `{"error": "..."}`; drift returns `200`
with `"state":"drift"`, because the check ran and produced an answer.

Each request writes one structured access log line to stderr at `info` — method,
route pattern, status, duration, request ID, client address, and the token's
name:

```json
{"time":"2026-01-02T15:04:05Z","level":"INFO","msg":"http request","method":"POST","route":"/v1/deploy","status":201,"duration":1843000000,"request_id":"...","remote_addr":"127.0.0.1:52344","token":"ci"}
```

The bearer token value, request bodies, and key material are never logged.
`/health` is logged at `debug` only, so a liveness probe polling every second
does not flood the log.

### Kubernetes & Helm

A Helm chart for deploying `soroforge serve` is provided under [`deploy/helm/soroforge`](deploy/helm/soroforge).

- Runs database migrations as a pre-install/pre-upgrade hook (`soroforge migrate up`).
- Executes as non-root (`runAsNonRoot: true`, UID 65532) with a read-only root filesystem and an `emptyDir` on `/tmp`.
- Service defaults to `ClusterIP`.
- Reads `DATABASE_URL`, `SOROFORGE_SECRET_KEY`, and API tokens from existing Kubernetes Secrets (never plain values).
- Mounts `soroforge.yaml` from a ConfigMap.
- Configures `/health` (liveness) and `/ready` (readiness) probes.

```bash
helm install soroforge deploy/helm/soroforge \
  --set secrets.existingSecret=soroforge-secrets
```

### Async jobs

A deploy waits for two transactions, so `POST /v1/deploy?async=true` (or
`/v1/upgrade`) returns `202 Accepted` immediately with a job body and a
`Location` header:

```bash
curl -X POST 'http://localhost:8080/v1/deploy?async=true' \
  -H "Authorization: Bearer $SOROFORGE_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"alias":"counter","network":"testnet"}'

# 202 Accepted
# Location: /v1/jobs/6f1c...
# {"id":"6f1c...","state":"queued","alias":"counter","network":"testnet","created_at":"..."}
```

`GET /v1/jobs/{id}` reports one of `queued`, `running`, `succeeded`, or
`failed`. On success, `result` is the identical body the synchronous endpoint
returns; on failure, `error` carries the message instead.

Jobs run one at a time **per alias**, across both deploy and upgrade, so two
operations on one contract cannot race each other. A second job for the same
alias stays `queued` until the first finishes. Jobs for different aliases run
concurrently. Synchronous requests are unchanged and are not queued.

Job state is **in memory**: a job survives the request that created it, but not
a SoroForge restart. Finished jobs are kept for 10 minutes, then `404`. What
survives a restart is the thing that always did — the deployment history in
Postgres and the contract on-chain.

---

## Storage backends: Postgres and SQLite

SoroForge records history in whichever database `DATABASE_URL` names. Both
implement the same `store.Store` interface and pass the same conformance suite,
so command behaviour and the HTTP API are identical across them — only the
storage characteristics differ.

| | Postgres | SQLite |
|---|---|---|
| URL form | `postgres://user:pass@host/db` | `sqlite:///soroforge.db` (absolute or project-relative file path) |
| Setup | Server or `docker compose up -d` | None; a file |
| Driver | `pgx/v5` | `modernc.org/sqlite` (pure Go; `CGO_ENABLED=0` builds keep working) |
| Best for | Teams, multiple machines, concurrent writers, `serve` with real traffic | Solo use, CI, the laptop, anything where "just a file" beats a server |
| Migrations | `internal/store/migrations/postgres/` | `internal/store/migrations/sqlite/` |

`sqlite://` URLs select the SQLite store; anything else is Postgres. Migrations
are dialect-aware (`soroforge migrate up` reads the scheme the same way), and
both directories are embedded in the binary. A parity test
(`TestMigrationDialectsStayInParity`) fails if the two dialects' table, column,
or index sets ever diverge, because a schema that drifts between backends means
an export→import round trip that silently loses data.

**When to choose which:** use SQLite when you are one developer or one CI job —
it removes the Docker prerequisite entirely (`go test ./...` already runs the
SQLite suite with no external dependency). Use Postgres when multiple people or
machines need to see the same history, when you run `serve` for a team, or when
you want managed backups. A single SQLite file is local: writes serialise, so
two concurrent `soroforge` processes on one file contend (the driver's busy
timeout absorbs short bursts, but it is not a shared database).

To move history between backends, use `soroforge history export` on one and
`soroforge history import` on the other — ids and timestamps carry through.

---

## Security notes on key handling

**SoroForge reads a key you provide. It does not generate, store, rotate, or
manage keys** — that's deliberate, and out of scope.

**What SoroForge does with your key:**

- Reads the seed from `SOROFORGE_SECRET_KEY` or the file at
  `SOROFORGE_KEYSTORE_PATH`. If both are set, the environment variable wins, so
  CI can override a developer's on-disk key.
- Passes it straight into a signer, which holds it unexported.
- Records only the derived public `G...` address.
- **Or never sees the key at all:** with `SOROFORGE_SIGNER_COMMAND`, SoroForge
  sends the transaction hash to an external executable and receives a signature,
  which it verifies against `SOROFORGE_SIGNER_PUBLIC_KEY` before use. The key
  stays in the HSM or KMS. See [External signers](#external-signers). Configuring
  a signer command together with a seed or keystore is rejected rather than
  silently choosing one.

**What it never does:** log the seed at any level, write it to the database,
include it in `--json` output or API responses, or echo it in an error message —
a malformed seed is still a secret, and errors end up in CI logs.
`Env.Redacted()` reports every secret as `(set)`/`(unset)`.

**Recommendations:**

- Prefer a **keystore file** locally, at mode `0600`. Environment variables are
  readable by every child process and land in shell history.
- Use your CI provider's secret store for `SOROFORGE_SECRET_KEY`. Don't commit
  `.env` — it's gitignored, keep it that way.
- Use a **separate key per network**. A mainnet deploy key shouldn't be on a
  laptop that also deploys to testnet.
- Always `--dry-run` against mainnet first.

**HTTP API:** the server refuses to start without `SOROFORGE_API_TOKEN` or
`SOROFORGE_API_TOKENS`, and rejects tokens shorter than 16 characters. Tokens
are compared in constant time, since a naive comparison leaks how much of the
token matched through response timing. It binds `127.0.0.1` by default because
this process holds a signing key; widen that only behind your own TLS and
authentication. Generate a real token with `openssl rand -hex 32`.

Prefer a **read-only token** (`name:read:...`) for anything that does not
deploy — a dashboard, a monitoring job, a status page. It can call every `GET`
route but is refused with `403` on `/v1/deploy` and `/v1/upgrade`, so a leak
cannot become a mainnet transaction.

**Not implemented, by design:** key generation, multisig or threshold signing,
and CI plugin integrations. Hardware wallets and KMS-backed keys are supported
through the external signer command. The `stellar.Signer` interface remains the
extension point for signing-related work — see
[`internal/stellar/signer.go`](internal/stellar/signer.go).

### External signers

For keys that must not be exported — an HSM, a cloud KMS, or a remote signing
service — point `SOROFORGE_SIGNER_COMMAND` at an executable and set
`SOROFORGE_SIGNER_PUBLIC_KEY` to its `G...` address:

```bash
export SOROFORGE_SIGNER_COMMAND=/usr/local/bin/my-hsm-signer
export SOROFORGE_SIGNER_PUBLIC_KEY=GABC...
```

SoroForge never sees the private key. It runs the command directly (no shell),
writes one JSON line to stdin, and reads one base64 signature from stdout:

```json
{"hash":"9f2c...64 lowercase hex characters...","public_key":"GABC..."}
```

`hash` is the **network-specific transaction hash**, never the envelope — the
same 32 bytes a Stellar signer signs. `public_key` is the configured address, so
one command can serve several keys. stdout must be the raw 64-byte Ed25519
signature, standard or unpadded base64, surrounding whitespace ignored. Anything
on stderr is captured for error messages only.

SoroForge verifies the returned signature against `SOROFORGE_SIGNER_PUBLIC_KEY`
and refuses to submit if it does not match, so a command signing with the wrong
key fails the deploy rather than producing an invalid transaction. A non-zero
exit, a missing or malformed signature, or an invocation longer than 30 seconds
is a clear error.

[`examples/signer/main.go`](examples/signer/main.go) is a complete, minimal
implementation of the protocol. It holds a local test key rather than calling an
HSM — that is the one part a real signer replaces:

```bash
go build -o /tmp/soroforge-example-signer ./examples/signer
SOROFORGE_EXAMPLE_SIGNER_SEED=S... /tmp/soroforge-example-signer
```

The command is split on whitespace, so quoting is not interpreted; keep
credentials out of its arguments (they are visible in the process list). If the
signer needs a token, read it from the command's own environment.

---

## Architecture

```
cmd/soroforge/      cobra CLI
internal/config/    soroforge.yaml + env loading, validation, ScVal encoding
internal/stellar/   RPC client, transaction assembly, signing (all interfaced)
internal/deploy/    deploy/upgrade/status orchestration
internal/store/     history: Postgres, SQLite, in-memory + embedded migrations
internal/netcheck/  concurrent RPC health probes behind `soroforge networks`
internal/api/       chi HTTP handlers mirroring the CLI
internal/readiness/ database + per-network RPC probes behind GET /ready
internal/catalog/   best-effort registration with SoroVault after a deploy
```

Three interfaces are the seams: `stellar.Client` (RPC), `stellar.Signer`
(signing), and `store.Store` (persistence). `store.Store` has two production
implementations — Postgres and SQLite — plus an in-memory one, which is why
`go test ./...` needs neither a network nor a database.

`stellar.Client` deliberately speaks base64 XDR strings rather than SDK structs,
so a fake needs no XDR plumbing. Everything that interprets XDR — building
operations, assembling simulated transactions, deriving contract IDs — is a pure
function, tested directly.

### Data model

`contracts` holds current state, one row per contract per network, unique on
`(network, alias)`. `deployments` is append-only: an upgrade adds a row rather
than modifying one, so the rows for a contract *are* its version history. The
typed constructor arguments a deploy used are stored per row in
`deployments.constructor_args` as JSON, next to the free-form `notes`. Both are
indexed on `(network, contract_id)`.

Records are written **only after on-chain confirmation**. A history claiming a
deploy that never landed is worse than no history.

---

## Testing

```bash
make test          # no network, no database
make test-race
make test-integration   # adds the Postgres suite (needs docker compose up -d)
make test-live          # read-only smoke tests against live testnet
```

`go test ./...` never requires a network or a database — the Postgres and live
tests skip unless their environment variable is set. The SQLite store is pure
Go, so its full conformance suite runs as part of the default test command with
no setup at all. The Postgres suite runs the *same* test suite as the in-memory
store (and SQLite, and the history import/export suite), which keeps the fakes
honest about matching real database semantics.

The live tests are read-only (no keys, no submissions, no fees). They exist
because the rest of the suite proves SoroForge is internally consistent, not
that its understanding of the RPC API is still correct. Run them after upgrading
the Stellar SDK.

---

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). The interfaces above are the intended
extension points, and `TODO(extension)` comments mark where new implementations
plug in.

## License

[Apache-2.0](LICENSE).
