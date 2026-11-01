-- Создаёт пользователя и базу postman. Скрипт можно запускать повторно:
-- существующие пользователь и база не изменяются. Таблицы создаёт само приложение
-- миграциями при старте.
--
-- Запуск от суперпользователя:
--
--	psql -U postgres -d postgres -v password=СЕКРЕТ -f scripts/create_db.sql
--
-- Без -v password используется пароль из config.example.ini, подходящий только для разработки.

\set ON_ERROR_STOP on

\if :{?password}
\else
	\set password postman
\endif

-- CREATE DATABASE нельзя выполнить внутри DO-блока, поэтому условие проверяется через \gexec.
SELECT format('CREATE ROLE postman LOGIN PASSWORD %L', :'password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'postman')
\gexec

SELECT 'CREATE DATABASE postman OWNER postman ENCODING ''UTF8'' TEMPLATE template0'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'postman')
\gexec
