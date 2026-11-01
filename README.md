# blind-postman

Telegram-бот для алертинга. Служба (система мониторинга и т. п.) регистрирует своих
пользователей через подписанную ссылку и затем отправляет им сообщения через REST API
по Telegram ID.

## Регистрация

```
Служба                         Пользователь                    Бот
  │ ссылка t.me/<bot>?start=P ──▶ │                              │
  │                               │ /start P ──────────────────▶ │ проверка HMAC, срока, nonce
  │                               │                              │ upsert подписчика в PostgreSQL
  │                               │ ◀──── токен / одноразовый URL │
```

Параметр `P` — 40 символов base64url: версия, режим, срок действия, случайный nonce и
HMAC-SHA256 от общего секрета (`link_shared_secret`). Формат описан в
[pkg/startlink](pkg/startlink/startlink.go); служба на Go может импортировать этот пакет
и генерировать ссылки сама, либо вызвать `POST /api/v1/registration-links`.

Ссылка одноразовая: nonce запоминается в `registration_nonces`. Повторный `/start` по той же
ссылке тем же пользователем разрешён, другим пользователем — нет. По `nonce` служба может
сопоставить ссылку со своим пользователем.

Режимы:

- **token** — бот показывает UUID-токен. Пользователь вводит его в службе, служба вызывает
  `POST /api/v1/subscribers/resolve` и узнаёт `telegram_id`.
- **callback** — бот показывает одноразовую ссылку `<callback_url>?code=...` (действует
  `callback_ttl`). Служба получает `code` и обменивает его на данные подписчика через
  `POST /api/v1/registrations/redeem`.

Команды бота: `/status`, `/stop` (отписаться), `/start` без параметра (возобновить), `/help`.

## REST API

Все запросы к `/api/v1/*` требуют заголовок `Authorization: Bearer <api_token>`.

| Метод | Путь | Назначение |
|---|---|---|
| `POST` | `/api/v1/messages` | Поставить сообщение подписчику в очередь |
| `GET` | `/api/v1/messages/{id}` | Статус доставки сообщения |
| `POST` | `/api/v1/registration-links` | Сгенерировать ссылку регистрации |
| `POST` | `/api/v1/registrations/redeem` | Обменять callback-код на данные подписчика |
| `POST` | `/api/v1/subscribers/resolve` | Найти подписчика по токену |
| `GET` | `/api/v1/subscribers/{telegram_id}` | Статус подписчика |
| `GET` | `/healthz` | Проверка живости (без авторизации) |

Отправка сообщения:

```bash
curl -X POST localhost:8080/api/v1/messages \
  -H "Authorization: Bearer $API_TOKEN" -H "Content-Type: application/json" \
  -d '{"telegram_id": 123456789, "text": "<b>CPU 98%</b> on db-1", "parse_mode": "HTML"}'
```

Поля: `telegram_id` (обязательно), `text` (до 4096 символов), `parse_mode`
(`HTML` | `MarkdownV2` | `Markdown`), `disable_notification`, `ttl_seconds` (сколько
сообщение актуально, по умолчанию `message_ttl`), `token` (необязательная проверка, что
получатель тот самый).

Отправка асинхронная. Ответы: `202` — сообщение в очереди
(`{"message_id": 17, "status": "pending", "expires_at": "..."}`, заголовок `Location`);
`404` — нет такого подписчика; `409` — подписчик отписался или заблокировал бота.

Статус доставки — `GET /api/v1/messages/17`:

```json
{"message_id": 17, "telegram_id": 123456789, "status": "sent", "attempts": 1,
 "telegram_message_id": 4242, "created_at": "...", "sent_at": "..."}
```

| Статус | Значение |
|---|---|
| `pending` | в очереди или в отправке; `next_attempt_at` — когда следующая попытка |
| `sent` | доставлено |
| `failed` | не доставлено: пользователь заблокировал бота, Telegram отверг сообщение (400), исчерпаны попытки или подписчик неактивен; причина в `error` |
| `expired` | не успели доставить до `expires_at` |

## Очередь доставки

Очередь — таблица `messages`, обработчик — [internal/delivery](internal/delivery/delivery.go).

- **Порядок.** Сообщения одному подписчику уходят строго по очереди: воркер берёт только самое
  старое ожидающее сообщение подписчика и только если предыдущее уже завершено.
- **Конкурентность.** Сообщения разным подписчикам отправляются параллельно (`queue_workers`).
  Выборка идёт через `FOR UPDATE SKIP LOCKED` с арендой (`locked_until`), поэтому можно запускать
  несколько реплик.
- **Лимиты Telegram.** Глобальный token bucket (`global_rate`, по умолчанию 25/с при
  лимите Telegram ~30/с) и интервал между сообщениями в один чат (`chat_interval`,
  по умолчанию 1 с; хранится в `subscribers.last_sent_at`, общий для всех реплик). На ответ
  `429` отправка ставится на паузу на `retry_after`, а сообщение возвращается в очередь без
  списания попытки.
- **Ретраи.** Сетевые ошибки и 5xx повторяются с экспоненциальной задержкой (2 с, 4 с, 8 с…
  до 5 мин, ±20%) до `queue_max_attempts` попыток. 403 (бот заблокирован) и 400 не повторяются.
  Пользователь, заблокировавший бота, получает статус `blocked`, его очередь гасится.
- **Актуальность.** Алерт, не доставленный за `message_ttl` (или `ttl_seconds`), получает
  статус `expired`.
- **Задержка.** Триггер на `INSERT` шлёт `NOTIFY`, диспетчер слушает канал и забирает новое
  сообщение сразу; периодический опрос (`queue_poll_interval`) подбирает ретраи.
- **Гарантия — at-least-once.** Если процесс упадёт между отправкой и записью результата,
  после истечения аренды сообщение отправится повторно.

Ограничения: глобальный лимит считается в памяти процесса — при нескольких репликах делите
`global_rate` между ними. Пауза после 429 тоже локальна для реплики.

Ссылка регистрации:

```bash
curl -X POST localhost:8080/api/v1/registration-links \
  -H "Authorization: Bearer $API_TOKEN" -d '{"mode": "token", "ttl_seconds": 3600}'
```

## Хранение

Схемой управляет [goose](https://github.com/pressly/goose). Миграции встроены в бинарник и
применяются при старте ([internal/storage/migrations](internal/storage/migrations)), а версии
хранятся в таблице `goose_db_version`.

Каждая миграция — SQL-файл с секциями `-- +goose Up` и `-- +goose Down`. Применённые файлы не
меняют: любое изменение схемы оформляется новой миграцией.

```bash
go run ./cmd/migrate create add_something  # создать файл новой миграции
go run ./cmd/migrate status                # показать состояние миграций
go run ./cmd/migrate version               # показать текущую версию
go run ./cmd/migrate up                    # применить новые миграции
go run ./cmd/migrate down                  # откатить последнюю
go run ./cmd/migrate down-to 2             # откатить до версии 2
```

После отката запускайте версию приложения без откаченных миграций: текущая применит их снова
при старте.

- `subscribers` — подписчики, токен, статус (`active` / `unsubscribed` / `blocked`)
- `registration_nonces` — использованные ссылки регистрации
- `callback_codes` — одноразовые коды (хранится только SHA-256)
- `messages` — очередь исходящих сообщений и результат доставки
- `events` — журнал: регистрация, погашение кода, смена статуса
- `recipients` — получатели: Telegram ID, токен, необязательный псевдоним

## Запуск

Настройки читаются из `config.ini` (другой путь — флаг `-config`). Образец с описанием всех
параметров — [config.example.ini](config.example.ini). Файл содержит секреты и исключён из git
и из контекста сборки Docker.

```bash
cp config.example.ini config.ini
```

Заполните `api_token`, `bot_token`, `link_shared_secret` и секцию `[database]`, затем запустите:

```bash
go run ./cmd/blind-postman
```

Или в Docker (база данных берётся из `config.ini`, файл монтируется в контейнер):

```bash
docker compose up --build bot
```

Пароль в `[database]` указывается либо как есть, либо зашифрованным в формате `{HEX}`: это
результат `TangleText` из `Autils.pas` в фигурных скобках. Алгоритм портирован в
[pkg/winnow](pkg/winnow/winnow.go) и совместим с `aWinnow.pas`. Это обфускация без ключа, а не
защита: доступ к файлу всё равно нужно ограничивать.

Сгенерировать ссылку без API:

```bash
go run ./cmd/genlink -bot my_alert_bot -mode token -ttl 1h
```

Тесты: `go test ./...`. Тесты хранилища и очереди требуют PostgreSQL и без него пропускаются:

```bash
docker compose up -d postgres
TEST_DATABASE_URL="postgres://postman:postman@localhost:5432/postman?sslmode=disable" go test ./internal/storage -v
```
