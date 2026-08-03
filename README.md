# Xray Checker Modify

Форк [kutovoys/xray-checker](https://github.com/kutovoys/xray-checker) для мониторинга Xray-нод из одной или нескольких подписок. Приложение запускает Xray Core внутри процесса, периодически проверяет доступность прокси, выполняет speedtest, хранит историю и отправляет уведомления в Telegram.

В этой версии дополнительно доступны:

- приватная веб-админка;
- ручное и плановое обновление подписок;
- ручной и плановый speedtest с отдельным Test URL для каждой ноды;
- Nodes Overview с downtime, историей speedtest и GeoIP-сверкой;
- настраиваемое хранение истории speedtest, по умолчанию 60 дней;
- структурированные Telegram-команды, отчёты и уведомления о недоступности;
- ручные и автоматические резервные копии;
- проверяемое восстановление из архива после перезапуска.

## Быстрый запуск

Требования: Docker с Compose либо Go 1.25. При первом старте приложению нужен доступ к интернету для подписок и загрузки `geoip.dat`/`geosite.dat`.

### Локально через Docker

PowerShell:

```powershell
docker build --build-arg ENABLE_UPX=false -t xray-checker:local .

docker run --rm `
  --name xray-checker `
  -p 127.0.0.1:2112:2112 `
  -e SUBSCRIPTION_URL="https://example.com/subscription" `
  -e METRICS_PROTECTED=true `
  -e METRICS_USERNAME=admin `
  -e METRICS_PASSWORD="change-me" `
  -v xray_checker_data:/app/data `
  -v xray_checker_geo:/app/geo `
  xray-checker:local
```

После запуска:

- dashboard: `http://127.0.0.1:2112/`;
- admin: `http://127.0.0.1:2112/admin`;
- metrics: `http://127.0.0.1:2112/metrics`;
- OpenAPI UI: `http://127.0.0.1:2112/api/v1/docs`;
- healthcheck: `http://127.0.0.1:2112/health`.

### Docker Compose и публичная status page

```powershell
Copy-Item docker-compose.yaml.example docker-compose.yaml
```

Создайте `.env`:

```dotenv
SUBSCRIPTION_URL=https://example.com/subscription
METRICS_USERNAME=admin
METRICS_PASSWORD=replace-with-a-long-password
PUBLIC_DOMAIN=status.example.com
ACME_EMAIL=admin@example.com
```

Запуск:

```powershell
docker compose up -d --build
docker compose logs -f xray-checker
```

В примере Caddy публикует только status page и публичные endpoints. Админка, метрики и приватный API снаружи возвращают `404` и остаются доступны через локальный порт `127.0.0.1:2112`.

### Запуск из исходников

```powershell
go run . `
  --subscription-url="https://example.com/subscription" `
  --metrics-protected `
  --metrics-username=admin `
  --metrics-password="change-me"
```

Полный список параметров и переменных окружения:

```powershell
go run . --help
```

## Основные настройки

| Переменная | По умолчанию | Назначение |
| --- | --- | --- |
| `SUBSCRIPTION_URL` | обязательно | URL подписки; несколько значений можно передать повторением CLI-флага |
| `SUBSCRIPTION_UPDATE` | `true` | автоматическое обновление подписки |
| `SUBSCRIPTION_UPDATE_INTERVAL` | `300` | период обновления подписки, секунды |
| `PROXY_CHECK_INTERVAL` | `300` | период проверки доступности, секунды |
| `PROXY_CHECK_METHOD` | `ip` | метод проверки: `ip`, `status` или `download` |
| `PROXY_TIMEOUT` | `30` | timeout обычной проверки, секунды |
| `PROXY_RESOLVE_DOMAINS` | `false` | разворачивать доменные адреса нод в отдельные IP-конфигурации |
| `SPEED_TEST_URL` | OVH 10 MiB | глобальный Test URL по умолчанию |
| `METRICS_PROTECTED` | `false` | защищать интерфейс, API и метрики Basic Auth |
| `METRICS_USERNAME` | пусто, обязательно | имя пользователя Basic Auth для админки и admin API |
| `METRICS_PASSWORD` | пусто, обязательно | пароль Basic Auth для админки и admin API |
| `WEB_PUBLIC` | `false` | публичная status page; требует `METRICS_PROTECTED=true` |
| `WEB_SHOW_DETAILS` | `false` | показывать серверы и локальные proxy-порты на dashboard |
| `TELEGRAM_ENABLED` | `false` | включить Telegram-интеграцию |
| `TELEGRAM_BOT_TOKEN` | пусто | токен Telegram-бота |
| `TELEGRAM_CHAT_ID` | пусто | целевой chat ID |
| `TELEGRAM_MESSAGE_THREAD_ID` | пусто | topic/thread ID |
| `TELEGRAM_ADMIN_IDS` | пусто | разрешённые Telegram user ID через запятую |

Настройки расписания speedtest, глубины истории и поведения Telegram редактируются в `/admin` и сохраняются в volume `data`.

Админка и `/api/v1/admin/*` всегда требуют Basic Auth. Поэтому обычный запуск без явных `METRICS_USERNAME` и `METRICS_PASSWORD` завершается ошибкой; исключение — `RUN_ONCE=true`, когда HTTP-сервер не поднимается. Встроенных логина и пароля больше нет. При `METRICS_PROTECTED=true` та же защита распространяется на dashboard, приватный API и `/metrics`.

## Основные сценарии

### Обновление подписки

Подписка обновляется автоматически либо кнопкой в админке. Checker не изменяет полученные имена нод: например, суффикс `^~2~^`, добавленный Remnawave, будет показан как часть имени.

Нода идентифицируется по `StableID`, а не по имени. Одинаковые имена допустимы, если параметры подключения дают разные `StableID`. При обновлении приложение пытается сохранить прежние ID, статистику и настройки нод; неоднозначные совпадения по одинаковым именам намеренно не склеиваются. Если две конфигурации всё же дают одинаковый `StableID` без учёта регистра, startup или refresh отклоняется до перезапуска Xray — молчаливого смешивания статистики больше нет.

### Speedtest и история

В админке или Telegram можно запустить тест вручную либо настроить расписание. Все точки запуска используют сохранённый `ScheduleConfig`, включая URL, лимит, timeout и concurrency. Для выбранной ноды можно сохранить отдельный Test URL; он имеет приоритет над глобальным URL.

Speedtest удерживает Xray lifecycle-lock от выбора нод до завершения всех замеров. Refresh подписки дождётся активного теста, а новый тест во время restart начнётся только после запуска обновлённой конфигурации. Это исключает обращение к уже остановленным SOCKS-портам.

История хранится по времени. Значение по умолчанию — 60 дней, диапазон настройки — 1–3650 дней. При уменьшении срока устаревшие результаты удаляются.

### Telegram

Бот показывает сначала короткую сводку и проблемы, а технические поля и успешные результаты убирает в раскрываемые детали. На Telegram Bot API с поддержкой Rich Messages используются нативные заголовки, таблицы, списки и `Details`. Для старого или локального Bot API автоматически применяется компактный HTML-вариант; при неоднозначной сетевой ошибке бот не отправляет fallback повторно, чтобы не задвоить сообщение.

Режим отчётов `always` отправляет каждый завершённый тест, включая запуск по расписанию. Режим `issues` отправляет только отчёты с недоступными нодами, ошибками или скоростью ниже заданного порога. Recovery-уведомление хранится как pending до подтверждённой отправки и повторяется после временной ошибки Telegram.

### Резервные копии

Вкладка `Backup` позволяет скачать ZIP и загрузить его для восстановления. Архив содержит persisted JSON-состояние, manifest и SHA-256 каждого файла. JSON проверяется по типизированной схеме и на дублирующиеся ключи. Из Telegram-конфигурации токен, chat ID, thread ID, список администраторов и неизвестные поля в архив не попадают независимо от регистра ключа.

Автоматический архив создаётся при старте, затем ежедневно около `00:05 UTC` в `/app/data/backups`. Хранятся не более семи архивов и не дольше семи дней.

Загруженный архив сначала проверяется и помещается в staging. На следующем запуске восстановление остаётся транзакцией с rollback-копией, пока speedtest, node archive и Telegram не загрузят состояние. Ошибка загрузчика откатывает прежние файлы; оборванная транзакция безопасно завершается или откатывается при следующем startup.

## Постоянные данные

Не запускайте production-контейнер без volume `/app/data`, иначе после пересоздания контейнера пропадут история и настройки.

| Путь | Содержимое |
| --- | --- |
| `data/node_registry.json` | архив нод, downtime и GeoIP-состояние |
| `data/speedtest_results.json` | результаты и история speedtest |
| `data/speedtest_schedule.json` | расписание, срок хранения и Test URL нод |
| `data/telegram_config.json` | редактируемая Telegram-конфигурация |
| `data/node_alert_state.json` | состояние Telegram-уведомлений |
| `data/backups/` | автоматические ZIP-архивы |
| `geo/` | загруженные GeoIP/GeoSite-файлы |

`xray_config.json` генерируется заново и не считается пользовательским состоянием.

## Проверка изменений

На машине с Go:

```powershell
$goFiles = Get-ChildItem -Recurse -Filter *.go | Select-Object -ExpandProperty FullName
gofmt -w $goFiles
go test ./...
go vet ./...
go test -race ./backup ./checker ./speedtest ./telegram ./web
```

Эквивалентная проверка через Docker:

```powershell
docker build --target builder --build-arg ENABLE_UPX=false -t xray-checker-builder .
docker run --rm xray-checker-builder go test ./...
docker run --rm xray-checker-builder go vet ./...
docker build --build-arg ENABLE_UPX=false -t xray-checker:check .
```

## Документация проекта

- [`README.md`](README.md) — установка, запуск, конфигурация и основные сценарии.
- [`PROJECT_CONTEXT.md`](PROJECT_CONTEXT.md) — архитектура, workflow, бизнес-правила и ограничения форка.
- [`AGENTS.md`](AGENTS.md) — правила безопасного внесения изменений и обязательные проверки.
- [`docs/`](docs/) — многоязычный сайт документации upstream; он не является источником истины для особенностей этого форка.

Проект распространяется по лицензии upstream, см. [`LICENSE`](LICENSE).
