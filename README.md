# BlastGuard

PostgreSQL delete-impaction checker. Give it one row and it shows what `DELETE` would hit (cascades,
blockers, SET NULL, triggers). It then generates delete and restore scripts for you to run yourself.
Details: [docs/SPEC.md](docs/SPEC.md).

## Configure

Create `blastguard.yaml` yourself (it is git-ignored, so passwords stay local). Minimal example:

```yaml
tunnel:
  command: "ssh -L 6432:127.0.0.1:5432 apollogix_dev -N"
connections:
  readonly:
    port: 6432
    database: app
    user: ro_user
    password: "change-me"
  becareful:          # full permission account
    port: 6432
    database: app
    user: power_user
    password: "change-me"
```

BlastGuard looks for the config in this order: `-config <path>`, then `./blastguard.yaml` (working
directory), then `<source>/blastguard.yaml` (the parent of `bin/`). The first one found wins. No path under the
home directory is used, so the lookup is the same on macOS, Linux and Windows.

| Key | Notes |
|---|---|
| `connections.readonly` / `.becareful` | `host`, `port`, `database`, `user`, `password` (written directly in the file; required). becareful is optional. |
| `tunnel.command` | One tunnel shared by both connections, e.g. `ssh -L 6432:127.0.0.1:5432 apollogix_dev -N`. Point both `port`s at the local port (`6432`). |
| `tunnel.wait` | How long to wait for the port to open (default `30s`). |
| `tunnel.auto_start` | Start the tunnel without asking when a port is closed (default `true`). With `false`, startup only logs the port status. |
| `cache_dir` | Where the schema/table name cache is written. Empty (default) = `<source>/cache/`; a relative path is taken from the config file's directory. |
| `default_schema` | Default `feight_forwarder`; `""` starts from the schema list. |

Unknown keys (e.g. the old `password_env`) are rejected.

## Compile

```sh
make build        # → bin/blastguard
make build-all    # → bin/blastguard-darwin-{arm64,amd64}
make test         # unit tests; `make testdb test-integration` for the Docker suite
```

## Run

```sh
./bin/blastguard [-config path/to/config.yaml]
./bin/blastguard -version
```

Startup asks for a language and checks the port. If the port is closed and `tunnel.auto_start` is on (default), it starts
the tunnel without asking; otherwise it only logs the status. It then
connects and opens the TUI.

## Inputs

| Input | Where | Action |
|---|---|---|
| text | schema/table list | filter |
| ↑ ↓ PgUp PgDn | anywhere | move / scroll |
| Enter | list / Row ID / option | open, analyze, choose |
| Row ID | table screen | primary-key value of the row to analyze |
| `1..n` / `0` | after a report | generate scripts / cancel |
| Esc · `/back` | anywhere | one level back (cancels a running analysis) |
| `/schema` · `/table` | anywhere | jump to the list |
| `/refresh` | anywhere | rescan schema/table names |
| `/copy [report\|delete\|restore]` | after a report | copy to the clipboard (always copy SQL this way) |
| `/help` | anywhere | list commands |

## Exit

Use `/exit`, `/quit` or Ctrl+C. The last report is printed to the terminal. A tunnel that BlastGuard started is stopped. A tunnel you started
yourself is left alone.
