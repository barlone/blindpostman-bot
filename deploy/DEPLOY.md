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

Схема базы при этом не откатывается. Если новая версия применила несовместимую миграцию, сначала
откатите её текущим бинарником, потому что в предыдущей версии этой миграции ещё нет. Служба на это
время должна быть остановлена: иначе она продолжит обращаться к изменившимся таблицам, а при
перезапуске применит миграцию снова.

```bash
cd /opt/blindpostman-bot
systemctl stop blindpostman-bot
./blindpostman-bot -config config.ini migrate status
./blindpostman-bot -config config.ini migrate down
install -o root -g root -m 0755 blindpostman-bot.prev blindpostman-bot
systemctl start blindpostman-bot
```

`migrate down` откатывает одну миграцию, `migrate down-to N` — все до версии `N`.

## Миграции

Миграции встроены в бинарник и применяются при старте службы, поэтому обычно запускать их вручную
не нужно. Команды `migrate` работают с той же базой из `config.ini` и не запускают бота:

```bash
cd /opt/blindpostman-bot
./blindpostman-bot -config config.ini migrate status
./blindpostman-bot -config config.ini migrate version
./blindpostman-bot -config config.ini migrate up
./blindpostman-bot -config config.ini migrate down
./blindpostman-bot -config config.ini migrate down-to 4
```

Файл `config.ini` доступен только `root` и группе `blindpostman`, поэтому запускайте их под `root`.

## Nginx

Nginx принимает HTTPS и передаёт в бота только запросы к `/api/` и `/doc/`. Конфиг —
[blindpostman-bot.nginx.conf](blindpostman-bot.nginx.conf). Он рассчитан на nginx на отдельном
хосте и обращается к контейнеру по `192.168.0.212:8080`, поэтому этот адрес должен быть постоянным.
Если nginx стоит в том же контейнере, замените адрес в `proxy_pass` на `127.0.0.1`, а в `config.ini`
укажите `listen=127.0.0.1:8080`.

### DNS

Зона `dsstalker.online` обслуживается Cloudflare. Создайте там запись `A` с именем поддомена и
внешним адресом хоста с nginx. Порты 80 и 443 этого адреса должны вести на nginx.

Для API выберите режим **DNS only** (серое облако): TLS завершается на вашем nginx, а Cloudflare не
видит `api_token` и тексты алертов. В режиме **Proxied** (оранжевое облако) он расшифровывает весь
трафик, nginx видит адреса Cloudflare вместо адресов служб, поэтому `allow` по IP не работает. Если
он всё же нужен, поставьте SSL/TLS в **Full (strict)**: при **Flexible** запросы зациклятся на
редиректе с HTTP на HTTPS.

### Установка

Команды для Debian и Ubuntu выполняются на хосте с nginx. Сертификат выпускается до установки
конфига, иначе `nginx -t` не найдёт его файлы. `certonly --nginx` не меняет конфиги nginx, а
продлевать сертификат будет таймер certbot.

```bash
apt-get install -y nginx certbot python3-certbot-nginx
certbot certonly --nginx -d blindpostman.dsstalker.online
```

Скопируйте `deploy/blindpostman-bot.nginx.conf` на хост, затем:

```bash
install -m 0644 blindpostman-bot.nginx.conf /etc/nginx/sites-available/blindpostman-bot.conf
ln -s ../sites-available/blindpostman-bot.conf /etc/nginx/sites-enabled/blindpostman-bot.conf
nginx -t
systemctl reload nginx
```

### Проверка

```bash
curl -i https://blindpostman.dsstalker.online/api/v1/subscribers/1
```

Ответ `401` с телом `{"error":"unauthorized"}` приходит от самого бота, значит цепочка работает.
Запрос с `Authorization: Bearer <api_token>` вернёт данные подписчика или `404` с
`subscriber not found`, если такого подписчика нет.

Документация API открывается без токена по адресу `https://blindpostman.dsstalker.online/doc/`.
Если она не нужна снаружи, удалите из конфига блок `location /doc/` или ограничьте его через
`allow` и `deny`.

### Закройте прямой доступ

Порт 8080 отдаёт API по HTTP без TLS. Разрешите его только с адреса nginx, например файрволом
контейнера в Proxmox (Firewall → Add, порт 8080, источник — адрес хоста с nginx, остальное запрещено).

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
| Nginx отвечает `502 Bad Gateway` | Бот не запущен, сменился адрес контейнера или файрвол закрыл 8080 для хоста с nginx |
| `ERR_TOO_MANY_REDIRECTS` | Cloudflare в режиме Flexible: переключите на Full (strict) или DNS only |
| Снаружи отвечает другой сайт, например `404 page not found` на `/doc` и `/api/`, а `curl --resolve` на `127.0.0.1` даёт `200` | На хосте несколько адресов, и другой блок слушает `IP:443`. Запросы к этому IP обрабатывают только блоки с таким же адресом, а не с общим `listen 443`. Укажите в конфиге `listen <внешний IP>:443 ssl;` и перезагрузите nginx |
| `nginx -t`: `socket() [::]:80 failed` | На хосте выключен IPv6: уберите из конфига строки `listen [::]` |
| Служба перезапускается каждые 5 секунд | Смотрите первую ошибку `fatal` в журнале: при недоступной базе или Telegram служба сама поднимется, когда они вернутся |
