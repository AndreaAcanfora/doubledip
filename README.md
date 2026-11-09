# doubledip

[![CI](https://github.com/AndreaAcanfora/doubledip/actions/workflows/ci.yml/badge.svg)](https://github.com/AndreaAcanfora/doubledip/actions/workflows/ci.yml)

**Send the same request twice at the same moment, and see what your database does.**

Most web apps check something, then write: *is the coupon unused? then redeem it*;
*is the balance enough? then withdraw*; *is the email free? then sign up*. Run two
of those requests at the same moment and both pass the check. Real users and
attackers hit this every day. Tests almost never do, because the window between the
check and the write is a few milliseconds wide.

doubledip makes the window as wide as it needs to be. It sits between your app and
Postgres, records one request, finds the reads the request acts on, then sends the
request twice and **holds both copies right before their first write** until both
have read. Then it compares the database with what running the two requests one
after the other leaves behind.

```
$ doubledip --db postgres://postgres@localhost/shop --app ./shop --ready http://localhost:8080/health \
    'curl -sS -X POST "localhost:8080/withdraw?account=1&amount=80"'

recorded 3 statements in 3 transactions

✗ RACE CONFIRMED  check-then-act on accounts
  read   tx1  SELECT balance FROM accounts WHERE id = $1  [1]
  write  tx2  UPDATE accounts SET balance = balance - $1 WHERE id = $2  [80, 1]
  both requests read, then both wrote: 2 of 2 reached the write and were held there
  one after the other  #1 exit 0 "withdrew 80"   #2 exit 0 "insufficient funds"
  at the same time     #1 exit 0 "withdrew 80"   #2 exit 0 "withdrew 80"
  database, serial vs concurrent:
    accounts  2 rows serially, 2 rows concurrently
      - (1,20)
      + (1,-60)
    ledger  1 row serially, 2 rows concurrently
      + (1,-80)
  fix  the read (transaction 1) and the write (transaction 2) are in separate transactions: put them in one
       put the check in the write's WHERE clause (e.g. WHERE ... AND balance >= $amount) and treat 0 rows as "rejected"
       or read with SELECT ... FOR UPDATE in the same transaction as the write to accounts

1 race(s) confirmed
```

An account with 100 paid out 160. That is a real run against
[`examples/shop`](examples/shop/main.go).

- **No code changes, any language.** doubledip speaks the Postgres wire protocol.
  Tested with pgx (Go, prepared statements), psycopg2 (Python, simple protocol) and
  node-postgres.
- **A proof, not a warning.** A finding means doubledip reproduced it: it shows the
  rows that no serial order of the two requests produces, and what each request
  answered.
- **It knows a fix when it sees one.** `SELECT ... FOR UPDATE`, a guarded
  `UPDATE ... WHERE balance >= $1`, a unique index or `SERIALIZABLE` come out as
  *safe* or *serialized*, because doubledip actually held both requests and the
  database stayed consistent.

## Install

```sh
go install github.com/AndreaAcanfora/doubledip/cmd/doubledip@latest
```

It needs Go 1.25+ and a C compiler: doubledip parses SQL with the real Postgres
parser ([libpg_query](https://github.com/pganalyze/pg_query_go)).

## Use

```sh
doubledip --db postgres://postgres@localhost:5432/myapp_dev \
  --app 'npm start' --ready http://localhost:3000/health \
  'curl -fsS -X POST localhost:3000/api/coupons/WELCOME/redeem -H "Authorization: Bearer $TOKEN"'
```

- `--db` is your **development** database. doubledip saves its contents, empties
  and refills every table between runs, and puts everything back when it is done.
  It refuses anything but localhost unless you pass `--allow-remote`.
- `--app` starts your app with `DATABASE_URL` (and `PGHOST`/`PGPORT`) pointing at
  doubledip's proxy on `127.0.0.1:6543`. If your app reads its database settings
  differently, point it at that address yourself.
- The last argument is a shell command that sends **one** request. It runs with
  `DOUBLEDIP_N=1` or `2`, so the two copies can differ (`user=u$DOUBLEDIP_N`).
- Give two commands to race two different requests against each other, e.g. a
  withdrawal against a transfer.

Exit status: `0` nothing reproduced, `1` race confirmed, `2` error. It works in CI.

| flag | default | |
|---|---|---|
| `--barrier-timeout` | `3s` | how long the first request waits at the write for the second |
| `--max` | `8` | candidate races to test per run |
| `--ignore` | | `table.column` list left out of comparisons |
| `--json` | | machine-readable report |
| `-v` | | show the app's output |

## How it works

1. **Record.** The request runs once through the proxy. doubledip notes every
   statement, its parameters, its transaction and its isolation level.
2. **Find.** For every `SELECT` followed by an `INSERT`, `UPDATE` or `DELETE` on a
   table it read, it marks the first write after that read as the place to hold
   the requests. Statements are parsed with libpg_query, so ORMs' SQL is fine.
3. **Serial runs.** The two requests run one after the other, in both orders, from
   the same starting data. Those are the correct outcomes. A repeated run spots
   tables that change on their own; those are left out.
4. **Race.** From the same starting data, both requests start together. The proxy
   holds the first one to reach the write (matched by query fingerprint) until the
   second gets there, then lets both go.
5. **Compare.** If the database now holds anything the serial runs never produce,
   the race is confirmed. Generated ids, sequence values, timestamps and random
   defaults are ignored.

| outcome | meaning |
|---|---|
| **confirmed** | both requests got past the check; the data shows it |
| **safe** | both were held at the write, and the result still matched a serial run (a guarded `UPDATE`, a unique index, `SERIALIZABLE`) |
| **serialized** | the second request could not reach the write while the first waited there (a row lock such as `FOR UPDATE`) |

doubledip reports three patterns: **lost update** (read a value, write a new one
computed in the app), **check-then-act** (check a value, then change it relative to
itself without re-checking) and **check-then-insert** (check that something does not
exist, then create it).

## Limits

- Postgres only, plain TCP to the database (no TLS, no Unix sockets) and HTTP or
  anything you can drive from a shell command.
- The request must be repeatable from the same data: a request that needs a fresh
  CSRF token or a one-time login link per run needs a wrapper script.
- Races that need the second request to arrive at a different statement, or more
  than two requests, are out of scope for now.
- It restores data through `TRUNCATE` and `INSERT`, as fast as your dev database
  is small. With a superuser it also skips triggers and foreign key checks while
  restoring.
- In-memory caches in your app are not reset between runs.

## Prior art

[ACIDRain](https://www.bailis.org/papers/acidrain-sigmod2017.pdf) (Warszawski and
Bailis, SIGMOD 2017) found 22 of these bugs in 12 e-commerce applications by
analysing database logs. Its [prototype](https://github.com/stanford-futuredata/acidrain)
is research code. doubledip takes the same idea and turns each suspicion into a
reproduced run. [Hermitage](https://github.com/ept/hermitage) documents what each
isolation level allows. [Elle](https://github.com/jepsen-io/elle) checks histories
of databases themselves rather than of apps.

## License

MIT
