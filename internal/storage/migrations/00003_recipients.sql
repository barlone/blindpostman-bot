-- +goose Up
-- IF NOT EXISTS: таблицу можно создать и вручную, но только под ролью приложения.
-- Иначе её владельцем станет другая роль, и приложение получит permission denied:
--	ALTER TABLE recipients OWNER TO postman;
CREATE TABLE IF NOT EXISTS recipients (
    tg_id BIGINT  NOT NULL,
    token UUID    NOT NULL,
    alias VARCHAR DEFAULT NULL
);

-- +goose Down
DROP TABLE recipients;
