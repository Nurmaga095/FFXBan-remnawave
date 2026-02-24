# Установка агентов FFXBan (Remnawave / 3x-ui)

В репозитории есть два разных агента:

- `ffxban_agent` — для Remnawave-нод (ставит `blocker-worker` + `Vector`)
- `ffxban_agent_3xui` — для 3x-ui-нод (ставит только `Vector`, блокировка через API 3x-ui)

## 1. Агент для Remnawave (`ffxban_agent`)

## Что делает

Скрипт `ffxban_agent/install.sh`:

- устанавливает `nftables` (если отсутствует)
- устанавливает Docker (если отсутствует)
- создаёт nftables table/set для блокировок
- пытается пропатчить `docker-compose` Remnawave, чтобы получить доступ к логам Xray
- ставит `blocker-worker` как `systemd` сервис `ffxban-blocker`
- запускает `Vector` контейнер `ffxban-vector`

## Вариант A (рекомендуется): деплой с Observer-сервера

С Observer-хоста выполните:

```bash
cd ffxban_agent
bash deploy.sh
```

Скрипт:

- соберёт `blocker-worker` из `ffxban_blocker`
- скопирует `blocker-worker` и `install.sh` на ноду
- удалённо запустит установку

### Автоматический запуск (без интерактива)

```bash
cd ffxban_agent
NODE_NAME="Латвия" \
NODE_IP="1.2.3.4" \
NODE_USER="root" \
RABBITMQ_URL="amqp://ffxban_user:StrongPassword@observer.example.com:5672/" \
OBSERVER_DOMAIN="observer.example.com" \
bash deploy.sh
```

## Вариант B: ручная установка на ноде

На ноде (root):

```bash
cd /tmp
# заранее скопируйте сюда install.sh и blocker-worker
NODE_NAME="Латвия" \
RABBITMQ_URL="amqp://ffxban_user:StrongPassword@observer.example.com:5672/" \
OBSERVER_DOMAIN="observer.example.com" \
bash install.sh
```

## Проверка Remnawave-агента

На ноде:

```bash
systemctl status ffxban-blocker
journalctl -u ffxban-blocker -f
docker logs -f ffxban-vector
```

## Важные замечания (Remnawave)

- `NODE_NAME` должен точно совпадать с именем ноды в панели Remnawave.
- На Observer откройте `5672/tcp` только для IP ваших нод (RabbitMQ).
- Скрипт пытается сам найти compose-файл Remnawave и добавить volume для логов; если не найдёт, покажет что добавить вручную.

## 2. Агент для 3x-ui (`ffxban_agent_3xui`)

## Что делает

Скрипт `ffxban_agent_3xui/install.sh`:

- устанавливает Docker (если отсутствует)
- создаёт `Vector` конфиг для чтения `/usr/local/x-ui/access.log`
- запускает контейнер `ffxban-vector-3xui`

`blocker-worker` не ставится: блокировки в 3x-ui режиме выполняются через API 3x-ui.

## Вариант A (рекомендуется): удалённый деплой

```bash
cd ffxban_agent_3xui
bash deploy.sh
```

### Автоматический запуск (без интерактива)

```bash
cd ffxban_agent_3xui
NODE_NAME="Amsterdam" \
NODE_IP="1.2.3.4" \
NODE_USER="root" \
OBSERVER_DOMAIN="observer.example.com" \
bash deploy.sh
```

## Вариант B: ручная установка на 3x-ui ноде

На 3x-ui сервере (root):

```bash
cd /tmp
# заранее скопируйте сюда install.sh
NODE_NAME="Amsterdam" \
OBSERVER_DOMAIN="observer.example.com" \
bash install.sh
```

## Проверка 3x-ui агента

```bash
docker ps --format '{{.Names}}' | grep ffxban-vector-3xui
docker logs -f ffxban-vector-3xui
```

## Важные замечания (3x-ui)

- По умолчанию лог ожидается в `/usr/local/x-ui/access.log`.
- Если путь другой, правьте `LOG_HOST_DIR`/`LOG_FILE` в `install.sh` перед установкой.
- Агент отправляет логи по HTTPS на `https://<OBSERVER_DOMAIN>:38213/` (TLS verify выключен в конфиге Vector по умолчанию).

## Общая диагностика агентов

Проверьте на ноде:

```bash
curl -vk https://observer.example.com:38213/
docker logs --tail=100 ffxban-vector
docker logs --tail=100 ffxban-vector-3xui
```

Проверьте на Observer:

```bash
docker logs --tail=100 ffxban-vector-aggregator
docker logs --tail=100 ffxban
```
