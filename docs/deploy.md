# Deploy git-event-hub

A merge to `master` builds the hub image and deploys the Komodo stack `git-event-hub` on the VTC dev server. This file is the operator runbook for the repository side of that pipeline.

Server stack rules: `operator-master/.claude/skills/deploy-vtc-komodo/SKILL.md`.
The same pattern for Agent-Memory: `operator-master/.claude/skills/deploy-agent-memory/SKILL.md`.

Do not run Compose by hand on the server. Do not print a secret.

## Flow

1. A push to `master` starts CI (`.github/workflows/ci.yml`). A deploy starts only after that CI run finishes with success. A failed CI run does not deploy. A pull request does not deploy.
2. GitHub then starts `.github/workflows/deploy.yml` from the default branch. The jobs check out the commit that CI tested (`head_sha`). They accept that commit only when it is a push to `master` in this repository. The deploy workflow checks `deploy/komodo/config/config.yaml` again, then builds.
3. Set a branch protection rule on `master` that requires the CI workflow. That rule is a GitHub setting. A person creates it. This repository does not set it.
4. The build job builds `apps/server/Dockerfile` for `linux/amd64` and pushes `ghcr.io/kevinle128/git-event-hub:<short-sha>`. The tag is the first 7 characters of the commit. The workflow does not push `latest`.
5. The deploy job runs on a self-hosted runner with the label `git-event-hub-deploy`. It runs `deploy/komodo/deploy-revision.py` from the checked-out commit.
6. The script rewrites the line `HUB_BUILD_REVISION=<short-sha>` in the stack environment, runs `DeployStack`, and waits until the hub container is healthy on that tag. It prints the container name, the image and the health.
7. Run the same workflow by hand (`workflow_dispatch`) when you need a commit that CI already passed, or a rollback. A manual run checks the config first. It does not wait for a new CI run. For a commit, pass the input: `gh workflow run deploy.yml --repo kevinle128/git-event-hub -f ref=<sha>`. For a branch or a tag, select it with `--ref <branch-or-tag>`. Leave `ref` empty to use the ref you select in the GitHub UI.

The deploy key can write the stack environment. It cannot write Komodo Variables. Only an admin creates Variables.

## Names

The process reads the names in the first column. The stack environment maps a secret name to a Komodo Variable with `[[NAME]]`. `HUB_BUILD_REVISION` and `HUB_PORT` are literal lines. They are not Variables.

| Environment name | Komodo Variable or GitHub secret | Who creates it |
|---|---|---|
| `KOMODO_API_KEY` | GitHub Actions secret `KOMODO_API_KEY` | Human |
| `KOMODO_API_SECRET` | GitHub Actions secret `KOMODO_API_SECRET` | Human |
| `HUB_BUILD_REVISION` | Literal line in the stack environment | Human once, then the deploy job |
| `HUB_PORT` | Literal line in the stack environment (optional, default `8080`) | Human |
| `POSTGRES_PASSWORD` | `HUB_POSTGRES_PASSWORD` | Human |
| `DATABASE_URL` | `HUB_DATABASE_URL` | Human |
| `GITHUB_WEBHOOK_SECRET` | `HUB_GITHUB_WEBHOOK_SECRET` | Human |
| `GITLAB_WEBHOOK_TOKEN` | `HUB_GITLAB_WEBHOOK_TOKEN` | Human |
| `HUB_API_TOKEN` | `HUB_READ_API_TOKEN` (required; use a strong value) | Human |
| `OMB_GITHUB_EVENT_SECRET` | `HUB_OMB_GITHUB_EVENT_SECRET` | Human |
| `OMB_GITLAB_EVENT_SECRET` | `HUB_OMB_GITLAB_EVENT_SECRET` | Human |

`DATABASE_URL` must use the same password as `POSTGRES_PASSWORD`, the user `postgres`, the database `git-event-hub`, and the host `postgres` (the Compose service name). Use `sslmode=disable` on the Docker network. Example shape, not a real password: `postgres://postgres:replace-me@postgres:5432/git-event-hub?sslmode=disable`.

Paste these lines into the stack environment. Do not replace `[[NAME]]` with a value. Komodo resolves each name from the Variable.

```text
HUB_BUILD_REVISION=0000000
HUB_PORT=8080
POSTGRES_PASSWORD=[[HUB_POSTGRES_PASSWORD]]
DATABASE_URL=[[HUB_DATABASE_URL]]
GITHUB_WEBHOOK_SECRET=[[HUB_GITHUB_WEBHOOK_SECRET]]
GITLAB_WEBHOOK_TOKEN=[[HUB_GITLAB_WEBHOOK_TOKEN]]
HUB_API_TOKEN=[[HUB_READ_API_TOKEN]]
OMB_GITHUB_EVENT_SECRET=[[HUB_OMB_GITHUB_EVENT_SECRET]]
OMB_GITLAB_EVENT_SECRET=[[HUB_OMB_GITLAB_EVENT_SECRET]]
```

The line `HUB_BUILD_REVISION` must exist exactly once before the first deploy. `0000000` is only a placeholder. The first successful deploy replaces it. The script stops when the line is missing or duplicated.

The workflow also uses the built-in `GITHUB_TOKEN` to push the image. That token is not a repository secret you create.

Save the same OMB secret in the OMB Vault for that connector. The default Vault names are `GITHUB_EVENT_HUB_SECRET` and `GITLAB_EVENT_HUB_SECRET` (see the README).

## Runner

A human installs this runner. The workflow does not create it.

1. Install a GitHub Actions self-hosted runner for `kevinle128/git-event-hub` on the VTC dev server. The Komodo API is `http://localhost:9120` on that host. A runner anywhere else cannot call it.
2. Add the label `git-event-hub-deploy`. The workflow requires `self-hosted` and `git-event-hub-deploy`.
3. Run the runner as a user that can run `docker` and `python3`. The deploy script uses both.
4. Suggested layout, the same shape as the Agent-Memory runner: directory `~/actions-runner-git-event-hub`, systemd user unit `github-runner-git-event-hub.service`. Confirm the names on the server.
5. Add the two GitHub Actions secrets `KOMODO_API_KEY` and `KOMODO_API_SECRET`. The key belongs to a Komodo service user with Write (with Logs) on the stack `git-event-hub`. Do not use an admin key.

## First stack

A human does these steps once.

1. Create the GitHub App and the GitLab group webhook token. The README section Human setup lists the permissions, the events and the triggers. Create the webhook URLs only after the Funnel route below exists.
2. Create the Komodo Variables in the table above. Put the values there. Do not commit them.
3. In Komodo, create the stack `git-event-hub` from the repository `kevinle128/git-event-hub`, branch `master`, compose file `deploy/komodo/compose.yml`. Turn server build off. Turn the Komodo webhook off. GitHub Actions deploys this stack.
4. Paste the stack environment lines above and save.
5. Make the GHCR package readable by the server. Agent-Memory publishes its packages as public. This workflow does not set package visibility. If the package is private, give the server a way to pull `ghcr.io/kevinle128/git-event-hub` or the deploy cannot start the new tag.
6. Run the workflow on `master` after the stack, the Variables, the environment lines and the runner exist.

The compose file mounts the config directory from the clone:

`/etc/komodo/stacks/git-event-hub/deploy/komodo/config`

The container reads `/etc/git-event-hub/config.yaml` inside that mount. The mount is the directory, so the hub watcher sees a replaced file. The path must be the Komodo clone of this stack. The directory is in the repository, so the clone contains it. The image user `hub` (uid 10001) must be able to read it.

`deploy/komodo/config/config.yaml` is the production config. It starts with one subscriber (`kevin`) and a placeholder repository list `kevinle128/*` on each source. A human confirms the subscriber accounts, the review-bot accounts and the repository globs before real traffic. `*` does not match `/`. A GitLab project in a subgroup needs a longer glob.

The hub container listens on port 8080. Compose publishes `127.0.0.1:${HUB_PORT:-8080}` on the host. Postgres has no published port. Its data is the named volume `postgres_data` (Docker name `git-event-hub_postgres_data` when the project name is `git-event-hub`).

## Funnel and webhook URLs

Funnel on the VTC dev server uses port 8443. It removes the path prefix, then forwards the request. Give the hub its own prefix, for example `/git-event-hub`. Do not use the prefix `/hooks`. After Funnel removes `/hooks`, the hub would see `/github` instead of `/hooks/github`.

| Item | Value |
|---|---|
| Funnel public URL | `https://vtc.taildae6a9.ts.net:8443/git-event-hub` |
| Forward to | `http://127.0.0.1:8080` (or `127.0.0.1:<HUB_PORT>` when `HUB_PORT` is not 8080) |
| GitHub App webhook URL | `https://vtc.taildae6a9.ts.net:8443/git-event-hub/hooks/github` |
| GitLab group webhook URL | `https://vtc.taildae6a9.ts.net:8443/git-event-hub/hooks/gitlab` |

A human creates the Funnel route and the two webhooks. The hub checks the webhook secret or token. It does not call the GitHub API or the GitLab API.

Funnel also exposes `/healthz`, `/metrics` and `/api/*` through the same public prefix. Set `HUB_API_TOKEN` (`[[HUB_READ_API_TOKEN]]`) to a long random value. `/api/*` and `/metrics` refuse every request when that value is empty, and a weak value is public.

The hub sends events to OpenMausBot at:

- `https://minis-mac-mini.taildae6a9.ts.net:8451/events/github-event`
- `https://minis-mac-mini.taildae6a9.ts.net:8451/events/gitlab-event`

The server must be able to open those URLs on the tailnet. Confirm that path before the first delivery.

## Roll back

For a commit, pass the `ref` input. `--ref` selects a branch or a tag, not a raw commit:

```sh
gh workflow run deploy.yml --repo kevinle128/git-event-hub -f ref=<sha>
```

For a branch or a tag:

```sh
gh workflow run deploy.yml --repo kevinle128/git-event-hub --ref <branch-or-tag>
```

The deploy job checks out that commit and runs `deploy/komodo/deploy-revision.py` from it. A commit older than that script has no script to run, so you cannot deploy it this way.

Every short-sha tag stays in GHCR. The script points `HUB_BUILD_REVISION` at that tag and deploys it. You can also set `HUB_BUILD_REVISION` in the stack environment by hand and deploy the stack from Komodo. Use the 7-character tag that the workflow pushed. To deploy by hand, the image must already be in GHCR. That path does not need the script from the old commit.

## Postgres backup

Back up Postgres before a deploy that adds a migration. A code-only deploy needs no backup. Run this on the server. It does not print the password. Confirm the container name with `docker ps` first. The usual name is `git-event-hub-postgres-1`.

```sh
mkdir -p ~/backups/git-event-hub
docker exec git-event-hub-postgres-1 sh -c 'PGPASSWORD="$POSTGRES_PASSWORD" pg_dump -U postgres -Fc git-event-hub' > ~/backups/git-event-hub/pre-migrate.dump
```

`pg_dump` inside the Postgres image uses the database user from the container. Keep the dump private. The volume stays when you deploy a new hub image.

## Steps that need a human

- Create the GitHub App, install it, and store the webhook secret in `HUB_GITHUB_WEBHOOK_SECRET`.
- Create the GitLab group webhook and store the token in `HUB_GITLAB_WEBHOOK_TOKEN`.
- Create every Komodo Variable in the table, and the two literal environment lines.
- Create the Komodo stack from this repository.
- Create the Funnel route.
- Install the self-hosted runner and add the two GitHub Actions secrets.
- Confirm the subscriber list, the bot accounts and the `repos` globs in `deploy/komodo/config/config.yaml`.
- On GitHub, require the CI workflow in the branch protection rule for `master`.
- Make the GHCR package pullable by the server.
- Back up Postgres before a migration.

The merge to `master` does the build and the deploy after those steps exist.
