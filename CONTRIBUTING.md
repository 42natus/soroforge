# Contributing to SoroForge

Thanks for considering a contribution. SoroForge is deliberately a small core
with clear extension points rather than a feature-complete tool, so most useful
contributions are additive and land in one place.

## Getting set up

You need Go 1.25+ (required by the Stellar SDK) and Docker for Postgres.

```bash
git clone https://github.com/soroworks/soroforge.git
cd soroforge
make build
make test
```

`make test` requires neither a network nor a database. If it needs either,
something has regressed — please report it.

### Dev container (optional)

The repository ships a [dev container](.devcontainer/) with Go 1.26, Postgres
and golangci-lint already wired together. In VS Code, open the folder and
choose **Reopen in Container**; in GitHub Codespaces, create a codespace.
Then:

```bash
make check
make test-integration
```

`DATABASE_URL` and `TEST_DATABASE_URL` point at the container's Postgres, so
the integration suite runs without any other setup. A local Go install and
Docker compose still work exactly as described below — the dev container is a
convenience, not a requirement.

For the full suite:

```bash
docker compose up -d
make migrate-up
make test-integration   # adds the Postgres suite
make test-live          # read-only smoke tests against live testnet
```

Before opening a pull request:

```bash
make check    # fmt, vet, test
```

## How the code is organised

```
cmd/soroforge/      cobra CLI
internal/config/    soroforge.yaml + env loading, validation, ScVal encoding
internal/stellar/   RPC client, transaction assembly, signing
internal/deploy/    deploy/upgrade/status orchestration
internal/store/     Postgres history + embedded migrations
internal/api/       chi HTTP handlers mirroring the CLI
```

Three interfaces are the seams the whole design rests on:

| Interface | Production | Test double |
|---|---|---|
| `stellar.Client` | `RPCClient` | `MockClient` |
| `stellar.Signer` | `KeypairSigner` | `MockSigner` |
| `store.Store` | `Postgres` | `MemoryStore` |

The test doubles live in the packages themselves rather than in `_test.go`
files, because other packages need them too. If you add a feature, you should be
able to test it without provisioning anything.

Two rules that keep this working:

- **Lifecycle logic belongs in `internal/deploy`.** The CLI and the HTTP API are
  both thin layers over the same `deploy.Service`. Putting behaviour in one of
  them means the two surfaces drift apart.
- **Only `internal/stellar/rpc.go` touches the Stellar SDK's RPC types.**
  Everything else works against the `stellar.Client` interface, which speaks
  base64 XDR strings. An SDK upgrade should be containable to that one file.

## Common contributions

### Adding a constructor argument type

Everything is in one switch. Add a case to `Arg.ToScVal` in
[`internal/config/args.go`](internal/config/args.go), add the name to
`SupportedArgTypes`, and add a row to the table test in `args_test.go`. Nothing
else needs to change — and note that `{type: xdr}` already means no type is ever
a hard blocker for a user.

### Adding a Signer

Implement `stellar.Signer` (`Address` and `Sign`). Hardware wallets, remote
signing services, and multisig collection all fit behind it without any
orchestration change. See the `TODO(extension)` note in
[`internal/stellar/signer.go`](internal/stellar/signer.go).

If your implementation holds key material, follow the existing rules: keep it
unexported, never log it, never serialise it, and never include it in an error
message.

### Adding a store backend

Implement `store.Store` and run it against `runStoreSuite` in
`internal/store/store_test.go` — the same suite both existing implementations
pass. That's what keeps their semantics aligned.

### Adding a CLI command

Add a `newXxxCmd()` in [`cmd/soroforge/commands.go`](cmd/soroforge/commands.go)
and register it in `newRootCmd`. If it does anything a CI pipeline would want,
add the matching HTTP handler too, and have both call the same service method.

### Adding a migration

Add a numbered pair in `internal/store/migrations/`
(`000002_thing.up.sql` and `000002_thing.down.sql`). They're embedded into the
binary, so nothing else needs wiring. Write a real `down` — reversibility is
tested.

## Deliberately out of scope

These are not oversights. Please open an issue to discuss before building one:

- A web UI
- Key generation, storage, or rotation beyond reading a provided key
- Multisig and threshold signing
- CI plugin integrations

The interfaces above exist so these can be built *on* SoroForge rather than in
it.

## Testing expectations

- `go test ./...` must never require a network or a database. Anything that
  does belongs behind an environment-variable skip, like the Postgres and live
  suites.
- Test behaviour, not implementation. The most valuable tests here assert what
  SoroForge *does* — that a dry run submits nothing, that a failed transaction
  is never recorded, that drift is detected against the ledger rather than the
  database.
- Error messages are part of the interface. If an error tells a user how to fix
  their problem, assert on that.

## Style

- Standard Go. `gofmt` is enforced by `make check`.
- Use `log/slog` for logging. Never log key material.
- Comments should explain *why*, not restate the code. The tricky parts of this
  codebase — manual transaction assembly, the two-transaction deploy, comparing
  against the ledger rather than the database — are commented that way, and
  matching that style helps the next reader.

## Verifying against a changed SDK

Soroban's RPC evolves. After bumping `github.com/stellar/go-stellar-sdk`:

```bash
make test        # catches compile-level breaks
make test-live   # catches response-shape changes the mocks cannot
```

Then update the version table in the README. The mocks will happily keep passing
against an API that no longer exists — that's exactly what the live tests are
for.

## Pull requests

Keep them focused, explain the "why" in the description, and make sure
`make check` passes. If you're changing deploy or upgrade behaviour, say how you
verified it — a testnet dry run counts.

## License

Contributions are licensed under [Apache-2.0](LICENSE).
