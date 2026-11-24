# Notes for agents

Superblink Events is a Go library with a thin server (`cmd/faucet`). Read `README.md` first, then the page in `docs/` for the area you touch.

- `README.md` is the overview for humans and `docs/` holds what the code cannot tell: the contract for webhook receivers, the redelivery guarantees and limits, the security model and the deploy runbooks. Do not restate in either what an agent can read from the code (exported names, interface methods, reply fields, validation rules). When a change alters a documented behaviour or number, update the page in the same PR.
- CI keeps the docs' generated tables, the README's pictures and the Mermaid diagrams current; the `verify-docs` skill in `.agents/skills/` says how to run those checks and refresh each.
- Run the tests with a local Postgres 13 or newer: `FAUCET_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable' go test -race ./...`. Without the variable they fail, as in CI, and `go test -short` skips them; a test takes the server from `internal/pgtest`.
- After you change a migration or a query, run `go -C tools tool sqlc generate -f ../sqlc.yaml` and commit the result.
- The end-to-end checks live in `verify/` and are described by the `verify-superblink-events` skill in `.agents/skills/`.
- A feature with a small, one-way interface to core lives in its own package under `faucet/` (`faucet/redelivery` is the model). Core never imports a feature, features never import each other, a feature's non-test code names only the core identifiers that `coreUses` lists for it, and never `faucet.Service` or `faucet.Deps`, no core file is named like a feature, and `faucet/internal/` holds only helpers that import nothing else from the module (so does `internal/pgtest`, the test database helper that cmd/faucet's tests share). `TestCompartments` in `faucet/compartments_test.go` fails CI when the code breaks this: move the code instead of widening the rule. A new core name for a feature, a new helper or a new adapter is one reviewed entry in `coreUses`, `internals` or `adapters` in that test, and a helper only tests use goes in `testHelpers` too. A feature that stays in core keeps its files under one name prefix (`deliver*.go`).
- Redelivery selection keeps persisted state; no stateless sliding-window sweep (see #10).
- Redelivery never derives a read position or bound from Superblink Events' clock: the two clocks differ, and a position ahead of GitHub's drops the next failure unseen. It uses times that GitHub gave (attempt times, the `Date` header). A run also keeps to its context deadline by construction: the scan has its own time budget, so the asks and the purge always run.
- The scan takes no read bound and no gap from Superblink Events' clock (see #26). That clock is only the time of record that a step gives the store (`ScanStep.Now`). A read bound comes from an attempt time, and a `Date` can only lower it (the first run after an empty list or the migration); the types in `faucet/redelivery/time.go` enforce it. A gap ends at a page `Date` that passed the check (not more than S = 1h behind the newest attempt known), and a gap after a silence starts at the stored `Date` less M = 6h. M and S are the trust window (see docs/redelivery.md, known limits); a change to them needs a design review first.
- No selection shortcut that skips, suppresses or merges an overlap read or an open range (see #22).
- A trust rule for an issuer that serves many tenants (Vercel) pins the tenant's ID, never a name, and Superblink Events never trusts the issuer that every tenant shares (`https://oidc.vercel.com`). Superblink Events refuses GitHub Actions and gitlab.com until a test logs in with their real tokens. A platform's token logs in only through the token exchange grant (RFC 8693), never as a client assertion. The issuer profiles in `faucet/oauth/workload.go` enforce the first two: add a profile instead of loosening one.

## Rules and what enforces them

Each rule names the check that fails when a change breaks it. A rule marked "review" is a judgment call that no check can make. When the operator corrects an agent, fix the mistake and add the rule here. A rule that is already here, that nothing enforces and that is broken again is a repeat: give it a check (or a type, or a design that rules the mistake out) in the same change. Drop a rule once its mistake cannot happen.

| Rule | Enforced by |
|---|---|
| A behaviour or number that a doc states changes with the doc in the same PR | `TestGeneratedTablesAreCurrent`, `TestDocsNameWhatExists`, `TestPicturesMatchTheirSource` (internal/docgen), the number pins (`faucet/docs_internal_test.go`, `cmd/faucet/docs_internal_test.go`, `faucet/redelivery/docs_test.go`), the Mermaid check in `verify/`; prose and `.agents/skills/` text: review |
| A test that needs Postgres takes the server from `internal/pgtest`, and fails without one (`go test -short` skips it) | `pgtest.URL`, `TestOnlyPgtestReadsTheTestDatabaseVariable` (internal/rules) |
| A test never sleeps in real time; it waits for the event, or runs in a `synctest.Test` bubble | `TestNoTestAddsASleepInRealTime` (internal/rules), a ratchet over the sleeps that remain |
| A comment says the rule or reason itself, never the ID of a finding or plan item outside the repository | `TestNoCommentCitesAnIDFromOutsideTheRepository` (internal/rules) |
| Feature compartments under `faucet/` | `TestCompartments` |
| Generated queries are committed | CI `generate` job (`go -C tools tool sqlc generate -f ../sqlc.yaml`) |
| Migrations are numbered 1 to N with no gap | `TestTheMigrationsAreNumberedOneToNWithNoGap` |
| Every list in a tool's input schema declares `maxItems` | `TestEveryListInAnInputSchemaDeclaresMaxItems` |
| Logging goes through `cdr.dev/slog/v3`, never `log/slog` | depguard in `.golangci.yaml` |
| Redelivery takes read positions and bounds from GitHub's times, never from Superblink Events' clock | the types in `faucet/redelivery/time.go`, `TestOnlyRedeliverTimeMakesTheTimesThatTheScanKeepsApart` |
| The trust window M and S changes only after a design review | `TestRedeliveryDocStatesTheDateWindowThatTheConstantsGive` pins the docs; the design review: review |
| Redelivery selection keeps persisted state, and takes no shortcut that skips, suppresses or merges an overlap read or an open range | review |
| A multi-tenant issuer's rule pins the tenant ID; GitHub Actions and gitlab.com stay refused | the issuer profiles in `faucet/oauth/workload.go`, `TestFaucetTrustsOnlyTheIssuerOfOneVercelTeam`, `TestFaucetRefusesARuleForGitHubActionsOrGitLab` |
| A platform's token logs in only through the token exchange grant, never as a client assertion | `TestATokenExchangeCarriesNoSecretAndNoActor` refuses a client assertion in the exchange; the other grants: review |
