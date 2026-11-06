CREATE TABLE accounts (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    owner      TEXT        NOT NULL CHECK (owner <> ''),
    kind       TEXT        NOT NULL DEFAULT 'user' CHECK (kind IN ('user', 'system')),
    currency   CHAR(3)     NOT NULL CHECK (currency IN ('RUB', 'USD', 'EUR')),
    balance    BIGINT      NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- last line of defence, the service checks the balance first
    CONSTRAINT user_balance_not_negative CHECK (kind = 'system' OR balance >= 0)
);

-- one "outside world" account per currency, deposits come from here
CREATE UNIQUE INDEX accounts_one_system_per_currency ON accounts (currency) WHERE kind = 'system';

INSERT INTO accounts (owner, kind, currency) VALUES
    ('system', 'system', 'RUB'),
    ('system', 'system', 'USD'),
    ('system', 'system', 'EUR');

CREATE TABLE transfers (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    idempotency_key TEXT        NOT NULL UNIQUE CHECK (idempotency_key <> ''),
    from_account_id BIGINT      NOT NULL REFERENCES accounts (id),
    to_account_id   BIGINT      NOT NULL REFERENCES accounts (id),
    amount          BIGINT      NOT NULL CHECK (amount > 0),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT different_accounts CHECK (from_account_id <> to_account_id)
);

-- every transfer writes two rows: -amount and +amount
CREATE TABLE entries (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    transfer_id BIGINT      NOT NULL REFERENCES transfers (id),
    account_id  BIGINT      NOT NULL REFERENCES accounts (id),
    amount      BIGINT      NOT NULL CHECK (amount <> 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- statement query: WHERE account_id = $1 AND id < $2 ORDER BY id DESC LIMIT $3
CREATE INDEX entries_account_id_id_idx ON entries (account_id, id DESC);
CREATE INDEX entries_transfer_id_idx ON entries (transfer_id);

-- accounts whose stored balance does not match their entries, should always be empty
CREATE VIEW balance_mismatches AS
SELECT a.id AS account_id,
       a.balance,
       COALESCE(SUM(e.amount), 0)::BIGINT AS entries_sum
FROM accounts a
LEFT JOIN entries e ON e.account_id = a.id
GROUP BY a.id
HAVING a.balance <> COALESCE(SUM(e.amount), 0);
