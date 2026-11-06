# ledger-api

A small REST service that stores money the way banks do: with double-entry bookkeeping.
I built it to understand what actually goes wrong when many requests touch the same balance at once.

Stack: Go (standard `net/http`), PostgreSQL, [pgx](https://github.com/jackc/pgx).

## What it guarantees

| Problem | How it is handled |
| --- | --- |
| Two withdrawals spend the same money | Row locks (`SELECT ... FOR NO KEY UPDATE`) plus a `CHECK` constraint as a backstop |
| A->B and B->A deadlock each other | Accounts are always locked in id order |
| Client retries after a timeout and pays twice | `Idempotency-Key` header with a `UNIQUE` constraint |
| Money appears from nowhere | Every transfer writes two entries, all balances of a currency sum to zero |
| A bug makes a balance wrong | `GET /reconciliation` compares balances with entries |

Each of these has a test in [`store_test.go`](internal/ledger/store_test.go), including concurrent ones
(30 parallel withdrawals from a balance that only covers 10, 100 opposite transfers, 20 requests with one key).

## Data model

```
accounts            transfers                  entries
--------            ---------                  -------
id                  id                         id
owner               idempotency_key UNIQUE     transfer_id
kind (user/system)  from_account_id            account_id
currency            to_account_id              amount (+ or -)
balance             amount
```

`balance` is a denormalized sum of `entries`, it is there so reading a balance does not need to scan history.
Amounts are `BIGINT` in minor units, never floats.

Deposits come from a `system` account of the same currency. It is the only kind of account allowed to go negative,
and that is why the total for each currency stays zero.

## Run

```bash
make db    # postgres in docker on port 5433
make run   # server on :8080
make test  # tests with the race detector
```

## API

```bash
curl -X POST localhost:8080/accounts -d '{"owner":"alice","currency":"RUB"}'
# {"id":4,"owner":"alice","currency":"RUB","balance":0,"created_at":"..."}

curl -X POST localhost:8080/deposits -H 'Idempotency-Key: dep-1' \
  -d '{"account_id":4,"amount":100000}'

curl -X POST localhost:8080/transfers -H 'Idempotency-Key: tr-1' \
  -d '{"from_account_id":4,"to_account_id":5,"amount":2500}'

curl 'localhost:8080/accounts/4/entries?limit=20&before_id=100'

curl localhost:8080/reconciliation
# {"ok":true,"report":{"mismatches":[],"totals":{"EUR":0,"RUB":0,"USD":0}}}
```

| Status | Meaning |
| --- | --- |
| 201 | transfer done |
| 200 | replay, this key was already processed and nothing moved |
| 400 | bad input |
| 404 | account not found |
| 409 | key was already used with other parameters |
| 422 | not enough money, different currencies, system account |

## Decisions I had to think about

**Why row locks and not `SERIALIZABLE`?** Serializable would also be correct, but then the service needs a retry loop
for serialization failures. Here the set of rows is known in advance, so locking them is simpler.

**Why `FOR NO KEY UPDATE` and not `FOR UPDATE`?** Inserting into `entries` takes `FOR KEY SHARE` on the account because of the
foreign key. `FOR UPDATE` conflicts with that and blocks unrelated inserts, `FOR NO KEY UPDATE` does not.

**Why is the key only saved when the transfer succeeds?** The insert is in the same transaction, so a failed transfer
rolls it back. "Not enough money" is not final, the client can top up and retry with the same key.

**Why keyset pagination?** `OFFSET` gets slower with every page and skips rows when new entries come in.

## Known limitations

- All deposits of one currency lock the same system account, so they run one at a time. For real load I would
  look at sharding this account or recording deposits asynchronously.
- No authentication, anyone can read any account.
- No currency exchange.
- `/reconciliation` scans all entries, it is meant for occasional runs and not for a dashboard.
