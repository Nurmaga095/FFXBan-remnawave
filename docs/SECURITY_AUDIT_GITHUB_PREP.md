# Подготовка к публикации на GitHub (проверка секретов)

Ниже список локальных файлов, которые нельзя публиковать в текущем виде.

## Найдено (критично)

- `.claude/settings.local.json`
  - содержит SSH-команды с паролями, IP-адреса серверов, токены API.
- `ffxban_conf/.env`
  - содержит реальные токены/пароли (`INTERNAL_API_TOKEN`, `PANEL_TOKEN`, `RABBIT_PASSWD`, `PANEL_PASSWORD`, `ALERT_WEBHOOK_TOKEN`, `NETWORK_LOOKUP_TOKEN`).
- `ffxban_conf/.env.fixed`
  - содержит те же реальные секреты/пароли.

## Найдено (не секрет, но приватная инфраструктура / лучше заменить)

- `ffxban_conf/nginx.conf`
  - содержит ваши реальные домены и пути к сертификатам.

## Что сделать перед публикацией

1. Не коммитить `.claude/` целиком.
2. Не коммитить `ffxban_conf/.env` и `ffxban_conf/.env.fixed`.
3. Использовать только `ffxban_conf/.env.example` (без реальных значений).
4. Заменить реальные домены/IP в `ffxban_conf/nginx.conf` на примеры (`ffx.example.com`, `1.2.3.4`), если хотите полностью обезличить репозиторий.
5. Обязательно перевыпустить (rotate) уже засвеченные секреты:
   - SSH-пароли серверов
   - `INTERNAL_API_TOKEN`
   - `PANEL_TOKEN`
   - `RABBIT_PASSWD` / `RABBITMQ_URL`
   - `ALERT_WEBHOOK_TOKEN`
   - `NETWORK_LOOKUP_TOKEN`

## Быстрая локальная проверка перед `git push`

```powershell
rg -n --hidden -g '!.git/**' -i "password|passwd|token|api[_-]?key|bearer|amqp://|redis://"
rg -n --hidden -g '!.git/**' -P "\b(?:\d{1,3}\.){3}\d{1,3}\b"
```

## Рекомендуемые `.gitignore` записи (если ещё нет)

```gitignore
.claude/
*.local.json
ffxban_conf/.env
ffxban_conf/.env.fixed
*.pem
*.key
ffxban-linux-amd64
ffxban_conf/ffxban-custom
```
