-- Smart Campaign Composer schema (SQLite).
--
-- Conventions:
--   * Timestamps are TEXT in fixed-width UTC ISO-8601 ("YYYY-MM-DDTHH:MM:SSZ").
--     Fixed width keeps lexicographic order == chronological order, so range
--     filters like cart_last_active_at >= ? work on plain indexes.
--   * Booleans are INTEGER 0/1.
--   * Enum columns are guarded by CHECK constraints that mirror internal/domain.
--   * Derived, time-relative segment attributes are never stored: the
--     v_users_segment VIEW computes them at query time (schema version 3).

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

-- Segment view (schema version 3). The segment engine (internal/segment,
-- DefaultRegistry) only ever reads this view, never the users table:
--
--   * cart_last_active_days    whole days since the last cart activity,
--                              ROUNDED UP, NULL when the user never used the
--                              cart. Rounding up makes "<= N" mean exactly
--                              "cart activity within the last N x 24h"
--                              (activity 30 days + 1 hour ago is day 31).
--                              Computed in integer seconds, clamped at 0 for
--                              timestamps in the future (clock skew).
--   * has_purchased_since_cart 1 when the last purchase is at or after the
--                              last cart activity, else 0 (also 0 when the
--                              user never used the cart, so the column is
--                              never NULL and NOT_EQUALS has no NULL trap).
--
-- Both depend on 'now', so they cannot be stored columns; a VIEW keeps them
-- correct without a nightly job. It deliberately exposes no PII (name,
-- email, phone): even a bug in the rule compiler could not select them.
-- It is dropped and recreated on every Migrate (views hold no data), so a
-- changed definition always takes effect.
DROP VIEW IF EXISTS v_users_segment;
CREATE VIEW v_users_segment AS
SELECT
    id,
    city,
    age,
    total_spend,
    CASE
        WHEN cart_last_active_at IS NULL THEN NULL
        ELSE MAX(0, (CAST(strftime('%s', 'now') AS INTEGER)
                     - CAST(strftime('%s', cart_last_active_at) AS INTEGER)
                     + 86399) / 86400)
    END AS cart_last_active_days,
    CASE
        WHEN cart_last_active_at IS NOT NULL
         AND last_purchase_at IS NOT NULL
         AND last_purchase_at >= cart_last_active_at THEN 1
        ELSE 0
    END AS has_purchased_since_cart,
    opt_in_sms,
    opt_in_email
FROM users;

CREATE TABLE IF NOT EXISTS campaigns (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    brief      TEXT NOT NULL CHECK (length(trim(brief)) > 0),
    status     TEXT NOT NULL DEFAULT 'draft'
               CHECK (status IN ('draft', 'approved', 'sending', 'completed', 'failed')),
    created_at TEXT NOT NULL,
    -- Canonical, validated segment rule tree (JSON AST, never SQL) the
    -- marketer approved. Re-validated by the segment parser when read.
    -- (Schema version 3; added with ALTER TABLE on older databases.)
    segment_rule TEXT CHECK (segment_rule IS NULL OR json_valid(segment_rule))
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

-- One row per (campaign, user, channel) delivery. The row is inserted as
-- 'pending' BEFORE the provider is called (a claim), then moved to 'sent' or
-- 'failed'. Together with the UNIQUE constraint this makes a second delivery
-- of the same message to the same person impossible, even across workers,
-- concurrent runs and process restarts. (Schema version 2, see db.go.)
CREATE TABLE IF NOT EXISTS dispatch_logs (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    campaign_id         INTEGER NOT NULL REFERENCES campaigns (id) ON DELETE CASCADE,
    user_id             INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    channel             TEXT    NOT NULL CHECK (channel IN ('sms', 'email')),
    status              TEXT    NOT NULL CHECK (status IN ('pending', 'sent', 'failed')),
    provider_message_id TEXT,
    retry_count         INTEGER NOT NULL DEFAULT 0 CHECK (retry_count >= 0),
    last_error          TEXT,
    sent_at             TEXT,
    created_at          TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at          TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    -- Storage-level guarantee that a user never gets the same campaign
    -- message twice on the same channel (matches the idempotency key).
    UNIQUE (campaign_id, user_id, channel),
    -- A sent row always carries its send time.
    CHECK (status <> 'sent' OR sent_at IS NOT NULL)
);

CREATE INDEX IF NOT EXISTS idx_dispatch_logs_campaign_status ON dispatch_logs (campaign_id, status);
