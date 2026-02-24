# Установка FFXBan (Observer)

Инструкция для центрального сервера FFXBan (Observer), который принимает логи от нод и показывает веб-панель.

## Что устанавливается

- `ffxban` (основной сервис)
- `redis`
- `rabbitmq` (нужен для Remnawave/blocker-агентов)
- `vector-aggregator` (принимает HTTP-батчи логов от агентов)
- `nginx` (TLS + прокси для панели и агентских запросов)

Конфиги находятся в `ffxban_conf/`, исходники сервиса в `ffxban/`.

## Требования

- Linux-сервер (Ubuntu/Debian)
- Docker + Docker Compose plugin
- домен(ы) и TLS-сертификаты (Let’s Encrypt), если используете `ffxban_conf/nginx.conf` как есть
- открытые порты:
  - `443/tcp` (панель и входящий HTTPS)
  - `38213/tcp` (агенты -> Observer через nginx)
  - `5672/tcp` (только для Remnawave blocker-агентов; ограничьте firewall по IP нод)

## 1. Подготовка конфигов

1. Скопируйте пример:

```bash
cp ffxban_conf/.env.example ffxban_conf/.env
```

2. Заполните минимум обязательные поля в `ffxban_conf/.env`:

- `PANEL_PASSWORD`
- `INTERNAL_API_TOKEN`
- `RABBIT_USER`
- `RABBIT_PASSWD`
- `RABBITMQ_URL`

3. При необходимости заполните интеграции:

- `PANEL_URL` / `PANEL_TOKEN` (Remnawave API, динамические лимиты/HWID)
- `ALERT_WEBHOOK_*`
- `EXCLUDED_IPS`

## 2. Настройка nginx (домен и сертификаты)

Файл: `ffxban_conf/nginx.conf`

Замените:

- `server_name observer.example.com ...`
- `server_name panel.observer.example.com`
- пути к сертификатам `/etc/letsencrypt/live/...`

`docker-compose.yml` монтирует `/etc/letsencrypt` с хоста внутрь контейнера nginx, поэтому сертификаты должны лежать на сервере.

## 3. Сборка образа и бинарника

В текущем `ffxban_conf/docker-compose.yml` сервис `ffxban` использует:

- образ `ffxban-remnawave:latest`
- volume `./ffxban-custom:/app/ffxban`

Поэтому нужно подготовить и образ, и бинарник.

### Сборка Docker image

```bash
docker build -t ffxban-remnawave:latest ./ffxban
```

### Сборка Linux-бинарника для volume `ffxban-custom`

```bash
cd ffxban
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ../ffxban_conf/ffxban-custom ./cmd/ffxban
cd ..
chmod +x ffxban_conf/ffxban-custom
```

## 4. Запуск Observer

```bash
cd ffxban_conf
docker compose up -d
```

Проверка:

```bash
docker ps --format 'table {{.Names}}\t{{.Status}}' | grep ffxban
docker logs --tail=50 ffxban
docker logs --tail=50 ffxban-nginx-proxy
docker logs --tail=50 ffxban-vector-aggregator
```

## 5. Доступ к панели

- Панель проксируется через `https://panel.<ваш-домен>/` (по шаблону из `nginx.conf`)
- Health-check: `https://panel.<ваш-домен>/health`

## Режимы работы

## Remnawave mode

Используется по умолчанию:

- лимиты и профили можно подтягивать из Remnawave (`PANEL_URL` / `PANEL_TOKEN`)
- блокировки IP выполняют Remnawave-агенты (`ffxban_agent`) через RabbitMQ + nftables

## 3x-ui mode

Включается через `.env`:

```env
THREEXUI_ENABLED=true
THREEXUI_SERVERS=[{"name":"AMS","url":"https://panel.example.com:2053/secret","username":"admin","password":"StrongPass"}]
THREEXUI_BLOCK_MODE=api_only
```

Пояснения:

- `THREEXUI_SERVERS` — JSON-массив 3x-ui панелей
- `THREEXUI_BLOCK_MODE=api_only` — рекомендуемый стартовый режим
- в 3x-ui режиме блокировки выполняются через API 3x-ui, а не через blocker-worker

Для парсинга логов 3x-ui используйте агент `ffxban_agent_3xui` (см. отдельную инструкцию).

## Обновление

После изменений в коде:

```bash
docker build -t ffxban-remnawave:latest ./ffxban
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ./ffxban_conf/ffxban-custom ./ffxban/cmd/ffxban
chmod +x ./ffxban_conf/ffxban-custom
cd ffxban_conf && docker compose up -d --force-recreate ffxban
```
