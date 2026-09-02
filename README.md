# Xray Checker Modify

Форк [kutovoys/xray-checker](https://github.com/kutovoys/xray-checker) для мониторинга Xray-нод из одной или нескольких подписок. Приложение запускает Xray Core внутри процесса, периодически проверяет доступность прокси, выполняет speedtest, хранит историю и отправляет уведомления в Telegram.

В этой версии дополнительно доступны:

- приватная веб-админка;
- переключаемая EN/RU-локализация dashboard и админки с сохранением выбора в браузере;
- ручное и плановое обновление подписок с защитой от массового удаления нод и rollback Xray;
- ручной и плановый speedtest с отдельным Test URL для каждой ноды;
- Nodes Overview с downtime, историей speedtest, GeoIP-сверкой и безопасным merge retired-ноды после смены ключа;
- persisted-журнал одиночных и массовых инцидентов с диагностическими кодами причин;
- режим обслуживания всего проекта: фоновый мониторинг и уведомления на паузе, трафик и ручные действия работают;
- общий настраиваемый срок хранения историй speedtest и Availability, по умолчанию 60 дней;
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

Язык dashboard и админки переключается кнопками `EN`/`RU` в верхней панели. Выбор хранится в `localStorage` браузера и применяется к обоим экранам; технические названия (`StableID`, `Test URL`, `Remnawave Host`, `Internal Squad`, `External Squad`, `announce`, `reconcile` и подобные) остаются без смыслового перевода.

### Docker Compose и публичная status page

```powershell
Copy-Item docker-compose.yaml.example docker-compose.yaml
Copy-Item Caddyfile.example Caddyfile
Copy-Item .env.example .env
```

`Caddyfile` — локальная конфигурация и игнорируется Git. Версионируемый шаблон находится в `Caddyfile.example`, поэтому доменные или инфраструктурные правки не засоряют `git status`.

Отредактируйте `.env`: это единственный операторский файл с runtime-настройками основного stack-а. `xray-checker` читает его целиком через `env_file`, поэтому добавление или изменение настройки приложения не требует правки Compose. Полный шаблон настроек и прежних Compose-defaults находится в `.env.example`. Caddy получает из `.env` только явный allowlist `PUBLIC_DOMAIN`, `ACME_EMAIL` и `PROBE_TRUSTED_PROXY_SECRET`; Telegram, Remnawave и subscription-секреты в reverse proxy не передаются.

При обновлении существующей установки сначала сравните её `.env` с актуальным `.env.example` и перенесите недостающие параметры, не затирая реальные секреты. Особенно проверьте `WEB_PUBLIC`, `METRICS_PROTECTED`, `METRICS_USERNAME` и `PROXY_CHECK_METHOD`: раньше пример Compose подставлял их самостоятельно.

```dotenv
CONTROLLER_IMAGE=ghcr.io/invisibleproxy/xray-checker-controller:main
SUBSCRIPTION_URL="https://example.com/subscription"
# Для нескольких источников перечислите URL через запятую внутри кавычек:
# SUBSCRIPTION_URL="https://one.example.com/subscription,https://two.example.com/subscription"
WEB_PUBLIC=true
WEB_SHOW_DETAILS=false
METRICS_PROTECTED=true
METRICS_USERNAME=admin
METRICS_PASSWORD=replace-with-a-long-password
PUBLIC_DOMAIN=status.example.com
ACME_EMAIL=admin@example.com
```

Controller image публикуется workflow-ом [`controller-image.yml`](.github/workflows/controller-image.yml) для `linux/amd64` и `linux/arm64`. Push в `main` создаёт теги `main` и `sha-<short>`, а тег `controller-vX.Y.Z` — `X.Y.Z` и `latest`. `:main` удобен для первого запуска, но в production после успешного workflow замените его в `.env` на напечатанный в summary immutable digest:

```dotenv
CONTROLLER_IMAGE=ghcr.io/invisibleproxy/xray-checker-controller@sha256:<digest>
```

Запуск:

```powershell
docker compose pull
docker compose up -d
docker compose logs -f xray-checker
```

Для обновления только controller-а не пересобирайте проект и не останавливайте весь stack:

```powershell
docker compose pull xray-checker
docker compose up -d --no-deps --force-recreate xray-checker
```

Локальная сборка из checkout-а остаётся отдельным сценарием из раздела «Локально через Docker»; build-аргумент `ENABLE_UPX` задаётся непосредственно команде `docker build` и не относится к production `.env`.

В примере Caddy публикует только status page и публичные endpoints. Админка, метрики и приватный API снаружи возвращают `404` и остаются доступны через локальный порт `127.0.0.1:2112`.

### Remote Diagnostics и отдельный Compose probe-agent-а

Remote Diagnostics включает создание агентов из админки, IP-bound enrollment, постоянную Ed25519 identity, подписанный heartbeat, ручную диагностику одной ноды и opt-in автоматическую перепроверку неразрешённого speedtest fallback. В раскрытой карточке active-ноды секция `Remote Diagnostics` создаёт эфемерную generation-bound session; пока session для той же пары `StableID` + agent активна, повторный запуск блокируется. Agent получает задание через подписанный outbound poll и выполняет выбранную диагностику. Отдельный direct-connectivity control определяет, можно ли считать observation достоверным.

Рядом с выбором агента находится выбор диагностики. Задание несёт только ID профиля: endpoint-ы и параметры принадлежат конфигурации агента, поэтому выбор в админке не может превратить job в произвольный fetch. Доступны семь профилей:

| Профиль | Через туннель | Что показывает |
| --- | --- | --- |
| `Endpoint status` | да | код ответа status-endpoint; самая быстрая проверка |
| `Exit IP` | да | сравнение выходного IP с прямым IP агента — ловит трафик мимо туннеля |
| `Download throughput` | да | переданный объём и достигнутые Mbps, в том числе для оборванной передачи |
| `Latency profile` | да | серия коротких запросов: медиана, p95 и джиттер вместо одного замера |
| `Connection stability` | да | удержание передачи, ловит фильтрацию, разрывающую сессию с задержкой |
| `TLS handshake` | нет | прямой handshake с SNI ноды — отличает «порт отвечает» от «сессия не устанавливается» |
| `DNS resolution` | нет | резолвинг имени ноды несколькими резолверами и расхождение между ними |

Туннельные профили поднимают временный embedded Xray и при ошибке добавляют TCP/ping evidence; для `Endpoint status`, `Exit IP`, `Download throughput` и `Latency profile` неудача сначала перепроверяется через резервный endpoint, что отличает недоступный endpoint от недоступной ноды. Транспортные профили Xray не запускают вообще — они идут к ноде напрямую, иначе туннель в пути скрыл бы именно то, что они ищут.

Профили гейтятся по capability агента: `Endpoint status`, `Exit IP` и `Download throughput` требуют `diagnostic-v1`, остальные — `diagnostic-v2`. Агент старой сборки видит новые профили в списке неактивными, а controller отклоняет такой запрос с понятной ошибкой вместо невнятного configuration-сбоя. Пустой выбор сохраняет профиль, соответствующий `PROXY_CHECK_METHOD` controller-а.

Remote observation никогда не меняет Availability, history, downtime, incidents, speedtest/retry или Remnawave и не создаёт самостоятельный Telegram alert. Автоматический слой может только добавить вероятностную строку к speedtest-алерту, решение об отправке которого уже принято по локальному результату. Session хранится только в памяти controller-а, экспортируется отдельным sanitized JSON и исчезает после restart. Единственный multi-agent workflow — периодический sweep достижимости, описанный ниже; он тоже ничего не пишет в operational state.

Автоматика запускается только после фактически выполненного country fallback: резервы исчерпаны техническими ошибками либо финальная скорость всё ещё ниже Telegram threshold. Retry ставится в очередь до ожидания агента; одна healthy idle probe выбирается автоматически, а одинаковый `StableID` дедуплицируется на время cooldown. Вердикт «воспроизвелось» по низкой скорости выносится только когда весь интервал агентского замера ниже порога: агент отдаёт целые Mbps, и на границе округления результат считается невоспроизведённым, чтобы не поднимать ложную тревогу. В алерте называется тот агент, который подписал observation. Per-node и project maintenance, retired/offline-ноды и выключенные фоновые speed reports пропускаются. В `Settings → Agents` виден read-only runtime snapshot автоматики.

На controller-е включите подсистему и задайте отдельный длинный secret между Caddy и Xray Checker:

```dotenv
REMOTE_DIAGNOSTICS_ENABLED=true
REMOTE_DIAGNOSTICS_AUTOMATION_ENABLED=true
PROBE_AUTOMATION_COOLDOWN_MINUTES=30
PROBE_AUTOMATION_ALERT_WAIT_SECONDS=90
PROBE_AUTOMATION_MAX_CONCURRENT=2
PROBE_TRUSTED_PROXY_SECRET=replace-with-output-of-openssl-rand-hex-32
PROBE_AGENT_IMAGE=registry.example.com/xray-checker-probe-agent:1.0.0
```

`PROBE_AUTOMATION_ALERT_WAIT_SECONDS=0` отключает ожидание: алерт уйдёт сразу с состоянием `проверка запущена`. Максимум — 300 секунд. Эти параметры задаются через env/CLI и применяются после restart; админка намеренно не персистит их.

Один и тот же `PROBE_TRUSTED_PROXY_SECRET` передаётся `xray-checker` и `caddy`; актуальный [`Caddyfile.example`](Caddyfile.example) публикует только четыре `POST` endpoint: `/enroll`, `/heartbeat`, `/jobs/next` и `/observations`, добавляет proxy secret и передаёт фактически увиденный `{remote_host}`. Controller игнорирует произвольные forwarded IP headers без правильного proxy secret. При прямом подключении без reverse proxy оставьте secret пустым: тогда проверяется socket peer из `RemoteAddr`.

`PROBE_CONTROLLER_URL` и `PROBE_CONTROLLER_IP` описывают сам controller и одинаковы для всех агентов, поэтому задаются один раз в env и подставляются в форму создания агента. Поля остаются редактируемыми: второй адрес controller-а или миграция должны оставаться выразимыми. Задавать нужно оба или ни одного — половина умолчания всё равно заставляет вводить второе поле каждый раз. Ошибка в них останавливает старт, а не всплывает при добавлении следующей пробы.

После restart откройте `Settings → Agents` и укажите:

- `Expected source IP` — точный публичный IPv4/IPv6, с которого controller увидит исходящее соединение агента после NAT. Единственное поле, которое действительно уникально для агента;
- `Controller URL` — HTTPS URL с корректным TLS-сертификатом. Подставляется из `PROBE_CONTROLLER_URL`;
- `Controller IP` — точный IP, к которому агенту разрешено открывать control connection. Подставляется из `PROBE_CONTROLLER_IP`.

Кнопка `Create agent` возвращает персональный Compose с одноразовым enrollment token. Controller хранит только SHA-256 токена и принимает enrollment один раз, до истечения TTL, только с ожидаемого source IP. Агент не использует DNS для control connection: TCP всегда открывается на `Controller IP`, а TLS проверяет hostname из `Controller URL`; redirects запрещены. После появления capability `diagnostic-v1` подключённый agent доступен в selector-е `Remote Diagnostics` раскрытой карточки ноды.

Развёртывание пробы не требует checkout-а: `Create agent` возвращает Compose, который ссылается на опубликованный образ. На Linux-host достаточно docker и файла из админки:

```bash
mkdir -p /opt/xray-checker-probe && cd /opt/xray-checker-probe
# вставьте Compose из Create agent в docker-compose.yml
docker compose up -d
```

Образ `ghcr.io/invisibleproxy/xray-checker-probe-agent` собирается workflow [`probe-agent-image.yml`](.github/workflows/probe-agent-image.yml) для `linux/amd64` и `linux/arm64`: push в `main` даёт теги `main` и `sha-<short>`, тег `probe-agent-vX.Y.Z` — `X.Y.Z` и `latest`. Имя образа в выдаваемом Compose берётся из `PROBE_AGENT_IMAGE` на controller-е.

В production подставляйте digest, а не плавающий тег: `ProtocolVersion` сверяется строго, поэтому агент новее controller-а не пройдёт enrollment. Digest печатается в summary workflow-а или запрашивается так:

```bash
docker buildx imagetools inspect ghcr.io/invisibleproxy/xray-checker-probe-agent:main --format '{{println .Manifest.Digest}}'
```

```dotenv
PROBE_AGENT_IMAGE=ghcr.io/invisibleproxy/xray-checker-probe-agent@sha256:<digest>
```

Порядок обновления — сначала controller, затем `docker compose pull && docker compose up -d` на каждой пробе с новым digest. Сам по себе `restart: unless-stopped` образ не перетягивает, поэтому работающие агенты не обновятся неожиданно.

Альтернатива для host-а без доступа к GHCR — сборка образа из checkout-а:

```bash
cp probe-agent.env.example probe-agent.env
# перенесите PROBE_AGENT_ID, PROBE_ENROLLMENT_TOKEN, URL и IP из результата Create agent
docker compose --env-file probe-agent.env -f docker-compose.agent.yml build
docker compose --env-file probe-agent.env -f docker-compose.agent.yml up -d
```

`probe-agent.env` одновременно задаёт Compose-параметры локальной сборки/лимитов и передаётся агенту через `env_file`. Локальное имя образа задаётся отдельной переменной `PROBE_AGENT_LOCAL_IMAGE`, чтобы не смешивать её с controller-настройкой `PROBE_AGENT_IMAGE`, используемой при генерации персональных Compose-файлов. Старое имя в локальном env пока поддерживается как fallback. Поэтому runtime-настройки агента также меняются только в env-файле; после изменения пересоздайте сервис через `docker compose --env-file probe-agent.env -f docker-compose.agent.yml up -d --force-recreate`.

Шаблон не открывает inbound-порты, запускает container без root и capabilities, с read-only root filesystem, resource limits и именованным `probe_agent_identity` volume. Приватные identity/observation keys генерируются внутри агента и сохраняются с mode `0600`. Временный Xray config создаётся с mode `0600` внутри tmpfs `/run/xray-checker-agent` и удаляется после задания. Endpoint URLs принадлежат конфигурации агента (`PROBE_IP_CHECK_URL`, `PROBE_STATUS_CHECK_URL`, `PROBE_DOWNLOAD_URL`, `PROBE_DIRECT_CHECK_URL`); controller выдаёт только фиксированный profile ID и не может превратить job в произвольный URL fetch. Проба `download` читает ровно `PROBE_DOWNLOAD_MIN_SIZE` байт и останавливается, поэтому это не нижняя граница, а сам объём передачи: по умолчанию 10 МБ с `100Mb.dat`, чего хватает и для осмысленной оценки скорости, и для того, чтобы оборвавшийся на середине endpoint попал в `download_incomplete`, а не зачёлся успешным. Меняя URL, проверяйте, что файл не меньше этого объёма.

После обычного restart одноразовый token больше не нужен: agent продолжает heartbeat с той же identity и persisted sequence. Когда агент появился как `Connected`, значение `PROBE_ENROLLMENT_TOKEN` можно очистить или удалить из персонального Compose; `docker-compose.agent.yml` допускает пустое значение. Именованный `probe_agent_identity` volume при этом нужно сохранить. Если identity volume удалён, старый token не сработает — в админке нужно выполнить `Re-enroll`, остановить прежний stack и развернуть новый персональный Compose. Re-enroll Compose получает новый project/volume, поэтому не переиспользует отозванные ключи; старый volume можно удалить после успешного подключения. `Revoke` немедленно блокирует старую identity и её observation public key. После отзыва запись можно удалить из controller registry кнопкой `Delete`; удалённый Compose stack и identity volume при этом не удаляются автоматически.

`data/diagnostic_agents.json` привязан к конкретной инсталляции и ожидаемым IP, поэтому намеренно не входит в backup/restore. Не публикуйте персональный Compose: до первого enrollment он содержит действующий одноразовый token.

### Логи агента

`docker logs` контейнера пробы показывает, что агент делает:

- при старте — версия, agent ID, controller URL/IP, capabilities, все четыре probe endpoint и лимиты проб. Контейнер, который так и не подключился, всё равно сообщает, с какой конфигурацией он пытался;
- enrollment и первый успешный heartbeat — момент, с которого controller показывает агента как `Connected`;
- по строке на задание: нода, профиль, `host:port` цели и срок; затем результат — статус, latency, Mbps, `failureCode`/`stage`, TCP, ping и отдельно direct-connectivity контроль. Провал этого контроля выделен, потому что controller отказывается делать из такого observation вывод;
- отказ observation и падение heartbeat.

Отдельно закрыт слепой пятно конфигурационных ошибок: в подписанном observation все они схлопываются в единый bounded-код `configuration`, из которого нельзя понять, что именно не сработало. В лог пишется конкретный шаг — не совпал fingerprint (обычная гонка с обновлением подписки), не пишется runtime-каталог, неизвестный профиль, не стартовал Xray.

Токен, приватные ключи и сам Xray-конфиг в лог не попадают никогда. Исходная ошибка Xray или транспорта может процитировать фрагмент конфига ноды, поэтому она пишется только на `PROBE_LOG_LEVEL=debug`; на `info` остаётся объяснение шага.

```dotenv
PROBE_LOG_LEVEL=info
```

### Матрица достижимости

Обычный цикл availability смотрит на ноду из одной точки — с самого controller-а. Этого достаточно, чтобы отличить мёртвую ноду от живой, но недостаточно, чтобы отличить мёртвую от недостижимой по одному конкретному пути. Локально они выглядят одинаково, а чинить их надо по-разному: в первом случае проблема на ноде, во втором — в сети между пользователем и совершенно здоровой нодой.

Sweep периодически задаёт один и тот же вопрос про каждую ноду каждому подключённому агенту и хранит последний ответ на пару «нода × агент».

Находка формулируется на уровне строки, а не пары. Если хотя бы одна точка наблюдения достучалась до ноды — нода жива, и тогда **каждая** точка, которая до неё не достучалась, сообщает о проблеме достижимости. Сам checker здесь такая же точка наблюдения, как агент, и точно так же может оказаться единственным, кто ноду не видит.

Это не мелочь формулировки. Сравнение «каждый агент против checker-а» ломается ровно тогда, когда отрезан сам checker: агент, который тоже не видит ноду, формально «согласен» с checker-ом — и его сообщение теряется, хотя третий агент только что доказал, что нода жива. Поэтому в API есть два поля: `verdict` хранит попарное сравнение как исторический факт, а решение принимается по `unreachable`, который выводится из всей строки при чтении.

Вердикт называет наблюдение, а не причину. Отличить блокировку от null-route или от ноды, которая не принимает одну сеть, по одной ячейке нельзя, поэтому вместо догадки в ячейке лежат `failureCode`, `failureStage` и признак `tcpReached`: TCP не дошёл — дело в сети перед нодой, TCP дошёл и упало внутри туннеля — дело в ноде.

Одно наблюдение находкой не считается. Локальная половина сравнения — это последний результат обычного обхода, то есть она может отставать на один `PROXY_CHECK_INTERVAL`; нода, умершая за минуту до sweep-а, на один проход выглядит как расхождение. Поэтому у каждой ячейки есть `streak`, и находкой она становится только со второго совпадающего прохода подряд. Артефакт устаревания до этого не доживает.

Нода, недоступная вообще отовсюду, находкой не является: это обычный отказ, он уже виден в списке нод и в инцидентах.

Агент, потерявший собственный uplink, сообщил бы о недоступности всех нод сразу. Этого не происходит: controller помечает observation как `Reliable` только при успешном direct-connectivity контроле, и вердикт из ненадёжного наблюдения не выводится вообще.

```dotenv
REACHABILITY_SWEEP_ENABLED=true
REACHABILITY_SWEEP_INTERVAL_MINUTES=60
REACHABILITY_SWEEP_TIMEOUT_SECONDS=120
REACHABILITY_SWEEP_PROFILE=
```

Интервал — это пауза между концом одного прохода и началом следующего, а не фиксированный период: проход, который идёт дольше интервала, замедляется, но не накладывается сам на себя. Минимум — 5 минут и 15 секунд соответственно. Пустой профиль выбирает `default-status` — самую дешёвую пробу, которую поддерживает любой агент; TCP, ping и direct-connectivity приходят с любым observation независимо от профиля, поэтому для sweep-а этого достаточно.

Каждый проход идёт по одной goroutine на агента, ноды внутри — последовательно. Это не про пропускную способность: controller отказывает во второй сессии для той же пары «нода × агент», а агент всё равно выполняет одну пробу за раз. Сессия удаляется сразу после того, как из неё извлечена ячейка, поэтому sweep не засыпает список диагностик и не растит память controller-а.

Вкладка `Reachability` показывает находки отдельной таблицей (подтверждённые сверху) и полную матрицу с легендой; у самого checker-а в матрице своя колонка, рядом с агентами. `Sweep now` запускает внеочередной проход, повторный запрос во время идущего прохода игнорируется. Ноды в maintenance пропускаются: их локальный статус никто не обновляет, сравнивать не с чем. Проект в maintenance останавливает sweep целиком. Матрица лежит в `data/reachability.json` и переживает restart; это кэш наблюдений, поэтому его потеря стоит одного интервала, а не корректности.

Через API — `GET /api/v1/admin/reachability` и `POST` там же для внеочередного прохода.

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
| `SUBSCRIPTION_URL` | обязательно | один или несколько URL подписок; в env разделяются запятыми, в CLI флаг `--subscription-url` повторяется |
| `SUBSCRIPTION_UPDATE` | `true` | автоматическое обновление подписки |
| `SUBSCRIPTION_UPDATE_INTERVAL` | `300` | период обновления подписки, секунды |
| `PROXY_CHECK_INTERVAL` | `300` | период полного обхода всех нод, секунды |
| `PROXY_RECOVERY_INTERVAL` | `15` | период каскадной проверки уже недоступных нод, секунды; `0` отключает быстрый recovery-loop |
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
| `REMNAWAVE_ANNOUNCE_ENABLED` | `false` | master-switch интеграции управляемого subscription `announce` |
| `REMNAWAVE_API_URL` | пусто | базовый URL панели Remnawave, с `/api` или без него |
| `REMNAWAVE_API_TOKEN` | пусто | API token Remnawave; читается только из env и не сохраняется |
| `REMNAWAVE_API_TIMEOUT_SECONDS` | `10` | timeout одного обращения к API Remnawave |
| `REMNAWAVE_RECONCILE_INTERVAL_SECONDS` | `60` | период сверки управляемых `announce`, минимум 10 секунд |
| `REMNAWAVE_TOPOLOGY_INTERVAL_SECONDS` | `300` | период обновления Hosts/Squads, минимум 30 секунд |

В `/admin` блок `Controls` содержит только операции и параметры, применяемые к выбранной ноде или набору нод: ручной speedtest, персональный Test URL, Maintenance/Resume, Telegram mute, фильтры выбора и расписание с зафиксированными `StableID`. Общие операции и настройки вынесены в отдельную верхнюю вкладку `Settings`: обновление подписки, единый срок хранения Speedtest/Availability history, Telegram-конфигурация и backup/restore. Сохраняемое состояние остаётся в volume `data`.

Dashboard админки использует компактные раскрываемые строки нод. Карточка плавно раскрывается по клику в любом неинтерактивном месте шапки или по стрелке; чекбокс, IP/Server и отдельные действия `Check`/`Run` при этом не переключают карточку. Клик по показанному IP/Server копирует только адрес без порта; это же поведение действует на основном dashboard, в Nodes Overview, карточках Remnawave locations и merge-диалоге. Master-checkbox рядом с заголовком `Nodes` выбирает или снимает выбор со всех видимых после фильтрации нод и показывает промежуточное состояние, если выбрана только часть списка. Список сортируется по исходному порядку, имени, status, claimed location, latency, latest speed или subscription. Режим `Manual order` позволяет перетаскивать карточки за handle либо перемещать их стрелками клавиатуры; выбранный режим, направление и порядок по `StableID` хранятся только в `localStorage` текущего браузера. При активных фильтрах переставляются только видимые карточки, а скрытые сохраняют свои позиции. Несколько карточек можно держать открытыми одновременно: период, загрузка истории и состояние каждой панели независимы. Основной dashboard автоматически обновляет статусы и latency с периодом availability-check; автообновление включено по умолчанию и может быть отключено кнопкой `Auto`. Админка каждые 30 секунд заново запрашивает proxies, speedtest snapshot, Telegram config и Nodes Overview, а после возврата на скрытую вкладку обновляется сразу. Фоновое обновление сохраняет существующие DOM-элементы, раскрытие и позицию горизонтальной прокрутки графика, пока набор видимых `StableID` не изменился; сортировка и ручной порядок переставляют уже существующие карточки. Внутри показываются сетевая диагностика и area-график фактически измеренной скорости с отметкой последнего замера; в offline-карточке успешные `TCP OK` и `Ping OK` выделяются зелёным независимо от красного proxy-статуса, а неуспешная диагностика остаётся красной. Сводка выбранного периода показывает процент успешных измерений: успешным считается завершённый замер со скоростью не ниже настроенного low-speed threshold; low-speed, offline и error входят в знаменатель как неуспешные. При движении курсора график выбирает ближайший реальный замер, показывает вертикальный crosshair и tooltip со статусом, Mbps, TTFB и скачанным объёмом. Для обоих режимов графика доступны общие периоды 1 и 3 часа, 24 часа, 3, 7, 14 и 30 дней, а также произвольные даты. Пропуски не интерполируются: фактическая линия прерывается, её участки мягко затухают без вертикальной границы заливки, а тонкий пунктир и полупрозрачная error-зона показывают отсутствие замера без ложного падения скорости до нуля. При системной настройке reduced motion декоративные анимации отключаются.

В правом нижнем углу раскрытой карточки переключатель `Speedtest`/`Availability` меняет dataset этого же графика. Блок статистики резервирует одинаковую высоту в обоих режимах, поэтому перенос длинной строки `Checks` не сдвигает область графика при сравнении. Availability-режим показывает latency реальных proxy-checks в ms, статистику `online`/`proxy_failure`/`offline`, crosshair и tooltip на тех же периодах; история хранится по `StableID` за тот же срок, что и speedtest. Если online-замеров не меньше 20, верхняя граница графика адаптируется по P90, чтобы редкие TTFB-пики не сплющивали основной диапазон. Пики выше шкалы отмечаются жёлтым маркером у верхней границы; реальное значение остаётся в tooltip и `Maximum`. Для короткой истории показывается полный диапазон. Maintenance и probe-only проверки в неё не записываются, а при node merge retained history переносится в active target.

Текущая таблица `Results` и dashboard KPI используют только active `StableID`. Результаты retired-нод не удаляются: их сохранённая история остаётся доступна через `Speed History` и `Nodes Overview`.

Для плановых работ выберите active-ноду и используйте контекстную кнопку `Maintenance`/`Resume` в `Controls → Actions`; тот же переключатель доступен в `Nodes Overview`. Режим сохраняется по `StableID` в `data/node_registry.json` и переживает restart. Maintenance-нода исключается из availability/downtime-статистики, быстрого recovery-loop, планового speedtest и Telegram alerts/retries, но медленный полный availability-обход продолжает выполнять через неё технический proxy-probe для Remnawave announce. Результат такого probe не попадает в Prometheus, node incidents, downtime и Telegram. Кнопки `Check` и `Run` в админке остаются доступны и не снимают maintenance; результат ручного speed-probe виден в текущем admin snapshot, но не записывается в speedtest history и не участвует в агрегатах. Telegram-команды paused-ноду пропускают. При включении закрываются текущий downtime и активный node incident; cumulative downtime, incident journal, прежняя speedtest history, mute и персональный Test URL сохраняются. `/config/<StableID>` во время обслуживания отвечает `200 Maintenance` с `X-Xray-Checker-Status: maintenance`, чтобы внешний uptime-check не записывал плановую остановку как outage. После `Resume` нода снова участвует в мониторинге, но не считается online до следующей настоящей проверки.

В Remnawave maintenance-нода остаётся member своей location и оценивается по фактическому полному proxy-probe. Пока probe успешен, announce не меняется. Подтверждённый offline участвует в обычных `down`/`partial` сценариях; только если все members конкретной локации одновременно offline и каждый из них находится в maintenance, для этой location используется отдельный настраиваемый текст: по умолчанию серверы локации находятся на обслуживании и возможны проблемы в работе. Для смешанного outage нескольких локаций обычный и maintenance-тексты объединяются, а oversized результат использует отдельный fallback. Применяются те же `OutageMinutes`, `MinimumFailures`, recovery hysteresis и exact ownership rules; неизвестный либо изменённый вне checker-а `announce` остаётся нетронутым. После `Resume` status не считается healthy до реального full check.

`Refresh Geo` в Nodes Overview проверяет только active-ноды из текущего фильтра. Retired-записи сохраняют уже накопленные GeoIP-данные, но новые внешние запросы для них не выполняются.

Админка и `/api/v1/admin/*` всегда требуют Basic Auth. Поэтому обычный запуск без явных `METRICS_USERNAME` и `METRICS_PASSWORD` завершается ошибкой; исключение — `RUN_ONCE=true`, когда HTTP-сервер не поднимается. Встроенных логина и пароля больше нет. При `METRICS_PROTECTED=true` та же защита распространяется на dashboard, приватный API и `/metrics`.

## Основные сценарии

### Обновление подписки

Подписки обновляются автоматически либо кнопкой в админке. Обновление идёт одним длинным запросом, поэтому его фаза опрашивается отдельно через `GET /api/v1/admin/subscription/refresh/progress`: в админке видно, что именно выполняется сейчас — загрузка подписок, резолвинг адресов, сравнение с текущим набором, применение с перезапуском Xray или завершение — и сколько это уже длится. После завершения в той же строке остаётся разбивка по времени фаз длиннее 200 мс, чтобы медленное обновление можно было отнести к конкретному этапу, а не гадать. Чтение прогресса никогда не вмешивается в само обновление. Все настроенные источники загружаются параллельно, а их ноды объединяются в одну конфигурацию. Checker не изменяет полученные имена нод: например, суффикс `^~2~^`, добавленный Remnawave, будет показан как часть имени. Одинаковый `StableID` из двух источников считается конфликтом и отклоняет startup/refresh.

Для VLESS/VMess xHTTP share-links checker сохраняет полный объект `xhttpSettings`, включая валидный URL-encoded JSON из параметра `extra`, и переносит его в сгенерированный Xray config без сведения к одним только `path`, `host` и `mode`. Невалидный JSON в `extra` не считается поддерживаемой конфигурацией.

Нода идентифицируется по `StableID`, а не по имени. Одинаковые имена допустимы, если параметры подключения дают разные `StableID`. При обновлении приложение пытается сохранить прежние ID, статистику и настройки нод; неоднозначные совпадения по одинаковым именам намеренно не склеиваются. Если две конфигурации всё же дают одинаковый `StableID` без учёта регистра, startup или refresh отклоняется до перезапуска Xray — молчаливого смешивания статистики больше нет.

Перед применением строится diff по `StableID`. Пустой результат или удаление минимум трёх и одновременно не менее половины прежних нод считается подозрительным: плановый refresh отклоняется, а ручной требует отдельного подтверждения со списком удаляемых нод. Подтверждение привязано opaque fingerprint к конкретному previewed candidate, поэтому повторно изменившийся ответ подписки не применится по старому согласию. Новый Xray config сначала генерируется как candidate. Если Xray не запускается, восстанавливается предыдущий файл и повторно стартует last-known-good конфигурация; checker и web endpoints переключаются только после успешного restart.

### Перенос истории после смены ключа ноды

Если UUID/пароль/ключ, имя или порт ноды изменились, generated `StableID` может измениться: старая запись становится retired, а новая появляется как active. Во вкладке `Nodes Overview` у retired-записи нажмите `Merge` и явно выберите active target. Список содержит все совместимые active-ноды с теми же нормализованными `SubName`, protocol и server; имя и порт могут отличаться и показываются в preview отдельными предупреждениями. Конкретный target всегда выбирает администратор, даже если кандидат один.

Preview показывает обе пары StableID, изменившиеся имя/порт и итоговые количества. После подтверждения merge только ставится в staging, а UI показывает состояние `restart required`. Перезапустите сервис:

```powershell
docker compose restart xray-checker
```

На startup в active StableID переносятся сохранённая speedtest history/latest result, накопленный downtime, incident count и ссылки incident journal, наиболее ранний `FirstSeenAt`, максимальный downtime, актуальнейшие GeoIP-данные и lineage прежних StableID. Текущая active-конфигурация и её live-состояние остаются целевыми. Retired-запись удаляется лишь после того, как speedtest, node archive и Telegram успешно загрузили новое состояние. До этого исходные `node_registry.json` и `speedtest_results.json` лежат в rollback-копии; ошибка загрузки возвращает их и останавливает startup с требованием повторного запуска. После подтверждённого startup `Nodes Overview` сверяет исчезновение source с `MergedFromStableIDs` target и показывает `Node merge completed successfully`; у target также остаётся постоянный `Merge applied` marker. Backup restore и node merge нельзя ставить в staging одновременно.

Availability history хранится внутри `node_registry.json`, поэтому та же merge-транзакция переносит её вместе с downtime и incident state без третьего runtime-файла.

Remnawave location membership в node merge намеренно не переносится: транзакция merge по-прежнему меняет только node archive и speedtest history. Если source StableID был member location во вкладке `Remnawave`, после успешного merge вручную замените его на новый active StableID в той же location и сохраните настройки.

### Проверка доступности и recovery

Полный обход всех отслеживаемых нод выполняется с периодом `PROXY_CHECK_INTERVAL` и всегда запускает настроенный proxy-check. Для maintenance-нод этот обход работает как probe-only для Remnawave и не обновляет monitoring-статистику; быстрый recovery-loop их пропускает. Для уже недоступных monitored-нод дополнительно работает цикл `PROXY_RECOVERY_INTERVAL`: TCP и ping проверяются параллельно, а proxy-check запускается в том же цикле только при успешном TCP-соединении. Ping остаётся диагностикой и никогда не блокирует proxy-check, потому что ICMP может быть запрещён у рабочей ноды. Состояние `TCP No` вместе с `Ping OK` означает, что хост отвечает, но порт ноды не принимает TCP-соединение. Обычный полный обход не использует TCP-гейт и остаётся контрольной проверкой на случай расхождения диагностики.

При провале proxy-check нода сначала получает `proxy_failure`. Она не становится `offline`, если TCP или ping прошли; только провал всех трёх проверок открывает `Down Since`. Длительность `proxy_failure` ведётся отдельно и не увеличивает downtime. Для внешнего `/config/<StableID>` такое состояние отвечает `200 Proxy failure` с заголовком `X-Xray-Checker-Status: proxy_failure`; `503` остаётся только для hard `offline`. Непривилегированный ping в Linux сопоставляет ответ по типу, sequence и уникальному payload: ICMP ID намеренно не используется, поскольку ядро может переписать его для datagram-сокета.

Ошибка proxy-check классифицируется стабильным кодом: `dns`, `tcp_refused`, `tcp_timeout`, `host_unreachable`, `proxy_handshake`, `proxy_timeout`, `tls`, `http_status`, `source_ip_unchanged`, `download_incomplete`, `check_endpoint` или `unknown`. Прямые TCP/ping-результаты уточняют причину, но отсутствие ICMP-ответа само по себе не объявляет хост мёртвым. Вкладка `Incidents` хранит начало, восстановление, длительность, причину и затронутые `StableID`.

Быстрые попытки не увеличивают Telegram `Failed checks` и не сдвигают расписание напоминаний. Когда настоящий proxy-check подтверждает восстановление, downtime закрывается сразу, а pending recovery передаётся Telegram без ожидания следующего alert-цикла. В админке `Check` доступен и в строке конкретной ноды, и как групповое действие для выбранных нод; в карточке ноды Telegram для администратора доступна кнопка «Проверить доступность». Все ручные точки используют тот же каскадный workflow и обновляют TCP/ping.

### Обслуживание проекта

Когда работы затрагивают весь стенд, а не отдельную ноду, включите режим в `Settings → Project` кнопкой `Enable Project Maintenance`. Включение запрашивает подтверждение, выход выполняется кнопкой `Resume Project` там же или прямо в баннере. То же доступно через `GET`/`PUT /api/v1/admin/project-maintenance` с телом `{"enabled": true}`.

Пока режим включён, приостановлены плановый обход доступности и быстрый recovery-loop, плановый speedtest и 30-минутные confirmation retry, Telegram-алерты, напоминания и отчёты, автоматическое обновление подписки и operational-запись в Remnawave. Продолжают работать Xray и пользовательский трафик, HTTP-сервер, `/health`, админка и API, автоматические бэкапы, heartbeat диагностических агентов, а также ручные `Check`, `Run`, backup и удалённая диагностика. Ручной speedtest в этом режиме считается разовой пробой: результат виден в текущем snapshot, но не попадает в историю, KPI и Telegram.

Включение закрывает текущие downtime, интервалы `proxy_failure` и активные инциденты, а также очищает счётчики Telegram-алертов и отложенные confirmation retry. История, накопленный downtime, журнал инцидентов, настройки, mute и персональные Test URL сохраняются. Отдельный per-node `Maintenance` живёт независимо: нода, поставленная на паузу до включения глобального режима, останется на паузе после выхода.

Состояние видно снаружи явно, без подмены статусов нод: `/config/<StableID>` отвечает `200 Project Maintenance`, `/api/v1/status` возвращает `status: maintenance`, dashboard и публичная status page показывают баннер, а Prometheus — метрику `xray_checker_project_maintenance`.

После `Resume` ноды не становятся `online` автоматически: сначала выполняется реальная проверка. Пропущенные за время работ расписания не выстраиваются в очередь догоняющих запусков — следующий плановый speedtest отсчитывается от момента выхода. Состояние хранится в `data/project_state.json`, переживает рестарт и входит в backup, поэтому восстановление архива, снятого во время работ, вернёт и включённый режим.

### Remnawave subscription announce

Интеграция меняет только header `announce` у выбранных External Squads. Имена Hosts, конфигурации нод, пользователи и состав сквадов не редактируются. Для отдельного API token в Remnawave нужны минимальные scopes:

- `hosts:list`;
- `internal-squads:list`;
- `external-squads:list`;
- `external-squads:update`.

Последний scope разрешает PATCH всего External Squad endpoint, а не только одного header, поэтому токен всё равно считается привилегированным. Храните его только в `REMNAWAVE_API_TOKEN`, ограничьте сетевой доступ checker → panel и не используйте административный JWT.

После добавления `REMNAWAVE_ANNOUNCE_ENABLED=true`, `REMNAWAVE_API_URL` и token перезапустите checker, откройте вкладку `Remnawave` и нажмите `Sync topology & reconcile`. Затем:

1. Добавьте пары `Internal Squad → External Squad`. Для схемы `1 in содержит только 1 ext`, `2 in → 2 ext`, `3 in → 3 ext` это три отдельные рабочие строки.
2. Добавьте сервисную пару checker и отметьте её `Monitoring-only`. Её External Squad никогда не изменяется.
3. Создайте location-карточки. Быстрый путь — кнопка `Suggest from host tags`: checker сопоставляет ноды с Hosts по `address:port` и показывает группировки, которые уже описаны тегами хостов в панели. Какие теги являются локациями, решает оператор: `BALANCER_NL` обычно да, `EU` обычно нет. Ноды, не совпавшие ни с одним тегированным Host, перечисляются отдельно и в подсказку не попадают.
4. Либо задайте карточку вручную: стабильный ключ, при необходимости `Public label`, и members. У member обязателен только checker-нода; поле Host можно оставить в значении `Auto · match by address:port`, тогда пара выводится на каждом sync и переживает смену `StableID`. Явно выбранный Host остаётся решением оператора и не перезаписывается; если на один `address:port` приходится несколько Hosts, пара не выводится и её нужно указать руками.
5. Если несколько Hosts являются резервами одной пользовательской локации, добавьте их members в одну location-карточку. Members сначала сворачиваются по адресу ноды: один сервер, отданный несколькими транспортами, считается одним сервером и остаётся доступным, пока жив хотя бы один его транспорт. Partial-сценарий включается только когда потерян сервер целиком, полный outage — когда потеряны все серверы локации.
6. В `Message constructor` включите нужные сценарии, отредактируйте шаблоны и проверьте preview.
7. Сохраните настройки и только после проверки topology включите `Automatic announce`.

Для Remnawave `proxy_failure` и подтверждённый `offline` означают недоступность сервиса и участвуют в тех же `down`/`partial` announce-сценариях; при этом таймер берёт отдельный `ProxyFailureSince` либо hard-offline `DownSince`, поэтому monitoring downtime не смешивается с service-failure временем.

Host сам по себе не принадлежит External Squad. Аудитория вычисляется через фактическую модель Remnawave:

```text
location → StableID → Host UUID → inbound Host → Internal Squad → явно выбранный External Squad
```

При этом учитывается `internalSquads` хоста: список читается как deny-list в режиме `EXCLUDE` и как allow-list в режиме `ALLOW_ONLY` — это правило панели начиная с 3.4. Панели старее шлют только `excludedInternalSquads`, что эквивалентно `EXCLUDE`, и обрабатываются как раньше. Disabled/hidden Hosts не считаются доступными пользователю. Если хотя бы один member location доступен, локация не считается полностью недоступной. Однако подтверждённый отказ другого member переводит её в состояние частичной доступности и может показать отдельное сообщение. Любой новый статус появляется только после непрерывного downtime не короче 15 минут и минимум трёх полных обходов. Ручной единичный `Check` и быстрый recovery-loop этот счётчик не увеличивают. Активный массовый incident с вероятностной причиной `check_endpoint` подавляет новое пользовательское сообщение: такая корреляция может означать отказ самого проверочного endpoint, а не всех нод.

Тексты не зашиты в outage workflow. Versioned config v5 хранит canonical `locations` и отдельные включаемые сценарии полной недоступности одной/нескольких/всех локаций, частичной недоступности одной/нескольких локаций и `everything stable`, а также обязательные fallback для слишком длинных списков. Поддерживаемые плейсхолдеры:

- одна локация: `{location}`, `{unavailable}`, `{total}`;
- несколько локаций: `{locations}`, `{unavailable}`, `{total}`;
- частичная недоступность одной/нескольких локаций: соответственно `{location}` или `{locations}`, а также `{affected}`, `{total}`;
- полный outage, stable-state и fallback полной недоступности: `{unavailable}`, `{total}`;
- fallback частичной недоступности: `{affected}`, `{total}`.

Кнопки плейсхолдеров вставляют токены в позицию курсора, а preview показывает результат на примере. Неизвестные плейсхолдеры, переносы строк, URL, Remnawave delimiters `{{...}}` и итоговый текст длиннее 240 символов отклоняются backend-валидацией. При более чем трёх недоступных локациях или слишком длинном rendered list используется настраиваемый fallback. Отключённый сценарий не держит status line для соответствующего состояния: reconcile удалит только точно принадлежащий checker-у suffix и сохранит операторскую первую строку.

Reconcile обновляет `announce`, когда подтверждённый outage начался, изменился набор полностью или частично проблемных локаций, состояние location перешло `healthy ↔ partial ↔ down`, завершился recovery-hysteresis либо были сохранены новые policy/location-members/templates. Полная недоступность имеет приоритет над partial-сообщением. Manual check и быстрый recovery-loop сами по себе сообщение не подтверждают. Unknown/ambiguous state не создаёт новый outage, а активный вероятностный `check_endpoint` incident подавляет его.

После подтверждённого улучшения `down → partial` и восстановления `partial/down → healthy` действует пятиминутный hysteresis. Если сценарий `Everything stable` выключен, checker удаляет только свою строку статуса: существовавший до интеграции `announce` восстанавливается байт-в-байт, а созданный полностью checker-ом header удаляется. Если сценарий включён, его настраиваемый текст заменяет аварийное сообщение; при наличии исходного base status остаётся последней строкой.

Если у External Squad уже есть однострочное значение с префиксом `rwEncodeBase64:`, checker сохраняет его как operator base и дописывает статус через `\n`. Он также может безопасно восстановить многострочный base, когда текущее значение заканчивается точным текстом настроенного healthy-сценария: совпавший последний suffix принимается под управление, а всё перед ним сохраняется байт-в-байт. Например, текущее значение

```text
rwEncodeBase64:{{USERNAME}} | Нажми, чтобы перейти в кабинет →

Нажми 🔄️, если сервер недоступен

Все сервера работают стабильно ✅
```

при полной недоступности Германии станет:

```text
rwEncodeBase64:{{USERNAME}} | Нажми, чтобы перейти в кабинет →

Нажми 🔄️, если сервер недоступен

⚠️ Временно недоступна локация «Германия». Остальные доступные вам локации работают.
```

При частичном отказе той же location последняя строка вместо этого будет начинаться с `⚠️ Часть серверов локации «Германия»`. Remnawave обрабатывает весь текст после единственного префикса, подставляет template variables и кодирует многострочное значение в `base64:...`, поэтому клиент показывает статус с новой строки. Checker сначала читает полный `responseHeadersAdd`, меняет только регистронезависимый ключ `announce` и PATCH-ит объединённую карту. Многострочный операторский `announce` можно передать под управление явным действием `Adopt current announce as base` в карточке сквада: checker запоминает точный текст, дописывает статус после него и при следующих проходах заменяет всё, что стоит за базой. Захват сделан отдельной кнопкой намеренно — базу, вычисленную на лету, пришлось бы угадывать отбрасыванием знакомой строки статуса, и после потери ownership-файла старый статус стал бы частью базы, а сообщение росло бы с каждым проходом. Редактирование текста в панели перестаёт совпадать с принятой базой и снова даёт конфликт, а не тихий дрейф. Произвольный многострочный чужой `announce`, suffix которого не совпадает с известным checker-сценарием, не принимается под управление и остаётся нетронутым. Если оператор изменил либо удалил составное управляемое значение вручную, checker отказывается от ownership и показывает конфликт в админке. Если отдельное Response Rule пользователя также переопределяет `announce`, оно имеет более высокий приоритет, чем External Squad, и сообщение этой интеграции пользователь не увидит.

### Speedtest и история

В админке `Run` доступен в каждой строке ноды и как групповое действие для выбранных нод; тест также можно запустить из Telegram либо по расписанию. Сразу после клика запрошенные кнопки показывают `Starting…`, общий status chip отражает запуск, а после принятия запроса UI переходит к счётчику `Running completed/selected`; повторный запуск на это время блокируется. Все точки запуска используют сохранённый `ScheduleConfig`, включая URL, лимит, timeout и concurrency. Для выбранной ноды можно сохранить отдельный Test URL; он имеет приоритет над глобальным URL. Перед любым ручным speedtest выполняется availability-check; в замер попадают только ноды с успешным proxy-check. Плановые тесты и confirmation retry пропускают `proxy_failure` и `offline`; в историю они не пишутся. После availability gate запуск из админки, из Telegram и confirmation retry используют двухфазный primary/country fallback workflow: все primary-замеры завершаются до первого fallback, поэтому резерв одной ноды не делит полосу с primary-замером другой и не искажает Mbps. Плановый обход остаётся параллельным, разменивая точность на пропускную способность. В историю и отчёт попадает только финальный результат каждой ноды.

Время следующего планового запуска сохраняется как абсолютный deadline в `data/speedtest_schedule.json`. Пересоздание контейнера при подключённом `/app/data` и сохранение фильтров, TestConfig или retention не начинают отсчёт заново. При смене интервала сохраняется прежний временной якорь; если запуск был пропущен во время остановки контейнера, после startup выполняется один немедленный тест, а не серия догоняющих запусков.

Если основной Test URL завершается технической ошибкой либо показывает скорость ниже настроенного Telegram-порога `Low Mbps`, checker повторяет замер через резервные адреса страны, заявленной в имени ноды или подписки. При нулевом пороге fallback по скорости отключён. GeoIP для выбора не используется. Скопируйте версионируемый пример в локальный каталог:

```powershell
Copy-Item country-test-urls.example.yaml data/country-test-urls.yaml
```

Файл перечитывается перед каждым запуском speedtest. Для страны можно задать несколько HTTP/HTTPS-адресов с уникальными `id` и начальным `priority`. Checker предпочитает последний успешный адрес конкретной ноды, учитывает общую статистику доступности и временно исключает сбойные URL. Перед полным тестом резерв проверяется коротким запросом до 64 КиБ; за один замер выполняется не более двух резервных попыток.

Резервный результат отображается в админке символом `↪` и сохраняется в истории. Если его скорость не ниже порога, он исключается из автоматических Telegram speed-report и confirmation retry. Если резерв тоже медленный, обычный фоновый запуск ставит повтор исходного speedtest через 30 минут; если доступные резервы завершаются ошибкой после медленного основного результата, для того же повтора сохраняется исходный low-speed результат. Неразрешённый `context deadline exceeded` использует тот же 30-минутный повтор, без отдельного короткого таймера, но остаётся технической ошибкой загрузки файла за timeout: значение `Error`/`PrimaryError` не заменяется и не классифицируется как low-speed. Прямой запуск из Telegram по-прежнему возвращает результат в исходный чат и topic.

Результат speedtest, запущенного командой или кнопкой Telegram-бота, всегда возвращается в тот же чат и topic, откуда пришёл запрос. Это прямой ответ пользователю, поэтому он не зависит от режима фоновых отчётов `always`/`issues`/`disabled`.

Speedtest удерживает Xray lifecycle-lock от выбора нод до завершения всех замеров. Refresh подписки дождётся активного теста, а новый тест во время restart начнётся только после запуска обновлённой конфигурации. Это исключает обращение к уже остановленным SOCKS-портам.

Истории speedtest и Availability хранятся по времени с общим сроком из `History retention days`. Значение по умолчанию — 60 дней, диапазон настройки — 1–3650 дней. При уменьшении срока устаревшие записи сразу удаляются из обеих историй.

### Telegram

Бот показывает сначала короткую сводку и проблемы, а технические поля и успешные результаты убирает в раскрываемые детали. Интерактивные экраны «Проблемы» и «Замеры», включая кнопки перехода к истории, используют только active-ноды текущей подписки; сохранённые результаты retired-нод остаются доступны в админской истории и архиве, но в Telegram не выводятся. На Telegram Bot API с поддержкой Rich Messages используются нативные заголовки, таблицы, списки и `Details`. Для старого или локального Bot API автоматически применяется компактный HTML-вариант; при неоднозначной сетевой ошибке бот не отправляет fallback повторно, чтобы не задвоить сообщение.

Режим отчётов `always` отправляет каждый завершённый тест, включая запуск по расписанию. Режим `issues` отправляет только отчёты с недоступными нодами, ошибками или подтверждённой низкой скоростью.

Telegram различает hard `offline` и `proxy_failure`: для первого используется красный down-alert и downtime, для второго — отдельный жёлтый proxy-failure alert с собственной длительностью. Переход между этими состояниями не считается recovery; recovery отправляется только после реального `online`. Поэтому proxy-only failure не увеличивает monitoring downtime, но его время и количество сохраняются в node archive.

Первый обычный фоновый результат ниже порога (расписание или админ-панель) сначала проверяется через country fallback. Если резервный результат нормальный, событие закрывается без сообщения и отложенного теста. Только если резерв тоже медленный либо доступные резервы завершаются ошибкой, checker ставит отдельный speedtest просевших нод через 30 минут: нормальный повтор снимает событие без сообщения, а повторно низкая скорость, offline или ошибка дают алерт «проблема подтверждена». Неразрешённый `context deadline exceeded` в основной ошибке или `PrimaryError` проблемного резервного результата входит в ту же 30-минутную очередь и до подтверждения не уведомляет; здоровый fallback такой retry не создаёт. Объединена только очередь планирования: deadline остаётся технической ошибкой и учитывается как `failed`, а не `slow`. Если до due time ручной тест из админки получает по ноде финальный результат без ошибки и не ниже порога, checker отменяет общий pending retry. Результат планового теста чужой pending retry не снимает. Pending retry сохраняется в `node_alert_state.json` с нейтральным типом `speed-confirmation`, due time, TestConfig и списком `StableID`, переживает restart и дедуплицируется по `StableID`. После завершения confirmation-run pending снимается со всех запрошенных нод, в том числе если нода успела перейти в `proxy_failure`/`offline` и была пропущена без speed-result. Старые записи без типа, `low-speed` и `deadline` автоматически мигрируют; для прежнего `deadline` срок переносится на 30 минут от исходного события. Для запуска из Telegram эти правила не задерживают запрошенный пользователем результат.

Если одновременно готовы алерты минимум по трём нодам и они составляют не менее половины активных нод области с одинаковым кодом причины, Telegram формирует один массовый инцидент. Сначала проверяется глобальная корреляция, затем область отдельной подписки. Доступные TCP-порты разных нод при одинаковой DNS/HTTP/TLS/timeout-ошибке отмечаются как вероятный общий сбой проверочного endpoint; это вывод по корреляции, а не притворная стопроцентная диагностика.

Recovery-уведомление хранится как pending до подтверждённой отправки и повторяется после временной ошибки Telegram. Оно создаётся только если уведомление о проблемном состоянии этой ноды ранее действительно было отправлено. При настройках по умолчанию быстрый цикл начинает очередную каскадную проверку не позднее чем через 15 секунд; после `TCP OK` успешный proxy-check немедленно обновляет статус, закрывает соответствующий интервал и запускает отправку recovery. Длительность самой сетевой проверки и Telegram-доставки добавляется к этому времени.

### Резервные копии

Вкладка `Backup` позволяет скачать ZIP и загрузить его для восстановления. Архив содержит persisted JSON-состояние, manifest и SHA-256 каждого файла. JSON проверяется по типизированной схеме и на дублирующиеся ключи. Из Telegram-конфигурации токен, chat ID, thread ID, список администраторов и неизвестные поля в архив не попадают независимо от регистра ключа. Remnawave policy/pairs/locations входят в backup, но API token и runtime ownership удалённых header-значений исключены.

Автоматический архив создаётся при старте, затем ежедневно около `00:05 UTC` в `/app/data/backups`. Хранятся не более семи архивов и не дольше семи дней.

Загруженный архив сначала проверяется и помещается в staging. На следующем запуске восстановление остаётся транзакцией с rollback-копией, пока speedtest, node archive и Telegram не загрузят состояние. Ошибка загрузчика откатывает прежние файлы; оборванная транзакция безопасно завершается или откатывается при следующем startup.

## Постоянные данные

Не запускайте production-контейнер без volume `/app/data`, иначе после пересоздания контейнера пропадут история и настройки.

| Путь | Содержимое |
| --- | --- |
| `data/node_registry.json` | архив нод, availability history с общим для speedtest сроком хранения, режим обслуживания, downtime, incident journal, GeoIP-состояние и lineage объединённых StableID |
| `data/speedtest_results.json` | результаты и история speedtest |
| `data/speedtest_schedule.json` | расписание, deadline следующего запуска, срок хранения и Test URL нод |
| `data/country-test-urls.yaml` | редактируемый каталог резервных Test URL по ISO-кодам стран |
| `data/speedtest_url_health.json` | доступность, cooldown и последние успешные резервные URL |
| `data/telegram_config.json` | редактируемая Telegram-конфигурация |
| `data/node_alert_state.json` | состояние Telegram-уведомлений, причины и pending 30-минутные speedtest confirmation retries |
| `data/remnawave_announce_config.json` | policy, пары Internal/External Squads, location members (`location key → StableID → Host UUID`) и принятые операторские announce-базы; входит в backup |
| `data/remnawave_announce_state.json` | runtime ownership управляемых `announce`; не входит в backup |
| `data/project_state.json` | глобальный режим обслуживания проекта и момент включения; входит в backup |
| `data/backups/` | автоматические ZIP-архивы |
| `data/.node-merge-*` | временные staging/rollback-каталоги node merge; очищаются после подтверждённого startup |
| `geo/` | загруженные GeoIP/GeoSite-файлы |

`xray_config.json` генерируется заново и не считается пользовательским состоянием.

## Проверка изменений

GitHub Actions и Dependabot в этом форке отключены: автоматические проверки, публикация Docker Hub description и release jobs на GitHub не запускаются. Проверки выполняются локально перед push.

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
