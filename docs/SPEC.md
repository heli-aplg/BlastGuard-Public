# BlastGuard — Spec

> v1 · 2026-10-11 · MVP

BlastGuard is a macOS console tool. You pick **one PostgreSQL row** (schema → table → primary-key value).
It reports what `DELETE` of that row would do and generates a **delete script** and a **restore script**.
You review and run the scripts yourself.

## 1. Principles

| # | Rule |
|---|---|
| P1 | **Never modify the database.** BlastGuard never runs the scripts it generates. |
| P2 | **Never guess.** Every conclusion comes from a catalog or data query. Anything it cannot prove is labeled `UNVERIFIED`. |
| P3 | **Say when it's stuck.** If neither account can run a query, analysis stops with `DEAD END` and each account's error. |
| P4 | **One row only**, identified by a single-column primary key. |

Out of scope: composite-PK or PK-less root tables (alerted), writing scripts to files, logical relations without
declared FKs, AI trigger analysis (stub only, §8).

## 2. Accounts

| Account | Required | Role |
|---|---|---|
| `readonly` | yes | Primary connection. Runs every query. |
| `becareful` | no | Higher-privilege fallback. A query is retried here when readonly fails with `42501` (permission), `57014` (timeout) or `55P03` (lock). Also used to recount tables with RLS and to cross-check triggers. |

Both are still read-only sessions. Each one's password is written directly in the yaml (internal tool).
`PGPASSWORD` and `~/.pgpass` are ignored. Both usually connect through the same local tunnel port (e.g. `6432`).

## 3. Read-only guarantees

1. Session: `default_transaction_read_only=on`, checked after connect. If it isn't `on`, the connection is refused.
2. Every analysis runs in one `REPEATABLE READ READ ONLY` transaction, which always ends in `ROLLBACK`. becareful imports readonly's snapshot.
3. The executor accepts only `SELECT` / `WITH` / `SHOW`. User input is passed only as `$1`. Identifiers come from the catalog and are quoted.
4. Only built-in `pg_catalog` functions are called. `search_path=pg_catalog`.
5. `statement_timeout`, `lock_timeout` and `idle_in_transaction_session_timeout` are set from the config.

## 4. Analysis

- **Catalog:** reads `pg_catalog` only (`information_schema` silently hides rows from low-privilege users). It covers columns, PK, constraints, incoming FKs, triggers, rules, RLS and inheritance.
- **Traversal:** BFS over incoming FKs from the root row. Each child row is read as server-quoted literals.
  - `CASCADE` → rows go into the delete-set, and the traversal continues from them.
  - `RESTRICT` / `NO ACTION` → blocker. Those rows go into a "manual" delete-set.
  - `SET NULL` / `SET DEFAULT` → rows go into the update-set. Their old values are kept for restore.
  - Rows already visited (diamonds, self-references) are counted once.
  - If the total number of affected rows exceeds `max_rows`, the result is `DEAD END`.
- **Verdict:**

| Verdict | When |
|---|---|
| `DEAD END` | A query failed on both accounts, or `max_rows` was exceeded |
| `NOT FOUND` | No such row |
| `UNSUPPORTED` | Root table has a composite PK or no PK |
| `BLOCKED` | Unresolved RESTRICT / NO ACTION rows, or SET NULL into a NOT NULL column (the latter cannot be fixed by script) |
| `CASCADE` | More than one row deleted, or any rows updated |
| `SAFE` | Nothing else is affected |

- **Alerts (`UNVERIFIED`):** DELETE/UPDATE triggers, rules, RLS, inheritance children, SET DEFAULT, disabled FK triggers, trigger counts that differ between the two accounts, and unreadable function source.
- **Triggers:** BlastGuard lists every affected trigger and adds static hints from its source (DML, `EXECUTE`, `RAISE`, `RETURN NULL`, dblink, NOTIFY, …). The conclusion is always "manual review required", never "safe".

## 5. Output

- **Report:** target, verdict, blockers (with a `SELECT` for each), impact tree, triggers, alerts, data sources (fallbacks), row data.
- **Options:** SAFE / CASCADE → delete + restore script. Resolvable BLOCKED → manual children-first script + restore. Otherwise → back.
- **Delete script:**
  - Starts with `BEGIN;` and ends with `-- COMMIT;`, which is commented out on purpose.
  - Each statement has `-- expect N row(s)`.
  - Tables that reference each other in a cycle are deleted in one statement (data-modifying CTE).
- **Restore script:**
  - `INSERT`s run parent → child. Generated columns are skipped, and `OVERRIDING SYSTEM VALUE` is used for identity columns.
  - Then `UPDATE`s put back the values that were SET NULL or SET DEFAULT.
  - The script warns about INSERT/UPDATE triggers that will fire again.

## 6. Cache

`<cache_dir>/<hash>.json` (config `cache_dir`; empty = `<source>/cache/`, i.e. the parent of `bin/`) holds schema and table names. It is used for navigation only; analysis
always queries live data. Rebuild it with `/refresh`.

## 7. User stories

### US1 — Start up (tunnels + connect)

Sequence diagram: [`sequences/us1-startup.mmd`](sequences/us1-startup.mmd)

### US2 — Analyze one row

Sequence diagram: [`sequences/us2-analyze-row.mmd`](sequences/us2-analyze-row.mmd)

### US3 — Generate and copy scripts

Sequence diagram: [`sequences/us3-generate-scripts.mmd`](sequences/us3-generate-scripts.mmd)

### US4 — Refresh metadata

Sequence diagram: [`sequences/us4-refresh-metadata.mmd`](sequences/us4-refresh-metadata.mmd)

### US5 — Exit

Sequence diagram: [`sequences/us5-exit.mmd`](sequences/us5-exit.mmd)

## 8. AI trigger analyzer (TODO)

The `TriggerAnalyzer` interface exists. `StaticAnalyzer` is implemented; `AIAnalyzer` is a stub enabled by
`ai.enabled`. When implemented, its output must be labeled `[AI ANALYSIS – UNVERIFIED]`, must never change the
verdict, and may use only read-only `db.Session` tools.

## 9. Tests

`testdata/fixture.sql` covers multi-level cascades, RESTRICT / NO ACTION, SET NULL / DEFAULT, self-reference,
diamonds, cycles, PK-less and composite-PK tables, triggers, rules, RLS, inheritance, identity and generated
columns, and special types. It also creates the roles `blastguard_ro` and `blastguard_bc`.

When `BLASTGUARD_TEST_DSN` is set, the integration tests run every case. For each one they execute delete, then
restore, and diff the data, inside a rolled-back transaction **on the test DB only**.

## 10. Known limits

- Triggers get static hints only. There is no automatic conclusion about them.
- Data may change between analysis and execution. Use the `-- expect N row(s)` checks.
- Run scripts with the application's `search_path`, because trigger bodies may use unqualified names.
- Only English UI. Report and script text is hard-coded.
