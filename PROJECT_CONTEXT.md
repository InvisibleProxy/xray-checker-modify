# Контекст проекта Xray Checker Modify

Документ фиксирует архитектуру и бизнес-правила текущего форка. Инструкции по установке и эксплуатации находятся в [`README.md`](README.md), правила работы агентов — в [`AGENTS.md`](AGENTS.md).

## Назначение

Приложение получает Xray-конфигурации из одной или нескольких подписок, поднимает локальные SOCKS-inbound для каждой ноды и проверяет доступность трафика через них. Поверх базового мониторинга форк добавляет приватную админку, speedtest, архив нод, Telegram и резервное копирование persisted-состояния.

Основная единица данных — нода с `StableID`. Отображаемое имя приходит из подписки и не является идентификатором.

## Компоненты

| Компонент | Ответственность |
| --- | --- |
| `main.go` | сборка приложения, startup sequence, планировщики, HTTP-маршруты и обновление подписок |
| `config/` | CLI-флаги, переменные окружения и валидация базовой конфигурации |
| `subscription/` | загрузка и парсинг подписок, поддержка нескольких источников, опциональное разрешение доменов |
| `models/` | модель proxy-конфигурации и генерация `StableID` |
| `xray/` | генерация runtime-конфига и встроенный экземпляр Xray Core |
| `checker/` | проверки доступности, latency, host/ping diagnostics, классификация причин и текущее состояние нод |
| `metrics/` | Prometheus-метрики и Pushgateway |
| `speedtest/` | ручные и плановые тесты скорости, Test URL нод и temporal retention |
| `nodearchive/` | долгоживущий реестр активных/выбывших нод, downtime, persisted incident journal, GeoIP активных нод и speedtest summary |
| `nodemerge/` | preview и crash-safe перенос persisted identity/history с retired StableID в active StableID |
| `remnawave/` | безопасный API client, topology model, audience-aware policy, ownership и reconciliation subscription `announce` |
| `telegram/` | компактный HTML и Rich Messages, команды, отчёты, алерты, recovery и настройки mute |
| `backup/` | создание ZIP, автоматическая ротация, типизированная проверка и транзакционный staged restore |
| `web/` | dashboard, admin UI, REST API, OpenAPI и Basic Auth middleware |

Frontend встроен в Go-бинарник через `embed`. `docs/` — отдельный сайт Astro/Starlight, унаследованный от upstream.

## Startup sequence

1. Разбираются CLI-флаги и переменные окружения; обычный server-mode требует явно заданные Basic Auth credentials.
2. Незавершённые node-merge и restore-транзакции предыдущего процесса завершаются или откатываются по durable commit marker.
3. Если существует staged restore, оно применяется к `data/`, но rollback-копия сохраняется; node merge в этот startup не применяется.
4. Инициализируются пользовательские web assets.
5. Проверяется наличие GeoIP/GeoSite-файлов; отсутствующие файлы скачиваются.
6. Загружаются подписки, назначаются индексы и проверяется уникальность `StableID`.
7. Если restore не применялся, staged node merge проверяется против фактически активных StableID и транзакционно заменяет `node_registry.json` и `speedtest_results.json`, сохраняя оригиналы для rollback.
   Availability history находится внутри node registry и re-keyed из retired source в active target в той же транзакции.
8. Запускается встроенный Xray Core с уже сгенерированным `xray_config.json`.
9. Загружаются speedtest, node archive, Remnawave announce config и Telegram state. Ошибка backup-владельца откатывает применённый node merge либо restore и останавливает startup. Отдельный Remnawave ownership runtime не входит в restore; его ошибка безопасно запрещает remote writes.
10. Только после успешной загрузки всех владельцев активная транзакция подтверждается и rollback-копия удаляется.
11. Восстанавливается накопленный downtime и синхронизируется speedtest history.
12. Запускаются автоматические бэкапы, Telegram, Remnawave reconciliation worker, speedtest scheduler, полные proxy-checks и быстрый recovery-loop недоступных нод.
13. Поднимается HTTP-сервер.

## Основные workflow

### Проверка доступности

Для каждой ноды создаётся SOCKS-inbound на `XRAY_START_PORT + Index`. Полный обход с периодом `PROXY_CHECK_INTERVAL` выполняет выбранный метод `ip`, `status` или `download` через каждый inbound без TCP-гейта. Состояние трёхуровневое: `online` — proxy-check успешен; `proxy_failure` — proxy-check не прошёл, но TCP или ping успешны; `offline` — proxy, TCP и ping все не удались. `ProxyFailureSince` накапливается отдельно от downtime; только offline открывает `DownSince` и node incident. Диагностика записывается после proxy-check и может перевести proxy_failure в offline; downtime начинается с момента подтверждения провала всех проверок и не включает предыдущий proxy-failure интервал. Статус, причина и диагностика хранятся по `StableID`; после итерации обновляются downtime, proxy-failure stats, incident journal, Telegram и Pushgateway.

Операторский maintenance mode хранится в `node_registry.json` по `StableID` и восстанавливается до запуска Telegram/фоновых workers. Переключение удерживает Xray lifecycle write-lock, поэтому уже запущенный availability-check или speedtest не может пересечь границу режима. Maintenance-нода исключается из monitoring metrics/status API, downtime и incident accounting, быстрого recovery-loop, планового speedtest и Telegram alert/retry state; она не входит в online/offline totals и массовый incident denominator. Медленный полный availability-обход всё же выполняет для неё probe-only proxy-check: raw результат хранится отдельно от публичного monitoring status и нужен для Remnawave evaluation. Явный admin `Check`/`Run` также разрешён и не снимает maintenance; ручной speed-result доступен в текущем admin snapshot как `maintenanceProbe`, но не попадает в persisted latest/history, статистику или Telegram. Включение режима закрывает текущий downtime и активный node incident на момент паузы, очищает live monitoring/alert state и сохраняет cumulative downtime, incident journal, прежнюю speedtest history, mute и per-node Test URL. `/config/<StableID>` возвращает `200 Maintenance`, а после выключения live status остаётся unknown до следующего реального check.

`nodearchive` открывает node incident и накапливает downtime только при `offline`, закрывая его при recovery или retirement. `proxy_failure` накапливается в отдельной duration/count статистике без роста downtime. Массовый incident создаётся, когда один код причины одновременно затрагивает минимум три и не менее 50% активных нод; корреляция `check_endpoint` остаётся вероятным выводом.

Уже недоступные ноды попадают в отдельный recovery-loop с периодом `PROXY_RECOVERY_INTERVAL` (default 15 секунд, `0` отключает). В одной ограниченной worker-pool итерации TCP и ping выполняются параллельно. Если TCP недоступен, proxy-check пропускается; после `TCP OK` полноценный настроенный proxy-check запускается немедленно. Ping никогда не является gate. Полный обход остаётся независимой контрольной проверкой и предотвращает постоянную блокировку recovery из-за ошибочной TCP-диагностики.

Полные, recovery и ручные availability-checks сериализованы и удерживают Xray lifecycle read-lock; refresh получает write-lock. Быстрые проверки не вызывают обычный Telegram alert-pass, поэтому не увеличивают `FailCount` и не сдвигают reminders. Успешный переход любого issue-состояния в `online` закрывает downtime либо proxy-failure интервал и передаётся в отдельный immediate-recovery путь Telegram. Ручная проверка `StableID` доступна через admin API, в строке ноды и как групповое действие для выбранных строк, а также в карточке ноды Telegram; для уже недоступной ноды она использует тот же TCP-гейт. Speedtest-кнопка `Run` в admin UI также имеет строковый и групповой варианты.

### Remnawave subscription announce

Для Remnawave `proxy_failure` и `offline` равнозначны service-unavailable: пользователь не может использовать proxy. Поэтому announce сохраняет прежнюю семантику down/partial, но outage timer берёт `ProxyFailureSince` для proxy-only failure и `DownSince` для hard offline. В алертах Telegram эти состояния разделены и не меняют друг друга ложным recovery.

Интеграция имеет два независимых gate: env master-switch `REMNAWAVE_ANNOUNCE_ENABLED` разрешает сетевой API client, а persisted `Policy.Enabled` включает автоматическое формирование сообщений. API token существует только в env. Клиент использует `GET /api/hosts`, `GET /api/internal-squads`, `GET /api/external-squads` и `PATCH /api/external-squads`; redirects запрещены, ответы ограничены по размеру, timeout общий для операции. Admin snapshot показывает только presence-флаги token и безопасные поля topology, не возвращает raw inbound или посторонние response headers.

Remnawave не хранит прямую связь Host → External Squad. Доступная аудитории topology вычисляется так:

```text
checker StableID → persisted Host UUID → Host inbound UUID
                 → Internal Squad, содержащий inbound и не исключённый Host
                 → явно настроенная пара → External Squad
```

Disabled/hidden Hosts исключаются. Пара `MonitoringOnly` моделирует сервисный checker squad, но никогда не становится target. Persisted config schema v5 хранит location-first карту `locations[key]`: опциональный `publicLabel` и `members` (`StableID → Host UUID`). Имя checker-ноды и Host remark остаются display-only. Если DNS expansion породил несколько StableID одного Host, они могут ссылаться на один Host UUID; несколько members одной location образуют её redundancy scope. Все подтверждённо offline members дают `down`; сочетание минимум одного healthy и минимум одного подтверждённо offline member даёт `partial`; остальные сочетания с unknown/ambiguous остаются pending/ambiguous. Если все members подтверждённо offline и все находятся в maintenance, location получает отдельное состояние `maintenance`; online maintenance-member считается healthy, а смесь offline maintenance и обычных offline members остаётся обычным `down`. Audience filtering выполняется до агрегации members, поэтому одинаковая структура разных тарифов не переносит health между External Squads.

Activation требует одновременно `DownSince >= OutageMinutes` (default 15) и `MinimumFailures` последовательных полных обходов (default 3). Только `ObserveFullCheck` увеличивает volatile confirmation counter; manual availability и быстрый recovery-loop лишь триггерят reconcile. Restart обнуляет counter и тем самым безопасно задерживает новое сообщение. Active mass incident с `CauseCode=check_endpoint` переводит затронутые locations в ambiguous и не создаёт новый пользовательский announce. Уже опубликованный реальный outage не снимается по ambiguous/pending результату.

Persisted config schema v5 содержит `MessageScenarios` и canonical `locations`: включаемые rules для полной недоступности single/multiple/all locations, частичной недоступности single/multiple locations, полностью offline maintenance-locations и healthy state плюс отдельные fallback для длинных full/partial/maintenance списков и смешанного outage+maintenance текста. v0-v3 получают отсутствующие rules при normalization, а legacy `nodeMappings` из v1-v4 мигрирует в locations по `GroupKey` с fallback на Host UUID. V1 API payload без `messages` сохраняет уже настроенные templates и применяет `normalMessage` только к healthy rule; V2 payload без partial-полей и V3 payload без maintenance-полей сохраняют их текущие значения. Templates используют только контекстно разрешённые `{location}`, `{locations}`, `{unavailable}`, `{affected}`, `{total}`. Backend отклоняет неизвестные braces, URL, Remnawave template delimiters, line breaks и rendered output длиннее 240 runes.

Desired announce пересчитывается на full-check/recovery observations, periodic reconcile, topology sync и сохранение policy/location-members/templates. Remote PATCH нужен только когда итоговый value изменился; изменение affected locations при одинаковом rendered message обновляет ownership state без лишнего PATCH. Отключённый rule даёт пустой status target для своего состояния и тем самым очищает ранее managed suffix после точного ownership check.

Переключение maintenance ставит reconcile в очередь, но само по себе не публикует planned-maintenance status. Нода остаётся member своей location; reconcile использует raw результат её probe-only full checks. Пока такой probe успешен, состояние location и announce не меняются. Подтверждённый offline обрабатывается как обычный member, кроме случая, когда все members location одновременно offline и каждый находится в maintenance: тогда рендерится отдельный maintenance scenario с предупреждением о возможных проблемах. Если одновременно есть обычные down locations, их outage-текст объединяется с maintenance-текстом; oversized комбинация использует versioned mixed fallback. Для activation и recovery действуют общие `OutageMinutes`, `MinimumFailures` и `RecoveryMinutes`. Remote value без точного ownership match не меняется; после `Resume` нода остаётся pending/unknown до реального full check.

Полный outage и fully-offline maintenance имеют приоритет над partial-состоянием других locations. Улучшения `down/maintenance → partial/healthy` удерживают прежнее сообщение до непрерывного `RecoveryMinutes` (default 5); ухудшение применяется сразу после общей outage-confirmation policy. Затем target определяется текущим scenario rule: disabled удаляет только строку статуса checker-а, enabled рендерит соответствующий текст. Если до outage существовал операторский base announce, он восстанавливается/сохраняется байт-в-байт; если header полностью создал checker и healthy rule disabled, он удаляется. Тексты статуса не содержат URL/диагностики.

Перед каждым write worker заново получает External Squads, case-insensitively находит `announce`, сохраняет остальные `responseHeadersAdd` и PATCH-ит объединённую карту. Неуправляемый однострочный `rwEncodeBase64:<body>` разрешено принять как opaque base: checker сохраняет его без нормализации и формирует `<base>\n<status>`. Многострочное значение разрешено восстановить только когда оно заканчивается точным rendered target или healthy message текущей аудитории; совпавший suffix становится managed status, а preceding base сохраняется байт-в-байт. Remnawave обрабатывает всё body после единственного prefix, поэтому subscription response получает одно многострочное `base64:...`. Неизвестное plain или многострочное значение без известного suffix не принимается под управление: это предотвращает повторное добавление suffix после потери runtime-state.

`data/remnawave_announce_state.json` schema v4 хранит точные base и составное remote value, status message, fully-down, partial и maintenance locations. Runtime v1-v3 безопасно мигрирует, добавляя отсутствующие ownership maps без получения новых прав на remote header. Замена, восстановление или удаление разрешены только при точном совпадении remote value с ownership state; при remote mismatch текущий reconcile не выполняет PATCH, снимает ownership и публикует conflict. Сохранившееся вручную изменённое значение остаётся нетронутым. Runtime ownership и opaque base исключены из admin API и backup, чтобы не раскрывать шаблон и не импортировать право изменить header другой инсталляции. Config v5 с policy/pairs/locations типизирован и входит в backup; legacy v1-v4 shapes нормализуются при чтении.

### Обновление подписки

Автоматическое и ручное обновление используют один и тот же callback и защищены от параллельного запуска.

1. Настроенные через повторяемый CLI-флаг или comma-separated `SUBSCRIPTION_URL` источники загружаются параллельно и объединяются. Дублирующийся между источниками `StableID` отклоняется общей проверкой идентичности.
2. При необходимости домены разворачиваются в IP-конфигурации.
3. `xray.PreserveStableIDs` пытается сопоставить новые ноды со старыми.
4. `xray.ValidateStableIDs` отклоняет пустые и дублирующиеся без учёта регистра ID до сравнения или restart.
5. Строится diff added/removed/changed. Пустая новая конфигурация или удаление минимум трёх и не менее 50% прежних нод блокирует scheduled refresh; ручной refresh требует explicit force-confirmation, привязанного opaque fingerprint к конкретному previewed candidate.
6. Если effective-конфигурация не изменилась, restart Xray не выполняется.
7. При изменении refresh получает Xray lifecycle write-lock, дожидается активного speedtest и генерирует candidate рядом с `xray_config.json`. Если Xray отклоняет candidate, предыдущий файл восстанавливается и last-known-good процесс запускается повторно.
8. Только после успешного restart checker и web endpoints получают новый список.
9. Node archive синхронизирует активные/выбывшие записи; mute и pending speed retries исчезнувших ID очищаются.

### Merge идентичности после смены ключа

Смена UUID/пароля/public key, имени или порта закономерно может создать новый generated `StableID`, даже если оператор считает ноду тем же сервером. Автоматически склеивать такие записи нельзя: одинаковое имя не доказывает идентичность. Admin workflow разрешает merge только из retired source в выбранный оператором active target при совпадении нормализованных `SubName`, protocol и server. Имя и порт разрешено менять; preview обязан показать эти расхождения, а UI всегда требует явного выбора target, в том числе при единственном кандидате.

`POST /api/v1/admin/nodes-overview/merge/preview` повторно читает node archive и speedtest history, вычисляет итоговый размер истории, возвращает предупреждения об изменившихся display name/port и выдаёт SHA-256 confirmation token, привязанный к source/target, `RetiredAt` и полям идентичности. `POST /api/v1/admin/nodes-overview/merge` заново строит preview и ставит инструкцию в `data/.node-merge-pending`; повтор того же запроса идемпотентен, а новый подтверждённый preview может заменить ещё не применённую инструкцию. Staging restore и merge сериализованы одним in-process gate и дополнительно проверяют transaction-каталоги друг друга.

На следующем startup merge применяется только если target всё ещё активен, source не вернулся в подписку, persisted identity не изменилась и оба JSON проходят те же typed/duplicate-key проверки, что backup restore. Source history и latest result re-keyed в target, нормализуются к активным display-полям, сортируются newest-first и дедуплицируются. В node archive суммируются cumulative downtime и incident count, берутся earliest `FirstSeenAt`, latest status timestamps, maximum longest downtime и более свежие GeoIP-группы; incident journal сохраняет opaque ID, но заменяет source StableID и дедуплицирует mass scope. `MergedNodes[target]` хранит отсортированный lineage всех прежних StableID. Live-поля active target остаются целевыми, retired source удаляется.

Оригиналы обоих файлов остаются в `data/.node-merge-rollback`, пока speedtest, node archive и Telegram не завершат `Load`. Любая ошибка вызывает byte-for-byte rollback и обязательный restart. Durable marker позволяет отличить неподтверждённое применение от оборванной очистки уже подтверждённого merge.

Admin UI хранит выбранную staged-пару локально только как ожидание результата. После restart источником истины остаётся `/nodes-overview`: успешное применение подтверждается отсутствием retired source и наличием его StableID в `MergedFromStableIDs` active target. Тогда UI показывает dismissible success notice и постоянный `Merge applied` marker у target; до restart показывается отдельное staged-состояние без ложного заявления о завершённом merge.

### Speedtest

Ручной запуск из админки и Telegram сначала выполняет availability-check выбранных нод и передаёт в speedtest только ноды с успешным proxy-check. Плановые запуски и automatic confirmation retry также требуют `online`, поэтому `proxy_failure` и `offline` не запускают speedtest. Персональный `NodeTestURLs[StableID]` имеет приоритет над глобальным URL. После availability gate manual run остаётся двухфазным: primary для прошедших gate нод, затем country fallback для low-speed/technical-error. Persisted history, latest snapshot и `RunReport` получают один финальный результат на `StableID`.

Scheduler хранит абсолютный `nextRunAt` рядом с `ScheduleConfig`. Перезапуск процесса и сохранение нетайминговых настроек не сдвигают этот deadline. Смена интервала сохраняет прежний временной якорь, поэтому уже прошедшая часть периода не теряется; если deadline прошёл во время downtime, после startup выполняется один немедленный запуск, а следующий назначается на полный интервал вперёд без последовательного воспроизведения всех пропущенных тиков. Старые schedule-файлы без `nextRunAt` получают первый deadline от времени startup.

После технической ошибки основного URL либо результата ниже текущего Telegram `LowSpeedThresholdMbps` speedtest запрашивает `ClaimedCountryCode` из node archive по `StableID` и выбирает до двух адресов из `data/country-test-urls.yaml`. Нулевой threshold отключает fallback по скорости. Telegram service синхронизирует threshold с manager при загрузке и каждом изменении config. GeoIP и результаты country-match в маршрутизации резервов не участвуют. Выбор устойчивый, а не максимизирующий Mbps: сначала используется последний успешный endpoint этой ноды, затем общая успешность/свежесть и только потом статический `priority`. Перед полным замером выполняется 64-КиБ probe с timeout 8 секунд. Per-node cooldown равен 10 минутам; три последовательных сбоя от минимум двух нод за 5 минут исключают endpoint глобально на 30 минут. Health переживает restart в `speedtest_url_health.json`.

Завершённый резерв сохраняет фактический URL, исходную ошибку и метаданные endpoint в `Result` и показывается в UI символом `↪`. Только резерв со скоростью не ниже threshold получает `TelegramAlertSuppressed`; медленный резерв остаётся кандидатом на общее 30-минутное confirmation. Если fallback был вызван медленным основным результатом, но все доступные резервы завершились ошибкой, manager возвращает исходный low-speed результат, чтобы тот не потерял 30-минутный retry. Неразрешённый `context deadline exceeded` использует ту же очередь подтверждения; отдельного 5-минутного пути нет. Это объединение не меняет семантику результата: timeout остаётся точным значением `Error`/`PrimaryError`, считается техническим failure и не превращается в low-speed. Прямой Telegram-запрос остаётся обязательным ответом инициатору. Если все резервы не сработали после другой технической ошибки основного URL, сохраняется и уведомляется исходная ошибка.

Telegram-запуск добавляет к неперсистентному `RunRequest` исходные chat ID и topic ID. `RunReport` переносит этот адрес до reporter, поэтому прямой результат возвращается инициатору даже при другом настроенном alert-чате и независимо от режима автоматических speed-report.

Каждый speedtest получает read-lock Xray lifecycle до выбора proxy pointers и SOCKS-портов и освобождает его только после сбора всех результатов. Availability-checks используют тот же lifecycle lock и дополнительно сериализуются между собой. Поэтому restart Xray не может пройти посередине сетевой проверки, а новая проверка не стартует на старой конфигурации после начала refresh.

Результаты хранятся по `StableID`. Retention основан на возрасте, а не на количестве:

- default: 60 дней;
- допустимо: 1–3650 дней;
- уменьшение значения немедленно отбрасывает более старые записи;
- `Failures` в Nodes Overview вычисляется по сохранённой speedtest history;
- cumulative downtime хранится отдельно в node archive и не ограничивается retention speedtest.

Админский `Controls` ограничен контекстом выбранных `StableID`: ручной speedtest и его параметры, персональный Test URL, Maintenance/Resume, Telegram mute, фильтры и расписание для выбранного набора. Глобальные update/settings workflow находятся в отдельной верхней вкладке `Settings`: Subscription, общий History retention, Telegram и Backup. Разделение является только UI-контрактом; существующие admin API и persisted owners не меняются.

Dashboard админки отображает активные ноды раскрываемыми строками, но не меняет контракт управления: клик по неинтерактивной части шапки или стрелке переключает карточку, а строковые и групповые `Check`/`Run`, чекбокс и копирование IP/Server остаются независимыми действиями по `StableID`. Контекстный переключатель `Maintenance`/`Resume` находится в `Controls → Actions`, работает для одной выбранной ноды и дублируется в `Nodes Overview`. Check/Run для maintenance-ноды остаются доступны как разовый admin probe; групповые действия также включают выбранные paused-ноды, не снимая их maintenance. Speed-probe помечается в current results, но исключается из dashboard KPI, persisted history и графика. Любое отображаемое checker-node поле IP/Server в dashboard/admin копирует адрес без порта; отдельная ссылка IP details в Nodes Overview остаётся самостоятельным действием. Основной dashboard включает автообновление по availability-check interval по умолчанию и обновляет состояние по `StableID`. Admin polling каждые 30 секунд повторно получает `/proxies`, `/speed-tests`, `/telegram` и `/nodes-overview`, а не перерисовывает старый snapshot; скрытая вкладка не создаёт запросы и немедленно обновляется при возврате. Header master-checkbox применяет выбор только к текущему `filteredProxies()` и синхронизирует checked/indeterminate/disabled при каждом render selection-state. Открытые карточки хранятся как множество `StableID`, поэтому несколько панелей работают одновременно и имеют отдельные range/history/loading/request-state. Пока порядок видимых `StableID` не изменился, reconcile обновляет поля существующих карточек на месте и не заменяет список через `innerHTML`; это сохраняет DOM панели, раскрытие, график и его горизонтальный scroll при polling, availability-check и выборе строк. Полная перестройка выполняется только при изменении фильтрованного состава или порядка. Раскрытие, закрытие и изменение высоты локально загруженного содержимого анимируются независимо с учётом `prefers-reduced-motion`; устаревший history-response отбрасывается request-счётчиком конкретной карточки. Раскрытие запрашивает существующую speedtest history с необязательными RFC3339-границами `from` (включительно) и `to` (не включительно). Предустановлены окна 24 часа, 3, 7, 14 и 30 дней; произвольный период задаётся календарными датами. Процент success использует все результаты выбранного периода как знаменатель; в числитель входят только завершённые замеры с Mbps не ниже текущего low-speed threshold, поэтому low-speed, offline и error уменьшают процент. График строится только по реальным замерам Mbps, показывает их area-заливкой, отмечает последний замер вертикальным пунктиром, а при движении курсора привязывает crosshair и tooltip к ближайшему сохранённому результату. Low-speed/error/offline сохраняются как отдельные состояния; отсутствующие интервалы обозначаются только мягким fade без резкой вертикальной кромки, пунктирным gap bridge и ограниченной plot-area error-зоной, не входят в статистику и не превращаются в вымышленные Mbps-замеры.

Нижний правый переключатель `Speedtest`/`Availability` меняет dataset того же chart-блока. Availability dataset хранится в `node_registry.json` по `StableID` за тот же `HistoryRetentionDays`, что и speedtest history, содержит один sample на фактический `CheckedAt` и не включает maintenance/probe-only checks. Сохранение `Settings → History` сразу применяет уменьшенный срок к обеим историям, не меняя выбранные ноды и остальные поля расписания. Endpoint `/api/v1/admin/availability/history` использует те же inclusive `from`/exclusive `to` границы, а UI сохраняет отдельные loading/result/request states двух режимов при общем выбранном периоде.

Текущие `Results`, latest-result и dashboard KPI пересекают speedtest snapshot с active-набором `/proxies` по `StableID`. Retired latest/history сохраняются в persisted state для архивного просмотра и merge, но не участвуют в текущей operational-сводке; разовый maintenance-probe active-ноды остаётся видимым в `Results`, хотя исключается из KPI и history.

Публичный dashboard и admin UI используют общий клиентский словарь EN/RU. Выбор хранится в `localStorage` под ключом `xray-checker-language`, поэтому сохраняется между экранами и перезагрузками на одном origin. Локализатор обрабатывает статический DOM, атрибуты доступности и добавляемые при render/polling элементы; operator-controlled данные, шаблоны сообщений и технические термины не переводятся. `localization.js` подключается с versioned URL и отдаётся с `Cache-Control: no-cache`, чтобы обновления словаря не застревали в годовом immutable-кэше остальных static assets.

### Резервное копирование и восстановление

В backup входят только разрешённые JSON-файлы из `data/`. Каждый файл описан в `manifest.json` с размером и SHA-256. Environment, geo, сгенерированный Xray config, чувствительные Telegram-поля, Remnawave API token и announce ownership runtime исключены. `remnawave_announce_config.json` входит в архив после typed validation.

Автоматический scheduler запускается сразу после startup и затем ждёт `00:05 UTC`. За один UTC-день создаётся не более одного архива; удаляются архивы старше семи суток и всё сверх семи новейших файлов.

Restore не заменяет работающие файлы сразу. Manifest и persisted JSON проверяются на структуру, типы, регистронезависимые дубликаты ключей и поддерживаемую схему. Безопасные файлы помещаются в `data/.restore-pending`, а на следующем старте заменяются с журналируемой rollback-копией. Файлы из разрешённого набора, которых нет в восстановленном архиве, удаляются как отсутствующие в снимке.

После установки файлов транзакция остаётся неподтверждённой до успешного `Load` у speedtest, node archive, Remnawave announce config и Telegram. Ошибка вызывает rollback и обязательный restart. Commit отмечается отдельным durable-маркером; startup различает оборванное применение, неподтверждённый restore и оборванную очистку уже подтверждённой транзакции.

### Telegram output

Каждый основной экран имеет пару представлений: Rich HTML для `sendRichMessage`/`editMessageText.rich_message` и компактный обычный HTML fallback. Rich-вариант строится как сводка, список проблем и раскрываемые технические детали; fallback не повторяет StableID, threshold и timing в каждой строке. Возможность Rich Messages кешируется только при однозначном ответе API. Сетевая ошибка не запускает повторную fallback-отправку, чтобы сохранить семантику at-most-once на неопределённом результате запроса.

Режим фонового speed-report определяется `SpeedReportMode`: `always` отправляет чистые scheduled-отчёты, `issues` скрывает чистые, `disabled` отключает их. Country-fallback со скоростью не ниже threshold исключается из автоматических отчётов. Низкая скорость запуска по расписанию или из админ-панели сначала проверяется через резервный URL; отдельный тест затронутых `StableID` через 30 минут создаётся только после низкой скорости либо ошибки резервов. До подтверждения эта часть отчёта подавляется; повторно низкий, offline или error-результат отправляется как подтверждённая проблема, успешный повтор не создаёт сообщения. Неразрешённый `context deadline exceeded` в `Result.Error` или `Result.PrimaryError` использует тот же 30-минутный retry с исходным TestConfig; финальный fallback не ниже threshold закрывает событие без retry. Общими являются только timer, deduplication и source повтора: deadline продолжает учитываться как `failed`, сохраняет исходный текст ошибки и не входит в `slow`. Успешный финальный `manual` result до due time удаляет для своего `StableID` общий pending entry и останавливает timer; low-speed/error manual result и любой `schedule` result чужой pending entry не отменяют. Runtime персистит нейтральный тип `speed-confirmation`; legacy-записи без типа и `low-speed` мигрируют без изменения due time, а legacy `deadline` переносится в общую очередь со сдвигом due time с 5 до 30 минут от исходного события. Причины дедуплицируются по одному ключу `StableID`.

Прямой результат Telegram-команды не является фоновым report: он отправляется сразу в исходные chat/topic и не фильтруется настройкой автоматических отчётов. Maintenance-ноды отсутствуют в Telegram status/issues/list/transport candidates и не могут быть выбраны через non-admin command path. Интерактивные экраны «Проблемы» и «Замеры» фильтруют latest speedtest results по набору текущих monitored proxy `StableID`, поэтому retired history и admin maintenance-probes сохраняют нужную админскую видимость, но не попадают в Telegram-текст и кнопки истории. Экран «Замеры» имеет Rich HTML-таблицу и компактный HTML fallback, как остальные основные экраны.

Recovery alert хранит `RecoveryPending`, время и latency до успешной отправки; сбой Telegram не стирает alert state. Recovery отправляется только после ранее подтверждённой отправки down-alert. Быстрый checker передаёт подтверждённый переход online напрямую, без ожидания `AlertCheckMinutes`; обычный alert-pass по-прежнему отвечает за `Failed checks`, down-alerts и reminders. Одновременные alert/recovery проходы сериализованы, чтобы одну pending recovery не отправили дважды.

## Идентичность нод

`StableID` — первые 16 hex-символов SHA-256 от набора параметров подключения: protocol, server, port, UUID/пароль, а также присутствующих SNI, transport type, security и public key.

Критические следствия:

- `Name` и `SubName` не входят в generated ID;
- одинаковые имена допустимы и не склеивают ноды;
- checker не нормализует и не переименовывает входные имена — маркеры вроде `^~2~^` приходят от источника подписки;
- при неизменных параметрах подключения статистика переживает переименование;
- fallback-сопоставление по имени применяется только для однозначной пары;
- две конфигурации с одинаковыми StableID, включая различие только регистром, отклоняются до создания status/history maps.

## Persisted state

| Файл | Владелец | Правило |
| --- | --- | --- |
| `data/node_registry.json` | `nodearchive` | долгоживущая статистика, availability history с общим для speedtest сроком хранения, maintenance mode, retired-ноды, incident journal и lineage объединённых StableID |
| `data/speedtest_results.json` | `speedtest` | latest result и temporal history |
| `data/speedtest_schedule.json` | `speedtest` | расписание, абсолютный deadline следующего запуска, фильтры, URL и retention |
| `data/telegram_config.json` | `telegram` | editable settings; secrets могут переопределяться env |
| `data/node_alert_state.json` | `telegram` | состояние последовательностей алертов, диагностические причины и pending 30-минутные speedtest confirmation retries |
| `data/remnawave_announce_config.json` | `remnawave` | versioned policy, Internal/External pairs и location-first members (`location key → StableID → Host UUID`); входит в backup |
| `data/remnawave_announce_state.json` | `remnawave` | последнее exact managed value и announced locations; не входит в backup |
| `data/backups/*.zip` | `backup` | до 7 автоматических архивов за последние 7 суток |
| `data/.node-merge-{pending,applied,rollback}` | `nodemerge` | временная crash-safe транзакция переноса identity/history |

`data/` должен быть постоянным volume и доступен UID `1000` внутри production image.

## Доступ и безопасность

- `/admin` и `/api/v1/admin/*` всегда должны оставаться за Basic Auth.
- В server-mode `METRICS_USERNAME` и `METRICS_PASSWORD` обязательны; публичных встроенных credentials нет. `RUN_ONCE=true` не поднимает HTTP-сервер и является единственным исключением.
- `WEB_PUBLIC=true` разрешён только вместе с `METRICS_PROTECTED=true`.
- `Caddyfile.example` — версионируемый источник шаблона; локальный `Caddyfile` создаётся копированием, игнорируется Git и публикует только status page, `/static`, `/config` и публичный список прокси.
- Нельзя писать Telegram token, Remnawave API token, chat ID, admin IDs, subscription URL с credentials или Basic Auth password в логи, API и backup.
- Restore принимает только известные пути, ограничивает размер архива, проверяет manifest/hash/JSON-схемы и не следует symlink/reparse-point вместо state-файлов.

## Известные ограничения

- Имена нод отображаются ровно как в источнике; косметические суффиксы Remnawave checker не исправляет.
- Retired-ноды сохраняются в Nodes Overview до ручного удаления либо подтверждённого merge в однозначно совпавшую active-ноду.
- Восстановление требует перезапуска приложения.
- Rich Messages зависят от версии Telegram Bot API; при отсутствии метода бот использует менее выразительный компактный HTML.
- GeoIP refresh выполняется только для active-нод, использует внешние сервисы и может быть недоступен из-за сети или rate limit.
- `check_endpoint` является корреляционным выводом по нескольким нодам; он не доказывает отказ внешнего сервиса без отдельной проверки с другой точки.
- Remnawave `external-squads:update` не ограничен одним полем; компрометация token позволяет менять и другие настройки External Squad. Минимальные scopes и сетевое ограничение остаются обязательными.
- User-specific Remnawave Response Rule имеет более высокий приоритет, чем External Squad header override, поэтому может скрыть управляемый `announce` для отдельного пользователя.
- Node merge не переносит location membership из Remnawave config: после смены StableID оператор вручную заменяет retired member на новый active StableID в нужной location. Host UUID и location key не переносятся автоматически, что сохраняет узкий transactional scope merge и исключает неявное изменение аудитории.

## Текущее состояние

Реализованы и покрыты Go-тестами: сохранение и проверка StableID при refresh, suspicious-diff guard и Xray config rollback, staged node identity merge с rollback/lineage, persisted maintenance mode со сквозным исключением из monitoring workflows, Xray lifecycle-lock speedtest, единый scheduled/Telegram TestConfig, temporal speedtest retention, persisted 30-минутные speedtest confirmation retries с миграцией legacy deadline entries, node archive и incident journal, детерминированные failure codes, корреляция массовых сбоев, audience-aware Remnawave announce с ownership guard, структурированный Telegram output, retryable recovery alerts, ручные/автоматические backup и транзакционный staged restore. GitHub Actions и Dependabot в форке отключены; CI, Docker Hub description и release jobs на GitHub не выполняются. Перед релизом обязательны локальные проверки из [`AGENTS.md`](AGENTS.md).
