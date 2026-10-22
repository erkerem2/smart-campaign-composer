-- Smart Campaign Composer schema (SQLite).
--
-- Conventions:
--   * Timestamps are TEXT in fixed-width UTC ISO-8601 ("YYYY-MM-DDTHH:MM:SSZ").
--     Fixed width keeps lexicographic order == chronological order, so range
--     filters like cart_last_active_at >= ? work on plain indexes.
--   * Booleans are INTEGER 0/1.
--   * Enum columns are guarded by CHECK constraints that mirror internal/domain.

CREATE TABLE IF NOT EXISTS users (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    first_name          TEXT    NOT NULL,
    last_name           TEXT    NOT NULL,
    email               TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    phone               TEXT    NOT NULL,
    city                TEXT    NOT NULL,
    age                 INTEGER NOT NULL CHECK (age BETWEEN 13 AND 120),
    total_spend         REAL    NOT NULL DEFAULT 0 CHECK (total_spend >= 0),
    cart_last_active_at TEXT,
    last_purchase_at    TEXT,
    opt_in_sms          INTEGER NOT NULL DEFAULT 0 CHECK (opt_in_sms IN (0, 1)),
    opt_in_email        INTEGER NOT NULL DEFAULT 0 CHECK (opt_in_email IN (0, 1)),
    created_at          TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_users_city                ON users (city);
CREATE INDEX IF NOT EXISTS idx_users_cart_last_active_at ON users (cart_last_active_at);
CREATE INDEX IF NOT EXISTS idx_users_last_purchase_at    ON users (last_purchase_at);

CREATE TABLE IF NOT EXISTS campaigns (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    brief      TEXT NOT NULL CHECK (length(trim(brief)) > 0),
    status     TEXT NOT NULL DEFAULT 'draft'
               CHECK (status IN ('draft', 'approved', 'sending', 'completed', 'failed')),
    created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS campaign_messages (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    campaign_id INTEGER NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    channel     TEXT    NOT NULL CHECK (channel IN ('sms', 'email')),
    content     TEXT    NOT NULL,
    subject     TEXT,
    status      TEXT    NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'approved')),
    -- Email always has a subject, SMS never does.
    CHECK ((channel = 'sms' AND subject IS NULL) OR (channel = 'email' AND subject IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS idx_campaign_messages_campaign ON campaign_messages (campaign_id);

CREATE TABLE IF NOT EXISTS dispatch_logs (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    campaign_id         INTEGER NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    user_id             INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    channel             TEXT    NOT NULL CHECK (channel IN ('sms', 'email')),
    status              TEXT    NOT NULL CHECK (status IN ('sent', 'failed')),
    provider_message_id TEXT,
    retry_count         INTEGER NOT NULL DEFAULT 0 CHECK (retry_count >= 0),
    sent_at             TEXT,
    -- Storage-level guarantee that a user never gets the same campaign
    -- message twice on the same channel (matches the idempotency key).
    UNIQUE (campaign_id, user_id, channel)
);

CREATE INDEX IF NOT EXISTS idx_dispatch_logs_campaign_status ON dispatch_logs (campaign_id, status);
