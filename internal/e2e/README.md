# End-to-end test

`TestDeployUpgradeAndDrift` runs SoroForge's real deploy -> status -> upgrade ->
drift cycle against a standalone Stellar network, instead of `MockClient`.

It deploys a small contract, checks `status` is `in_sync`, upgrades it through
SoroForge and checks `status` and that `history` has both rows, then upgrades
the contract outside SoroForge and expects `drift`.

It is behind the `e2e` build tag, so `make check` and `go test ./...` stay
offline.

## Running it

```
docker run -d --name stellar -p 8000:8000   stellar/quickstart:v678-b1525.1-latest --local --enable rpc
make test-e2e
```

The test funds a fresh key through the network's Friendbot and waits for it to
come up. Override the endpoints with `E2E_RPC_URL` (default
`http://localhost:8000/rpc`) and `E2E_FRIENDBOT_URL` (default
`http://localhost:8000/friendbot`).

CI runs the same thing in the `e2e` job.

## Pinned versions

- Image: `stellar/quickstart:v678-b1525.1-latest`, a release tag rather than `latest`,
  so a quickstart change cannot break CI unannounced. Bump it deliberately and
  re-run the job.
- Network passphrase: `Standalone Network ; February 2017`.

## The contract

`testdata/upgradeable/` is a minimal Soroban contract with two functions:
`version()` and `upgrade(new_wasm_hash)`. `v1.wasm` and `v2.wasm` are checked
in so CI needs no Rust toolchain; they differ only in what `version()` returns
(the `v2` cargo feature), which gives two distinct bytecode hashes.

It is built with `soroban-sdk` 25 on purpose: a contract's protocol version must not
be newer than the quickstart node's, and an SDK newer than the pinned image
fails with "contract protocol number is newer than host". Raise both together.

`upgrade` has no authorization check, on purpose: it lets the test upgrade the
contract outside SoroForge. Do not deploy it anywhere that matters.

To rebuild (needs `stellar` CLI v25.2.0 or newer):

```
cd internal/e2e/testdata/upgradeable
stellar contract build --out-dir out1 && cp out1/upgradeable.wasm v1.wasm
stellar contract build --features v2 --out-dir out2 && cp out2/upgradeable.wasm v2.wasm
```
