# git-event-hub

This file gives context about the project to AI assistants. `AGENTS.md` has the same text. Keep both files identical.

## Project overview

One team-wide hub for pull request events. It receives GitHub App and GitLab group webhooks, verifies them, and turns them into the pr-event v1 contract. It routes each event to the subscriber who opened the PR or MR, stores it in Postgres in one transaction (receipt, events, one outbox row per sink), and delivers it with per-sink retries. It holds only webhook secrets and sink secrets. It never calls the GitHub API or the GitLab API.

`README.md` is the full reference: architecture, contract, routing rules, configuration keys, delivery semantics, HTTP API and setup. Read it before you change behavior.

## Tech stack

- **Server** (`apps/server`): Go, Echo, fx, Viper and validator (configuration), zap, GORM on Postgres, golang-migrate, oapi-codegen, golangci-lint, testify.
- **Database**: Postgres in production. SQLite is for tests only.
- **Web** (`apps/web`, later): React, Vite, Tailwind, shadcn/ui. Today it holds only the generated shell. The login will be VTC SSO.

## Project structure

```
git-event-hub/
├── apps/
│   ├── server/
│   │   ├── cmd/server/       # main: flags -config, -check, -version, -healthcheck
│   │   ├── internal/
│   │   │   ├── source/       # inbound port; adapters githubapp, gitlab
│   │   │   ├── normalize/    # forge payload -> prevents.Event
│   │   │   ├── route/        # owner, subscriptions, filters, drop reasons
│   │   │   ├── store/        # Postgres state (GORM): receipts, events, outbox, log, PR memory
│   │   │   ├── dispatch/     # one dispatcher per sink: claim, deliver, settle, backoff
│   │   │   ├── sink/         # outbound port; adapters omb (OMB event contract v1), webhook
│   │   │   ├── hub/          # Receive, reload, dispatcher lifecycle, pruning
│   │   │   ├── httpapi/      # Echo handlers and bearer check
│   │   │   ├── api/          # GENERATED from api/openapi.yaml
│   │   │   ├── config/       # YAML configuration and live reload
│   │   │   └── app/          # fx wiring; blank imports of the adapters
│   │   ├── pkg/prevents/     # public event contract and review-block parser
│   │   ├── schema/           # pr-event.v1.json and pr-event.v1.d.ts
│   │   ├── migrations/       # embedded migrations: postgres/ and sqlite/
│   │   ├── api/openapi.yaml  # HTTP API contract
│   │   ├── config/           # example.yaml
│   │   └── testdata/         # GitHub and GitLab webhook fixtures
│   └── web/                  # React shell (later)
├── deploy/komodo/            # Komodo stack: compose, production config, deploy script
├── docs/deploy.md            # operator runbook for the Komodo deploy
├── docker-compose.yml        # hub + Postgres 16
└── .github/workflows/        # ci.yml and deploy.yml
```

## Common commands

Run the Go commands in `apps/server`.

- `go run ./cmd/server -config config/example.yaml` - run the hub (needs `DATABASE_URL` and the secret variables that the config names).
- `go run ./cmd/server -check -config config/example.yaml` - validate the config and its secrets (no database needed).
- `go test -race ./...` - all tests on SQLite.
- `TEST_DATABASE_URL=postgres://USER:PASSWORD@HOST:PORT/DB?sslmode=disable go test -race -count=1 ./...` - the same tests on Postgres (one schema for each test).
- `go vet ./...` - vet.
- `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint run ./...` - lint.
- `go generate ./internal/api` - regenerate the API code after you edit `api/openapi.yaml`.
- `docker compose up --build` (in the repository root) - run the hub and Postgres. Copy the two example environment files to `.env` files first (see `README.md`).
- `bun install`, `bun run check-types`, `bun run build` - web app only.

## Rules

- **No plan references.** Do not write plan names, phase numbers, finding codes or audit labels in code, comments, test names, migration names or commit messages. State the reason directly.
- **Regenerate the API code after each spec edit.** Never edit `internal/api/openapi.gen.go` by hand. CI fails when it is out of date.
- **Keep both migration directories in step.** Each change needs a migration in `migrations/postgres` and in `migrations/sqlite` with the same version number, the same tables and the same columns. Only the SQL dialect differs.
- **Back up Postgres before a schema change** on a real database.
- **Secrets are never in the config file, in Git or in logs.** The config names the environment variable (`secretEnv`, `urlEnv`, `tokenEnv`).
- **The contract is versioned.** Keep `pkg/prevents`, `schema/pr-event.v1.json` and `schema/pr-event.v1.d.ts` in step. A removed or renamed field needs a new version. Version 1 may gain optional fields.
- **Adapters register themselves in `init`.** A new source or sink needs one package and one blank import in `internal/app/app.go`, and tests with fixtures in `testdata/`.
- **Run one hub instance only.** The dispatchers are not safe for several replicas.
- **Do not weaken a test** to make it pass. Fix the cause.
- When you change behavior, the config keys or the setup steps, update `README.md` in the same change.

## Better Fullstack project context

`bts.jsonc` is the authority for the current Stack Graph. Its `stackParts` array owns role selection and `ownerPartId` bindings. Top-level option fields are a compatibility projection and must not become a second mutation path.

Use `bts.jsonc` for the generator and schema version. Use local package manifests and lockfiles for installed dependency versions. User code outside an explicit Better Fullstack managed region is not generator-owned. Run `create-better-fullstack doctor --json` before you repair graph drift.

<!-- <better-fullstack:recipes sha256=e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855> -->

<!-- </better-fullstack:recipes> -->

## Maintenance

Update this file when you:

- add or remove dependencies,
- change the project structure,
- add a feature or a service,
- change the build or development workflow.
