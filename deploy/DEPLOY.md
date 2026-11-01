# Деплой в LXC-контейнер

Бот работает как служба systemd. Бинарник статический, Go и другие библиотеки в контейнере
не нужны. Все файлы лежат в одной папке:

| Путь | Владелец и права | Назначение |
|---|---|---|
| `/opt/blindpostman-bot/` | `root:blindpostman`, 0750 | папка службы |
| `/opt/blindpostman-bot/blindpostman-bot` | `root:root`, 0755 | исполнимый файл |
| `/opt/blindpostman-bot/config.ini` | `root:blindpostman`, 0640 | настройки с секретами |
| `/etc/systemd/system/blindpostman-bot.service` | `root:root`, 0644 | файл службы |

Служба работает от пользователя `blindpostman` и не может изменить ни свой бинарник, ни настройки.

## Что нужно заранее

- Контейнер x86-64 с systemd (Debian, Ubuntu). Проверка: `uname -m` выводит `x86_64`,
  а `ps -p 1 -o comm=` — `systemd`. В образе Alpine systemd нет.
- Исходящий доступ к `api.telegram.org:443` и к серверу базы данных из секции `[database]`.
- Входящий доступ к порту из `listen` только с адресов служб, которые шлют алерты. API работает
  по HTTP без TLS, `api_token` передаётся открыто, поэтому не открывайте порт наружу: ограничьте
  его файрволом или поставьте перед ним обратный прокси с TLS.
- База создана (`scripts/create_db.sql`), роль из `config.ini` владеет таблицей `recipients`.
  Остальные таблицы создадут миграции при первом запуске.
- Рядом с репозиторием готов `config.ini` (образец — `config.example.ini`).

Режим поллинга не требует входящих соединений от Telegram и публичного адреса.

## Сборка

На машине разработчика, в корне репозитория. Скрипт собирает бинарник под Linux x86-64 на любой ОС:

```bash
./3_build-app.sh
```

В Windows без Git Bash:

```
3_build-app.cmd
```

В корне появится файл `blindpostman-bot`.

## Первая установка

Далее `CT` — адрес контейнера. Скопируйте файлы:

```bash
scp blindpostman-bot config.ini deploy/blindpostman-bot.service root@CT:/root/
```

В контейнере под root установите пакеты. `ca-certificates` обязателен: без него бот не сможет
проверить сертификат Telegram, а в минимальных образах этого пакета часто нет.

```bash
apt-get update
apt-get install -y ca-certificates curl
```

Создайте пользователя и разложите файлы:

```bash
useradd --system --home-dir /opt/blindpostman-bot --no-create-home --shell /usr/sbin/nologin blindpostman
install -d -o root -g blindpostman -m 0750 /opt/blindpostman-bot
install -o root -g root -m 0755 /root/blindpostman-bot /opt/blindpostman-bot/blindpostman-bot
install -o root -g blindpostman -m 0640 /root/config.ini /opt/blindpostman-bot/config.ini
install -o root -g root -m 0644 /root/blindpostman-bot.service /etc/systemd/system/blindpostman-bot.service
rm /root/blindpostman-bot /root/config.ini /root/blindpostman-bot.service
```

Перед запуском остановите все другие экземпляры с этим же `bot_token`, в том числе локальные.
Telegram разрешает только один поллинг на токен, иначе в журнале появится `409 Conflict`.

```bash
systemctl daemon-reload
systemctl enable --now blindpostman-bot
```

## Проверка

```bash
systemctl status blindpostman-bot
journalctl -u blindpostman-bot -n 30 --no-pager
curl -fsS http://127.0.0.1:8080/healthz
```

В журнале должны быть `database connected`, `bot started`, `http server started` и
`dispatcher started`, а `/healthz` отвечает `{"status":"ok"}`. Порт замените, если в `listen`
указан другой. Затем отправьте боту `/help` в Telegram.

Логи — JSON в журнале systemd. Следить в реальном времени:

```bash
journalctl -u blindpostman-bot -f
```

## Обновление

Соберите новую версию и скопируйте её под временным именем:

```bash
scp blindpostman-bot root@CT:/root/blindpostman-bot.new
```

В контейнере сохраните текущую версию и замените файл. Используйте `install`, а не `cp`:
`cp` поверх запущенного файла завершится ошибкой `Text file busy`.

```bash
cd /opt/blindpostman-bot
cp -p blindpostman-bot blindpostman-bot.prev
install -o root -g root -m 0755 /root/blindpostman-bot.new blindpostman-bot
rm /root/blindpostman-bot.new
systemctl restart blindpostman-bot
```

Сообщения в очереди при перезапуске не теряются: они хранятся в базе. Новые миграции
применяются при старте. После изменения `config.ini` нужен только `systemctl restart`.
Файл службы после изменения установите заново и выполните `systemctl daemon-reload`.

## Откат

```bash
cd /opt/blindpostman-bot
install -o root -g root -m 0755 blindpostman-bot.prev blindpostman-bot
systemctl restart blindpostman-bot
```

Схема базы при этом не откатывается. Если новая версия применила несовместимую миграцию, откатите
её с машины разработчика до запуска старой версии:

```bash
go run ./cmd/migrate -config config.ini down
```

## Дополнительная изоляция

Эти настройки используют пространства имён. В непривилегированном LXC они работают только с
включённым nesting (в Proxmox: Options → Features → nesting), поэтому в основной файл службы не
входят. Добавляются отдельным файлом:

```bash
systemctl edit blindpostman-bot
```

```ini
[Service]
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
```

Если служба перестала запускаться с `status=226/NAMESPACE`, верните всё как было:

```bash
systemctl revert blindpostman-bot
systemctl restart blindpostman-bot
```

## Неполадки

| Симптом в журнале | Причина и решение |
|---|---|
| `status=226/NAMESPACE` | Контейнер не разрешает пространства имён: уберите дополнительную изоляцию или включите nesting |
| `status=203/EXEC` | Бинарник не той архитектуры, собран для другой ОС или нет права на запуск. Пересоберите скриптом и проверьте права |
| `status=217/USER` | Не создан пользователь `blindpostman` |
| `permission denied` на `config.ini` | Проверьте владельца и права из таблицы вверху |
| `permission denied for table` | Таблица принадлежит другой роли. Под суперпользователем: `ALTER TABLE имя OWNER TO postman;` |
| `x509: certificate signed by unknown authority` | Не установлен `ca-certificates` |
| `409 Conflict`, `terminated by other getUpdates` | Запущен второй экземпляр с тем же `bot_token` |
| `password: cannot decrypt` | Значение `password` повреждено при копировании |
| Служба перезапускается каждые 5 секунд | Смотрите первую ошибку `fatal` в журнале: при недоступной базе или Telegram служба сама поднимется, когда они вернутся |
