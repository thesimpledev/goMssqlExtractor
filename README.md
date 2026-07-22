# mssqlsnap

Scripts out the schema of a Microsoft SQL Server database — every
object's definition — as one `.sql` file per object, suitable for
committing to git and diffing over time. It connects with a read-only
login, issues only catalog `SELECT`s, and never executes or modifies
anything in the database.

## Why this tool exists

We want our SQL Server schema — stored procedures especially — backed up
in version control. The standard tool for this was Microsoft's
`mssql-scripter`, but it is abandoned (last release ~2018) and its
bundled .NET Core runtime no longer runs on modern Linux: it requires
2016-era ICU and OpenSSL 1.0 libraries that current distributions have
removed. Propping it up meant stacking archived end-of-life libraries
under a dead tool. GUI tools (SSMS, Azure Data Studio) can generate
scripts but can't be automated or run headless on Linux.

So: a small, dependency-light Go binary that does the one job and will
keep compiling for the next decade. It is strictly read-only against the
database — the only SQL it contains is `SELECT` statements against
`sys.*` catalog views.

## Install

Requires Go 1.26+.

```sh
# straight from GitHub (installs as `goMssqlExtractor` in $GOBIN)
go install github.com/thesimpledev/goMssqlExtractor@latest

# or from a clone, picking your own binary name
git clone git@github.com:thesimpledev/goMssqlExtractor.git
cd goMssqlExtractor
go build -o mssqlsnap .            # ./mssqlsnap
go build -o "$(go env GOPATH)/bin/goSql" .   # or install as goSql
```

The result is a single static binary with one direct third-party
dependency, Microsoft's maintained driver
[`github.com/microsoft/go-mssqldb`](https://github.com/microsoft/go-mssqldb)
(plus `golang.org/x/term` for the no-echo password prompt).

## Setting up a read-only login

The only permission the login needs beyond `CONNECT` is
`VIEW DEFINITION` on the database. That both makes every object visible
in the catalog views and lets the login read source text — it grants no
ability to read table data or change anything.

```sql
-- on the server
CREATE LOGIN mssqlsnap_reader WITH PASSWORD = 'change-me';
GO
-- in the target database
USE YourDatabase;
GO
CREATE USER mssqlsnap_reader FOR LOGIN mssqlsnap_reader;
GRANT VIEW DEFINITION TO mssqlsnap_reader;
GO
```

If the login lacks `VIEW DEFINITION`, object definitions come back
`NULL` from `sys.sql_modules`. mssqlsnap detects that and exits with an
error telling you so, rather than writing empty files.

## Usage

The password is never a command-line flag (it would land in shell
history). Set `MSSQL_PASSWORD`, or let the tool prompt for it.

```sh
export MSSQL_PASSWORD='...'
mssqlsnap -server db.example.com -db ds -user mssqlsnap_reader -out schema
```

With a `.env`-sourced environment, no flags are needed at all:

```sh
export MSSQL_HOST=db.example.com   # or host,port
export MSSQL_DB=ds
export MSSQL_USER=mssqlsnap_reader
export MSSQL_PASSWORD='...'
mssqlsnap
```

On success it prints a single summary line and exits 0:

```
ds: 2 schemas, 41 tables, 7 views, 118 procedures, 12 functions, 3 triggers, 2 types, 1 sequences
```

Any failure (unreachable host, bad login, missing permission) exits
non-zero with an error on stderr.

### Flags

| Flag | Env var | Default | Meaning |
|---|---|---|---|
| `-server` | `MSSQL_HOST` | — | Host, or `host,port` |
| `-port` | — | `1433` | TCP port (ignored when `-server` is `host,port`) |
| `-db` | `MSSQL_DB` | — | Database name |
| `-user` | `MSSQL_USER` | — | SQL login |
| — | `MSSQL_PASSWORD` | prompt | Password |
| `-out` | — | `.` | Output directory |
| `-include` | — | all | Comma-separated categories to script |
| `-exclude` | — | none | Comma-separated categories to skip |

Categories: `schemas`, `tables`, `views`, `procedures`, `functions`,
`triggers`, `types`, `sequences`. `-include` and `-exclude` are mutually
exclusive.

```sh
# procedures only
mssqlsnap -include procedures
# everything except triggers
mssqlsnap -exclude triggers
```

Authentication is SQL authentication only. TLS is whatever the driver
negotiates by default (the target is AWS RDS).

## What gets scripted

- **Stored procedures, views, functions, triggers** — the original
  source text, verbatim from `sys.sql_modules`. No reformatting. (Line
  endings are normalized to LF; see below.)
- **Tables** — `CREATE TABLE` generated from the catalog views: columns
  with full type names, nullability, identity, defaults, computed
  columns, non-default collations; primary keys; unique constraints;
  then foreign keys, check constraints, and indexes (including filtered
  and columnstore) as separate statements in the same file.
- **Schemas, user-defined types (alias and table types), sequences.**

System-named constraints (auto-generated names like `PK__T__8CC331600...`)
are emitted without their names, so the same schema scripts identically
across environments that were built separately.

Objects created `WITH ENCRYPTION` have no readable source; they are
skipped with a warning on stderr.

## Output layout

One file per object:

```
<out>/<database>/
    schemas/<schema>.sql
    <schema>/tables/<name>.sql
    <schema>/views/<name>.sql
    <schema>/procedures/<name>.sql
    <schema>/functions/<name>.sql
    <schema>/triggers/<name>.sql
    <schema>/types/<name>.sql
    <schema>/sequences/<name>.sql
```

e.g. `schema/ds/dbo/procedures/GetUserActivity.sql`.

The output is deterministic: stable ordering, stable formatting, LF line
endings, trailing newline. Rerunning against an unchanged database
produces a byte-identical tree, so `git diff` shows only real schema
changes.

**Each run starts by deleting the output subtree it owns** —
`<out>/<database>` for a full run, or just the category directories
being regenerated when `-include`/`-exclude` is used. That way objects
dropped from the database disappear from git instead of lingering. Don't
keep anything else inside `<out>/<database>`.

## Limitations / out of scope

- No data export, no applying or restoring scripts, no diffing, no
  scheduling. Restoring is a human running the scripts through their
  normal tools.
- Not scripted: users/roles/permissions, database-level DDL triggers,
  extended properties, full-text catalogs, partition schemes/functions,
  synonyms.
