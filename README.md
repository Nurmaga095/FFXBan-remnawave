# FFXBan

<p align="center">
  <b>Self-hosted anti-sharing платформа для VPN-инфраструктуры</b><br>
  Анализирует подключения, выявляет подозрительное одновременное использование аккаунтов и управляет блокировками через RabbitMQ + nftables.
</p>

---

## Содержание

- [Что это](#что-это)
- [Ключевые возможности](#ключевые-возможности)
- [Как работает система](#как-работает-система)
- [Архитектура](#архитектура)
- [Структура репозитория](#структура-репозитория)
- [Пошаговая установка и настройка (FFXBan)](#пошаговая-установка-и-настройка-ffxban)
- [Установка и подключение нод (агенты)](#установка-и-подключение-нод-агенты)
- [Проверка после установки (чек-лист)](#проверка-после-установки-чек-лист)
- [SSH-управление нодами из админки](#ssh-управление-нодами-из-админки)
- [Основные переменные окружения](#основные-переменные-окружения)
- [API и интерфейс](#api-и-интерфейс)
- [Мониторинг и диагностика](#мониторинг-и-диагностика)
- [Безопасность](#безопасность)
- [Обновление](#обновление)

---

## Что это

**FFXBan** — это центральный сервис (FFXBAN) + агент на нодах (Blocker + Vector), который:

- собирает события подключений пользователей с нод;
- ведет runtime-состояние в Redis;
- применяет правила анти-шаринга и анти-false-positive;
- отправляет команды блокировки/разблокировки IP на ноды через RabbitMQ;
- применяет блокировки на нодах в `nftables`;
- показывает всё в веб-панели в реальном времени.

---

## Ключевые возможности

- Детекция одновременного использования подписки (sharing detection).
- Runtime banlist/permanent ban, эскалация банов.
- Учет сетевого типа IP (mobile/wifi/cable/unknown) для снижения ложных срабатываний.
- Эвристики против ложных блокировок.
- Гео-анализ (опционально): подозрение при одновременных сессиях из разных стран.
- Подтвержденные статусы блокировок и heartbeat от blocker-нод.
- Веб-панель с live-обновлениями (WebSocket + polling fallback).
- Массовое и точечное SSH-управление нодами прямо из панели.
- Runtime overrides конфигурации из UI (без ручного редактирования `.env`).

---

## Как работает система

1. Нода пишет логи подключений (Xray/Remnawave) в файл.
2. `Vector` на ноде парсит лог, формирует JSON и отправляет на сервер FFXBan (`/log-entry` через nginx).
3. `Vector Aggregator` на сервере FFXBan принимает батчи и проксирует в `ffxban`.
4. `ffxban` сохраняет и анализирует состояние в Redis (IP, TTL, триггеры, статусы, события).
5. При необходимости `ffxban` публикует команду block/unblock в RabbitMQ.
6. `blocker-worker` на каждой ноде читает очередь и применяет `nft`-команды в `nftables set`.
7. Нода отправляет heartbeat/action-отчеты обратно в статусный exchange.
8. Панель отображает текущую картину: пользователи, нарушения, бан-лист, состояние нод, SSH-вывод.

---

## Архитектура

```mermaid
flowchart LR
  U[Пользователи VPN] --> N[VPN ноды]
  N --> VL[Vector на ноде]
  VL -->|HTTPS /log-entry| NX1[Nginx FFXBan]
  NX1 --> VA[Vector Aggregator]
  VA --> OBS[FFXBan API]

  OBS <--> R[(Redis)]
  OBS -->|block/unblock| MQ[(RabbitMQ)]
  MQ --> BW1[blocker-worker нода A]
  MQ --> BW2[blocker-worker нода B]
  BW1 --> NFT1[nftables set]
  BW2 --> NFT2[nftables set]

  BW1 -->|status/heartbeat| MQS[(status exchange)]
  BW2 -->|status/heartbeat| MQS
  MQS --> OBS

  ADM[Админ] --> NX2[Nginx Panel]
  NX2 -->|/panel, /api/*, /api/ws| OBS
```

---

## Структура репозитория

```text
ffxban/                # основной сервис (API, панель, обработка, Redis/RabbitMQ интеграции)
ffxban_conf/           # docker-compose и конфиги FFXBan (nginx, vector, env-example)
ffxban_agent/          # скрипты deploy/install/uninstall для нод
ffxban_blocker/        # исходники blocker-worker (применяет nftables-команды)
ffxban_blocker_conf/   # пример docker-compose и vector-конфиг для blocker-ноды
docs/                  # служебные документы/ассеты
```

---

## Пошаговая установка и настройка (FFXBan)

Ниже инструкция для “чистой” установки центрального сервера FFXBan.

### Шаг 0. Что нужно заранее

- Сервер с Linux (Ubuntu/Debian).
- Домен(ы), которые укажете в `nginx.conf`.
- TLS-сертификат(ы) для этих доменов (например Let's Encrypt).
- Открытые порты на сервере FFXBan:
  - `443/tcp` — панель и входящий HTTPS;
  - `38213/tcp` — прием логов от нод через nginx;
  - `5672/tcp` — RabbitMQ для blocker-нод (лучше ограничить только IP нод).

### Шаг 1. Установить Docker

Если Docker уже стоит, этот шаг пропустите.

```bash
curl -fsSL https://get.docker.com | sh
sudo systemctl enable docker
sudo systemctl start docker
docker --version
docker compose version
```

### Шаг 2. Подготовить проект

Клонируйте репозиторий (любой из вариантов):

**Вариант 1 (SSH):**
```bash
git clone git@github.com:<GITHUB_USERNAME>/<REPOSITORY_NAME>.git FFX
cd FFX
```

**Вариант 2 (HTTPS):**
```bash
git clone https://github.com/<GITHUB_USERNAME>/<REPOSITORY_NAME>.git FFX
cd FFX
```

### Шаг 3. Подготовить `.env`

```bash
cp ffxban_conf/.env.example ffxban_conf/.env
```

Обязательно заполните в `ffxban_conf/.env`:

- `PANEL_PASSWORD`
- `INTERNAL_API_TOKEN`
- `RABBIT_USER`
- `RABBIT_PASSWD`
- `RABBITMQ_URL` (должен совпадать с user/password выше)
- `EXCLUDED_IPS` (добавьте IP нод и служебные IP, которые нельзя блокировать)

Быстрая генерация секретов:

```bash
openssl rand -hex 32   # INTERNAL_API_TOKEN
openssl rand -hex 24   # пароль панели или RabbitMQ пароль
```

Минимальный рабочий фрагмент `.env`:

```dotenv
PANEL_PASSWORD=CHANGE_ME_STRONG_PASSWORD
INTERNAL_API_TOKEN=CHANGE_ME_INTERNAL_API_TOKEN
RABBIT_USER=ffxban
RABBIT_PASSWD=CHANGE_ME_RABBIT_PASSWORD
RABBITMQ_URL=amqp://ffxban:CHANGE_ME_RABBIT_PASSWORD@rabbitmq:5672/
EXCLUDED_IPS=127.0.0.1
```

### Шаг 4. Настроить `nginx.conf`

Файл: `ffxban_conf/nginx.conf`

Замените:

- `ffxban.example.com` / `panel.ffxban.example.com`
- пути к сертификатам `/etc/letsencrypt/live/...`

Проверьте, что сертификаты реально существуют на сервере:

```bash
ls -la /etc/letsencrypt/live/<ваш_домен>/
```

### Шаг 5. Собрать сервис

FFXBan в текущем compose использует **и образ**, и локальный бинарник `ffxban-custom`.

```bash
docker build -t ffxban-remnawave:latest ./ffxban
cd ffxban
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ../ffxban_conf/ffxban-custom ./cmd/ffxban
cd ..
chmod +x ffxban_conf/ffxban-custom
```

### Шаг 6. Запустить контейнеры

```bash
cd ffxban_conf
docker compose up -d
```

### Шаг 7. Проверить запуск

```bash
docker ps --format 'table {{.Names}}\t{{.Status}}'
docker logs --tail=80 ffxban
docker logs --tail=80 ffxban-nginx-proxy
curl -k https://panel.<ваш-домен>/health
```

Ожидаемо в `docker ps`:

- `ffxban`
- `ffxban-nginx-proxy`
- `ffxban-rabbitmq`
- `ffxban-vector-aggregator`
- `ffxban-redis`

### Шаг 8. Первый вход в панель

- Откройте `https://panel.<ваш-домен>/`
- Введите `PANEL_PASSWORD` из `.env`.

---

## Установка и подключение нод (агенты)

Ниже самый понятный и быстрый путь: запуск `deploy.sh` с сервера FFXBan.

Скрипты агента:

- [`deploy.sh`](./ffxban_agent/deploy.sh) — удалённый деплой на ноду по SSH.
- [`install.sh`](./ffxban_agent/install.sh) — ручная установка на ноде.
- [`uninstall.sh`](./ffxban_agent/uninstall.sh) — удаление агента с ноды.

### Вариант A (рекомендуется): автоматический deploy с сервера FFXBan

Запуск на сервере FFXBan:

```bash
cd FFX/ffxban_agent
NODE_NAME="Latvia" \
NODE_IP="1.2.3.4" \
NODE_USER="root" \
RABBITMQ_URL="amqp://ffxban:пароль@<IP_OBSERVER>:5672/" \
OBSERVER_DOMAIN="ffxban.example.com" \
bash deploy.sh
```

Что сделает скрипт:

- соберет `blocker-worker`;
- передаст бинарник и `install.sh` на ноду;
- установит systemd-сервис blocker;
- поднимет `ffxban-vector` в Docker на ноде;
- настроит парсинг логов и отправку на сервер FFXBan.

Проверка на ноде:

```bash
systemctl status ffxban-blocker --no-pager
docker ps | grep ffxban-vector
journalctl -u ffxban-blocker -n 50 --no-pager
docker logs --tail=50 ffxban-vector
```

### Вариант B: ручная установка на ноде

Если нужно ставить вручную:

1. Скопируйте на ноду файлы `install.sh` и бинарник `blocker-worker`.
2. Запустите:
```bash
sudo NODE_NAME="Latvia" \
RABBITMQ_URL="amqp://ffxban:пароль@<IP_OBSERVER>:5672/" \
OBSERVER_DOMAIN="ffxban.example.com" \
bash install.sh
```

Удаление агента:

```bash
sudo bash uninstall.sh
```

---

## Проверка после установки (чек-лист)

Пройдите по порядку:

1. В панели `Nodes` нода отображается и heartbeat обновляется.
2. На сервере FFXBan в логах нет постоянных ошибок RabbitMQ/Redis.
3. На ноде `ffxban-blocker` в статусе `active (running)`.
4. На ноде контейнер `ffxban-vector` запущен.
5. В панели появляются свежие события в логах пользователей.
6. Тестовая блокировка из панели применяет IP в `nftables` на ноде.

Проверка `nftables` на ноде:

```bash
sudo nft list set inet firewall user_blacklist
```

---

## SSH-управление нодами из админки

Поддерживается:

- массовое выполнение команд на выбранных нодах;
- выполнение на одной ноде;
- интерактивный terminal (WebSocket);
- credentials per-node (user/password/private key) через UI.

Ключевые env:

- `NODE_SSH_ENABLED=true`
- `NODE_SSH_USER=...`
- `NODE_SSH_PASSWORD=...` или `NODE_SSH_PRIVATE_KEY=...`
- `NODE_SSH_COMMAND_TIMEOUT_SECONDS`
- `NODE_SSH_MAX_PARALLEL`

Важно:

- long-running SSH exec обслуживается отдельным nginx location `/api/nodes/ssh/exec` с увеличенными таймаутами;
- серверный HTTP `WriteTimeout` увеличен, чтобы не рвать длинные ответы.

---

## Основные переменные окружения

Полный список: `ffxban_conf/.env.example`.

### Безопасность и доступ

- `PANEL_PASSWORD` — пароль админ-панели (обязательно).
- `INTERNAL_API_TOKEN` — токен внутренних API (рекомендуется обязательно).

### Очереди и storage

- `RABBITMQ_URL`, `RABBIT_USER`, `RABBIT_PASSWD`
- `REDIS_URL`

### Логика детекции

- `MAX_IPS_PER_USER`
- `SHARING_*`
- `TRIGGER_*`
- `BANLIST_THRESHOLD_SECONDS`
- `BAN_ESCALATION_*`
- `GEO_SHARING_*`

### Сетевые эвристики

- `NETWORK_DETECT_ENABLED`
- `NETWORK_LOOKUP_PROVIDER`, `NETWORK_LOOKUP_URL`, `NETWORK_LOOKUP_TOKEN`
- `NETWORK_POLICY_ENABLED`
- `NETWORK_*_GRACE_IPS`

### Панель/интеграции

- `PANEL_URL`, `PANEL_TOKEN` (если используете внешнюю панель-источник лимитов)
- `ALERT_WEBHOOK_*`
- `PROMETHEUS_ENABLED`

---

## API и интерфейс

Основные публичные endpoints FFXBan:

- `POST /log-entry` — входящий поток логов.
- `POST /node-heartbeat` — heartbeat ноды.
- `GET /health` — healthcheck.
- `GET /metrics` — Prometheus метрики.
- `GET /panel` — UI панели.

Основные внутренние endpoints панели (`/api/*`, требуется авторизация):

- статистика, пользователи, логи, banlist, permanent sharing;
- управление блокировками (`/actions/block`, `/actions/unblock`, `/actions/clear`);
- node ssh exec/creds/terminal;
- runtime config overrides.

Live-обновления:

- `GET /api/ws` (WebSocket), fallback — polling.

---

## Мониторинг и диагностика

### Полезные команды

```bash
# FFXBan server
docker logs -f ffxban
docker logs -f ffxban-nginx-proxy
docker logs -f ffxban-vector-aggregator

# RabbitMQ health
docker exec -it ffxban-rabbitmq rabbitmq-diagnostics ping

# Redis health
docker exec -it ffxban-redis redis-cli ping
```

### Типовые проблемы

1. Нода offline в панели:
`blocker-worker` не отправляет heartbeat, проверьте `journalctl -u ffxban-blocker -f`.

2. Нет входящих логов:
проверьте `Vector` на ноде и доступность `https://ffxban-domain:38213/`.

3. SSH `unable to authenticate`:
проверьте credentials в UI (кнопка с шестеренкой у ноды) или глобальные `NODE_SSH_*`.

4. Ошибки `/api/nodes/ssh/exec`:
проверьте таймауты nginx и серверный `WriteTimeout`, а также сетевую доступность нод.

---

## Безопасность

- Не коммитьте реальные `.env` файлы.
- Не храните ключи/пароли в репозитории.
- Ограничьте доступ к `5672/tcp` только IP ваших нод.
- Не публикуйте `15672` RabbitMQ management в интернет.
- Защищайте `/metrics` и внутренние API через nginx/firewall.
- Используйте длинные случайные токены и периодический rotate секретов.

---

## Обновление

```bash
docker build -t ffxban-remnawave:latest ./ffxban
cd ffxban
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ../ffxban_conf/ffxban-custom ./cmd/ffxban
cd ../ffxban_conf
docker compose up -d --force-recreate ffxban
```

Проверка после обновления:

```bash
docker logs --tail=100 ffxban
curl -k https://panel.your-domain.tld/health
```

---

## Примечание

Проект ориентирован на self-hosted эксплуатацию и администрирование собственных нод.
Перед публикацией в открытый доступ обязательно проверьте репозиторий на секреты и приватные данные инфраструктуры.
