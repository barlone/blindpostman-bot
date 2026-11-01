-- +goose Up
-- Имя id занято числовым первичным ключом, на который ссылаются колонки subscriber_id,
-- поэтому он становится internal_id, а id теперь называется бывший токен получателя.
ALTER TABLE recipients RENAME COLUMN id TO internal_id;
ALTER TABLE recipients RENAME COLUMN token TO id;
ALTER TABLE recipients RENAME CONSTRAINT recipients_token_key TO recipients_id_key;

CREATE TABLE recipient_groups (
    id    UUID    NOT NULL DEFAULT gen_random_uuid(),
    name  VARCHAR NOT NULL,
    alias VARCHAR NOT NULL,
    CONSTRAINT recipient_groups_pk PRIMARY KEY (id),
    CONSTRAINT recipient_groups_unique UNIQUE (name),
    CONSTRAINT recipient_groups_unique_2 UNIQUE (alias)
);

-- Состав групп: удаление группы или получателя убирает и его записи здесь.
CREATE TABLE recipient_group_set (
    group_id     UUID NOT NULL,
    recipient_id UUID NOT NULL,
    CONSTRAINT recipient_group_set_pk PRIMARY KEY (group_id, recipient_id),
    CONSTRAINT recipient_group_set_group_fk FOREIGN KEY (group_id)
        REFERENCES recipient_groups (id) ON DELETE CASCADE,
    CONSTRAINT recipient_group_set_recipient_fk FOREIGN KEY (recipient_id)
        REFERENCES recipients (id) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE recipient_group_set;
DROP TABLE recipient_groups;

ALTER TABLE recipients RENAME CONSTRAINT recipients_id_key TO recipients_token_key;
ALTER TABLE recipients RENAME COLUMN id TO token;
ALTER TABLE recipients RENAME COLUMN internal_id TO id;
