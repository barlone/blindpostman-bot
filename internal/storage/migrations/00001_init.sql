-- +goose Up
CREATE TABLE subscribers (
    id          BIGSERIAL PRIMARY KEY,
    telegram_id BIGINT      NOT NULL UNIQUE,
    username    TEXT        NOT NULL DEFAULT '',
    first_name  TEXT        NOT NULL DEFAULT '',
    last_name   TEXT        NOT NULL DEFAULT '',
    auth_token  UUID        NOT NULL UNIQUE,
    status      TEXT        NOT NULL DEFAULT 'active'
                CHECK (status IN ('active', 'unsubscribed', 'blocked')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Использованные nonce из ссылок регистрации: делает ссылку одноразовой.
CREATE TABLE registration_nonces (
    nonce         BYTEA       PRIMARY KEY,
    mode          TEXT        NOT NULL,
    subscriber_id BIGINT      NOT NULL REFERENCES subscribers (id) ON DELETE CASCADE,
    expires_at    TIMESTAMPTZ NOT NULL,
    used_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Одноразовые коды для callback-URL. Хранится только SHA-256 от кода.
CREATE TABLE callback_codes (
    code_hash     BYTEA       PRIMARY KEY,
    subscriber_id BIGINT      NOT NULL REFERENCES subscribers (id) ON DELETE CASCADE,
    nonce         BYTEA       NOT NULL,
    expires_at    TIMESTAMPTZ NOT NULL,
    redeemed_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Исходящие сообщения (алерты).
CREATE TABLE messages (
    id            BIGSERIAL PRIMARY KEY,
    subscriber_id BIGINT      NOT NULL REFERENCES subscribers (id) ON DELETE CASCADE,
    text          TEXT        NOT NULL,
    parse_mode    TEXT        NOT NULL DEFAULT '',
    status        TEXT        NOT NULL CHECK (status IN ('pending', 'sent', 'failed')),
    error         TEXT,
    tg_message_id BIGINT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at       TIMESTAMPTZ
);
CREATE INDEX messages_subscriber_created_idx ON messages (subscriber_id, created_at DESC);

-- Журнал событий жизненного цикла подписчика.
CREATE TABLE events (
    id            BIGSERIAL PRIMARY KEY,
    subscriber_id BIGINT      REFERENCES subscribers (id) ON DELETE SET NULL,
    kind          TEXT        NOT NULL,
    details       JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX events_subscriber_created_idx ON events (subscriber_id, created_at DESC);

-- +goose Down
DROP TABLE events;
DROP TABLE messages;
DROP TABLE callback_codes;
DROP TABLE registration_nonces;
DROP TABLE subscribers;
