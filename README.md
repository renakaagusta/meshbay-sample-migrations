# meshbay-sample-migrations

Go 1.23 + Postgres fixture for Meshbay that proves **versioned migrations run in `pre_deploy`**, and that a
release whose migration fails never replaces the release that is already serving.

No Dockerfile on purpose: this repository exercises the Railpack build path plus a manifest-defined pre-deploy hook.

```
                            deploy of branch X
                                    │
                    ┌───────────────┴────────────────┐
                    │ build image (Railpack, no       │
                    │ Dockerfile, go build → binary)  │
                    └───────────────┬────────────────┘
                                    ▼
                  pre_deploy: sh -c './meshbay-migrations migrate'
                     │                                        │
        exit 0 (schema is up to date)            exit 1 (SQL error, rolled back)
                     │                                        │
                     ▼                                        ▼
        new revision starts on $PORT             rollout stops, the previous
        GET /health → {schema, release}          revision keeps serving traffic
```

## Branches

| Branch | Migrations | `release` in `/health` | Endpoints |
|---|---|---|---|
| `main` | `0001_items`, `0002_items_note` → schema 2 | `v2` | `/health`, `/api/items` |
| `v3-add-tags` | + `0003_tags` → schema 3 | `v3` | `/health`, `/api/items`, `/api/tags` |
| `bad-migration` | + `0004_items_tag_id`, **deliberately broken** | `v3-broken` | items and tags code that needs `items.tag_id` |

`bad-migration` is branched off `v3-add-tags` and adds one more migration, so the failure happens on the **next**
version whatever schema the database is at (3 after the v3 deploy, or 2 on a fresh project after `0003_tags` applies
and `0004_items_tag_id` fails).

## Environment

| Variable | Required | Default | Description |
|---|---|---|---|
| `DATABASE_URL` | yes | none | Postgres connection string, e.g. `postgres://user:password@host:5432/app` |
| `PORT` | no | `8080` | HTTP port to listen on; Meshbay injects it at deploy time |

`meshbay.toml` declares `DATABASE_URL` as a name only (`DATABASE_URL = ""`): the value comes from the project
variable that points at the project's Postgres database. Nothing secret is stored in the repository.

## Endpoints

| Method | Path | Description |
|---|---|---|
| GET | `/health` | `200 {"status":"ok","schema":2,"release":"v2"}`; `schema` is the highest applied migration (`0` when the database was never migrated), `503` when the database is unreachable |
| GET | `/api/items` | JSON array of items, ordered by id |
| POST | `/api/items` | Body `{"name":"...","note":"..."}` → `201` with the created item (`note` optional) |
| GET | `/api/items/{id}` | `200` with the item, `404` when it does not exist |
| PUT | `/api/items/{id}` | Body `{"name":"...","note":"..."}` → `200` with the updated item, `404` when it does not exist |
| DELETE | `/api/items/{id}` | `204`, `404` when it does not exist |

`v3-add-tags` and `bad-migration` also serve:

| Method | Path | Description |
|---|---|---|
| GET | `/api/tags` | JSON array of tags |
| POST | `/api/tags` | Body `{"name":"..."}` → `201`, `409` when the name already exists |

CORS is open (`Access-Control-Allow-Origin: *`).

## Migrations

Plain SQL files in `migrations/`, named `<version>_<name>.sql`, compiled into the binary with `go:embed`. There is no
migration dependency outside the Postgres driver:

```
  migrations/0001_items.sql ─┐
  migrations/0002_items_note.sql ─┤   go:embed ──► /app/meshbay-migrations
  migrations/0003_tags.sql ─┘            │
                                         ▼
                          ./meshbay-migrations migrate   (pre_deploy only)
                                         │
                         ┌───────────────┴────────────────┐
                         │ table schema_migrations        │
                         │ (version, name, applied_at)    │
                         └───────────────┬────────────────┘
                                         │
                    each migration: BEGIN → SQL → insert version → COMMIT
                    failure:       ROLLBACK, exit 1, print the failing file
```

* The server **never migrates on start**. If `pre_deploy` did not run, `/health` reports `"schema":0` and item
  queries fail — which is exactly what the smoke script catches.
* `pg_advisory_lock` serialises concurrent hooks, so raising `replicas` later cannot apply a migration twice.
* Every migration runs in its own transaction together with its `schema_migrations` row, so a failing migration
  (see `0004_items_tag_id` on `bad-migration`) leaves the schema at the previous version.

## meshbay.toml

```toml
[[services]]
name = "api"
pre_deploy = 'binary=./meshbay-migrations; ...; exec "$binary" migrate'
ports = ["8080"]

[services.build]
builder = "railpack"
build = "go build -o meshbay-migrations ."
start = "./meshbay-migrations serve"

[services.healthcheck]
path = "/health"
```

The build writes the binary into the application directory, which is also the runtime working directory, so both the
start command and the pre-deploy hook reach it as `./meshbay-migrations` (`/app/meshbay-migrations` is tried as a
fallback, and the hook exits `127` with `pwd` + `ls` in the log when neither exists).

## Test scenario (what the coordinator runs on the jkt cluster)

1. **Project + database.** Create the project from `renakaagusta/meshbay-sample-migrations`, branch `main`, and give it
   a Postgres database. Set the project variable `DATABASE_URL` to the database URL (this is the value
   `meshbay.toml` only names).
2. **Deploy `main`.** Expected: the deploy log shows the hook
   `migration 0001 0001_items: applied` → `migration 0002 0002_items_note: applied` → `schema version: 2`, then
   `listening on port 8080 (release v2)`. `/health` returns `{"status":"ok","schema":2,"release":"v2"}`.
3. **Smoke `main`.**
   ```sh
   bash scripts/smoke.sh https://<domain> 2 v2
   ```
   Ends with `OK schema=2 release=v2 on https://<domain>`.
4. **Deploy the valid next migration.** Switch the deployed branch to `v3-add-tags` and deploy. Expected: hook applies
   `0003_tags`, `schema version: 3`, `/health` reports `{"schema":3,"release":"v3"}`, `GET /api/tags` returns `[]`.
   ```sh
   bash scripts/smoke.sh https://<domain> 3 v3
   ```
5. **Deploy the broken migration.** Switch the deployed branch to `bad-migration` and deploy. Expected:
   * the deploy **fails**, and the log names the failing file:
     `migration 0004 0004_items_tag_id failed and was rolled back: ERROR: column "tag_note" ... does not exist (SQLSTATE 42703)`;
   * the new revision never serves: `bash scripts/smoke.sh https://<domain> 3 v3` still ends with
     `OK schema=3 release=v3`, and `/api/items` still works although the failing branch's code selects `items.tag_id`;
   * the Meshbay dashboard shows the failed deploy for `bad-migration` while the live release is still `v3`.
6. **Verify the rollback path.** `SELECT version, name FROM schema_migrations;` shows only 1, 2, 3: the failed 0004
   left nothing behind (its `ALTER TABLE items ADD COLUMN tag_id` was rolled back too). Deploy `v3-add-tags` again (or
   merge `v3-add-tags` into `main` after fixing `0004`); the deploy succeeds and `scripts/smoke.sh <url> 3 v3` is green
   again.

A reverse-order check is valid too: deploying `bad-migration` straight after `main` applies `0003_tags`, then fails on
`0004`, leaving the schema at 3 while the running release stays `v2`.

## Run locally

```sh
go mod download
DATABASE_URL=postgres://postgres:postgres@localhost:5432/app go run . migrate
DATABASE_URL=postgres://postgres:postgres@localhost:5432/app PORT=8080 go run . serve
```

Or build the binary the way Railpack does:

```sh
go build -o meshbay-migrations .
DATABASE_URL=... ./meshbay-migrations migrate
DATABASE_URL=... PORT=8080 ./meshbay-migrations serve
```

`scripts/smoke.sh` accepts a local URL as well: `bash scripts/smoke.sh http://localhost:8080 2 v2`.

## Known Meshbay gaps

* **No global variable reference syntax in `meshbay.toml`.** `DATABASE_URL` can only be declared as a name and filled
  from the project variable, so a fixture cannot say "use the `app` database of this project" itself.
* **`pre_deploy` has no working-directory or path contract.** It runs `sh -c` in the image, so this repo guesses the
  binary location (`./meshbay-migrations`, then `/app/meshbay-migrations`). A documented app directory would remove
  the guess.
* **No rollback of schema**, by design of this ticket: migrations here are forward-only, so a rollback only works when
  the previous release can run against the newer schema (all three branches can, `bad-migration` never applied its
  migration).
* **Previews and multi-service networks are out of scope for this repo** (MESH-132..136 cover them).
