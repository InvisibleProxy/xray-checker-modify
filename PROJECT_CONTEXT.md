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
8. Запускается встроенный Xray Core с уже сгенерированным `xray_config.json`.
9. Загружаются speedtest, node archive, Remnawave announce config и Telegram state. Ошибка backup-владельца откатывает применённый node merge либо restore и останавливает startup. Отдельный Remnawave ownership runtime не входит в restore; его ошибка безопасно запрещает remote writes.
10. Только после успешной загрузки всех владельцев активная транзакция подтверждается и rollback-копия удаляется.
11. Восстанавливается накопленный downtime и синхронизируется speedtest history.
12. Запускаются автоматические бэкапы, Telegram, Remnawave reconciliation worker, speedtest scheduler, полные proxy-checks и быстрый recovery-loop недоступных нод.
13. Поднимается HTTP-сервер.

## Основные workflow

### Проверка доступности

Для каждой ноды создаётся SOCKS-inbound на `XRAY_START_PORT + Index`. Полный обход с периодом `PROXY_CHECK_INTERVAL` выполняет выбранный метод `ip`, `status` или `download` через каждый inbound без предварительного TCP-гейта. При провале proxy-check переход в offline и `DownSince` сохраняются до TCP/ping-диагностики, а её результаты дописываются только если нода всё ещё offline. Непривилегированный Linux ICMP datagram socket может переписать Echo ID, поэтому ping reply сопоставляется по типу, sequence и уникальному payload. Ошибка check-метода сначала классифицируется как DNS, TCP, proxy handshake/timeout, TLS, HTTP, unchanged source IP, incomplete download или unknown; затем прямые host diagnostics могут уточнить её. Статус, причина и диагностика хранятся по `StableID`; после итерации обновляются downtime, incident journal, Telegram и Pushgateway.

`nodearchive` открывает node incident при первом offline, обновляет его диагностическую причину и закрывает при recovery или retirement. Массовый incident создаётся, когда один код причины одновременно затрагивает минимум три и не менее 50% активных нод: сначала проверяется global scope, затем отдельные подписки. Если разные серверы имеют доступный TCP-порт, но одинаково проваливают DNS/HTTP/TLS/timeout check, общий `check_endpoint` отмечается как вероятная корреляционная причина, а не доказанный факт.

Уже недоступные ноды попадают в отдельный recovery-loop с периодом `PROXY_RECOVERY_INTERVAL` (default 15 секунд, `0` отключает). В одной ограниченной worker-pool итерации TCP и ping выполняются параллельно. Если TCP недоступен, proxy-check пропускается; после `TCP OK` полноценный настроенный proxy-check запускается немедленно. Ping никогда не является gate. Полный обход остаётся независимой контрольной проверкой и предотвращает постоянную блокировку recovery из-за ошибочной TCP-диагностики.

Полные, recovery и ручные availability-checks сериализованы и удерживают Xray lifecycle read-lock; refresh получает write-lock. Быстрые проверки не вызывают обычный Telegram alert-pass, поэтому не увеличивают `FailCount` и не сдвигают reminders. Успешный переход offline → online закрывает downtime и передаётся в отдельный immediate-recovery путь Telegram. Ручная проверка `StableID` доступна через admin API, в строке ноды и как групповое действие для выбранных строк, а также в карточке ноды Telegram; для уже недоступной ноды она использует тот же TCP-гейт. Speedtest-кнопка `Run` в admin UI также имеет строковый и групповой варианты.

### Remnawave subscription announce

Интеграция имеет два независимых gate: env master-switch `REMNAWAVE_ANNOUNCE_ENABLED` разрешает сетевой API client, а persisted `Policy.Enabled` включает автоматическое формирование сообщений. API token существует только в env. Клиент использует `GET /api/hosts`, `GET /api/internal-squads`, `GET /api/external-squads` и `PATCH /api/external-squads`; redirects запрещены, ответы ограничены по размеру, timeout общий для операции. Admin snapshot показывает только presence-флаги token и безопасные поля topology, не возвращает raw inbound или посторонние response headers.

Remnawave не хранит прямую связь Host → External Squad. Доступная аудитории topology вычисляется так:

```text
checker StableID → persisted Host UUID → Host inbound UUID
                 → Internal Squad, содержащий inbound и не исключённый Host
                 → явно настроенная пара → External Squad
```

Disabled/hidden Hosts исключаются. Пара `MonitoringOnly` моделирует сервисный checker squad, но никогда не становится target. `NodeMappings` всегда keyed by exact `StableID`; имя ноды и Host remark display-only. Если DNS expansion породил несколько StableID одного Host, они могут ссылаться на один Host UUID. Несколько Hosts/StableID с одинаковым `GroupKey` образуют публичную redundancy group: она недоступна только когда все её mapped members подтверждённо offline.

Activation требует одновременно `DownSince >= OutageMinutes` (default 15) и `MinimumFailures` последовательных полных обходов (default 3). Только `ObserveFullCheck` увеличивает volatile confirmation counter; manual availability и быстрый recovery-loop лишь триггерят reconcile. Restart обнуляет counter и тем самым безопасно задерживает новое сообщение. Active mass incident с `CauseCode=check_endpoint` переводит затронутые groups в ambiguous и не создаёт новый пользовательский announce. Уже опубликованный реальный outage не снимается по ambiguous/pending результату.

После online group удерживается в сообщении до непрерывного `RecoveryMinutes` (default 5). Специальное recovery-сообщение не создаётся: target становится optional `NormalMessage`, а при пустом значении удаляется только строка статуса checker-а. Если до outage существовал операторский base announce, он восстанавливается байт-в-байт; если header полностью создал checker, он удаляется. Тексты статуса не содержат URL/диагностики.

Перед каждым write worker заново получает External Squads, case-insensitively находит `announce`, сохраняет остальные `responseHeadersAdd` и PATCH-ит объединённую карту. Неуправляемый однострочный `rwEncodeBase64:<body>` разрешено принять только как opaque base: checker сохраняет его без нормализации и формирует `<base>\n<status>`. Remnawave обрабатывает всё body после единственного prefix, поэтому subscription response получает одно многострочное `base64:...`. Неизвестное plain или уже многострочное значение не принимается под управление: это предотвращает повторное добавление suffix после потери runtime-state.

`data/remnawave_announce_state.json` schema v2 хранит точные base и составное remote value, status message и groups. Runtime v1 безопасно мигрирует как ownership целого значения без base. Замена, восстановление или удаление разрешены только при точном совпадении remote value с ownership state; при remote mismatch текущий reconcile не выполняет PATCH, снимает ownership и публикует conflict. Сохранившееся вручную изменённое значение остаётся нетронутым. Runtime ownership и opaque base исключены из admin API и backup, чтобы не раскрывать шаблон и не импортировать право изменить header другой инсталляции. Config с policy/pairs/mappings типизирован и входит в backup.

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

Ручной запуск из админки получает config из формы и сохраняет актуальный глобальный URL. Плановый и Telegram-запуски берут последнюю сохранённую `ScheduleConfig`, включая URL, `MaxBytes`, timeout и concurrency. Персональный `NodeTestURLs[StableID]` имеет приоритет над глобальным URL.

После технической ошибки основного URL speedtest запрашивает `ClaimedCountryCode` из node archive по `StableID` и выбирает до двух адресов из `data/country-test-urls.yaml`. GeoIP и результаты country-match в маршрутизации резервов не участвуют. Выбор устойчивый, а не максимизирующий Mbps: сначала используется последний успешный endpoint этой ноды, затем общая успешность/свежесть и только потом статический `priority`. Перед полным замером выполняется 64-КиБ probe с timeout 8 секунд. Per-node cooldown равен 10 минутам; три последовательных сбоя от минимум двух нод за 5 минут исключают endpoint глобально на 30 минут. Health переживает restart в `speedtest_url_health.json`.

Успешный резерв сохраняет фактический URL, исходную ошибку и метаданные endpoint в `Result`, помечается `TelegramAlertSuppressed` и показывается в UI символом `↪`. Автоматический Telegram reporter обычно исключает такой результат, включая low-speed confirmation. Исключение — резервный результат ниже threshold после `context deadline exceeded` основного URL: он остаётся видимым как немедленная low-speed проблема и запускает отдельный повтор исходного теста через 5 минут. Прямой Telegram-запрос остаётся обязательным ответом инициатору. Если все резервы не сработали, сохраняется и уведомляется исходная ошибка. Низкая скорость без технической ошибки резерв не включает.

Telegram-запуск добавляет к неперсистентному `RunRequest` исходные chat ID и topic ID. `RunReport` переносит этот адрес до reporter, поэтому прямой результат возвращается инициатору даже при другом настроенном alert-чате и независимо от режима автоматических speed-report.

Каждый speedtest получает read-lock Xray lifecycle до выбора proxy pointers и SOCKS-портов и освобождает его только после сбора всех результатов. Availability-checks используют тот же lifecycle lock и дополнительно сериализуются между собой. Поэтому restart Xray не может пройти посередине сетевой проверки, а новая проверка не стартует на старой конфигурации после начала refresh.

Результаты хранятся по `StableID`. Retention основан на возрасте, а не на количестве:

- default: 60 дней;
- допустимо: 1–3650 дней;
- уменьшение значения немедленно отбрасывает более старые записи;
- `Failures` в Nodes Overview вычисляется по сохранённой speedtest history;
- cumulative downtime хранится отдельно в node archive и не ограничивается retention speedtest.

Dashboard админки отображает активные ноды раскрываемыми строками, но не меняет контракт управления: клик по неинтерактивной части шапки или стрелке переключает карточку, а строковые и групповые `Check`/`Run` и чекбокс остаются независимыми действиями по `StableID`. Header master-checkbox применяет выбор только к текущему `filteredProxies()` и синхронизирует checked/indeterminate/disabled при каждом render selection-state. Открытые карточки хранятся как множество `StableID`, поэтому несколько панелей работают одновременно и имеют отдельные range/history/loading/request-state. Пока порядок видимых `StableID` не изменился, reconcile обновляет поля существующих карточек на месте и не заменяет список через `innerHTML`; это сохраняет DOM панели, раскрытие, график и его горизонтальный scroll при polling, availability-check и выборе строк. Полная перестройка выполняется только при изменении фильтрованного состава или порядка. Раскрытие, закрытие и изменение высоты локально загруженного содержимого анимируются независимо с учётом `prefers-reduced-motion`; устаревший history-response отбрасывается request-счётчиком конкретной карточки. Раскрытие запрашивает существующую speedtest history с необязательными RFC3339-границами `from` (включительно) и `to` (не включительно). Предустановлены окна 24 часа, 3, 7, 14 и 30 дней; произвольный период задаётся календарными датами. Процент success использует все результаты выбранного периода как знаменатель; в числитель входят только завершённые замеры с Mbps не ниже текущего low-speed threshold, поэтому low-speed, offline и error уменьшают процент. График строится только по реальным замерам Mbps, показывает их area-заливкой, отмечает последний замер вертикальным пунктиром, а при движении курсора привязывает crosshair и tooltip к ближайшему сохранённому результату. Low-speed/error/offline сохраняются как отдельные состояния; отсутствующие интервалы обозначаются только мягким fade без резкой вертикальной кромки, пунктирным gap bridge и ограниченной plot-area error-зоной, не входят в статистику и не превращаются в вымышленные Mbps-замеры.

### Резервное копирование и восстановление

В backup входят только разрешённые JSON-файлы из `data/`. Каждый файл описан в `manifest.json` с размером и SHA-256. Environment, geo, сгенерированный Xray config, чувствительные Telegram-поля, Remnawave API token и announce ownership runtime исключены. `remnawave_announce_config.json` входит в архив после typed validation.

Автоматический scheduler запускается сразу после startup и затем ждёт `00:05 UTC`. За один UTC-день создаётся не более одного архива; удаляются архивы старше семи суток и всё сверх семи новейших файлов.

Restore не заменяет работающие файлы сразу. Manifest и persisted JSON проверяются на структуру, типы, регистронезависимые дубликаты ключей и поддерживаемую схему. Безопасные файлы помещаются в `data/.restore-pending`, а на следующем старте заменяются с журналируемой rollback-копией. Файлы из разрешённого набора, которых нет в восстановленном архиве, удаляются как отсутствующие в снимке.

После установки файлов транзакция остаётся неподтверждённой до успешного `Load` у speedtest, node archive, Remnawave announce config и Telegram. Ошибка вызывает rollback и обязательный restart. Commit отмечается отдельным durable-маркером; startup различает оборванное применение, неподтверждённый restore и оборванную очистку уже подтверждённой транзакции.

### Telegram output

Каждый основной экран имеет пару представлений: Rich HTML для `sendRichMessage`/`editMessageText.rich_message` и компактный обычный HTML fallback. Rich-вариант строится как сводка, список проблем и раскрываемые технические детали; fallback не повторяет StableID, threshold и timing в каждой строке. Возможность Rich Messages кешируется только при однозначном ответе API. Сетевая ошибка не запускает повторную fallback-отправку, чтобы сохранить семантику at-most-once на неопределённом результате запроса.

Режим фонового speed-report определяется `SpeedReportMode`: `always` отправляет чистые scheduled-отчёты, `issues` скрывает чистые, `disabled` отключает их. Успешно завершённые country-fallback результаты обычно исключаются из автоматических отчётов. Низкая скорость запуска по расписанию или из админ-панели подтверждается отдельным тестом затронутых `StableID` через 30 минут. До подтверждения обычная low-speed часть отчёта подавляется; повторно низкий, offline или error-результат отправляется как подтверждённая проблема, успешный повтор не создаёт сообщения. `context deadline exceeded` в `Result.Error` или `Result.PrimaryError` создаёт независимый retry затронутых `StableID` через 5 минут с исходным TestConfig. Первичное error-уведомление остаётся немедленным, а low-speed fallback после такого timeout и low-speed результат самого 5-минутного повтора также отправляются сразу без постановки в новую 30-минутную очередь. Pending retry персистится вместе с типом (`low-speed` или `deadline`), исходным TestConfig, due time и `StableID`; старые записи без типа нормализуются как low-speed, оба типа восстанавливаются после restart и дедуплицируются независимо.

Прямой результат Telegram-команды не является фоновым report: он отправляется сразу в исходные chat/topic и не фильтруется настройкой автоматических отчётов. Экран «Замеры» имеет Rich HTML-таблицу и компактный HTML fallback, как остальные основные экраны.

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
| `data/node_registry.json` | `nodearchive` | долгоживущая статистика, retired-ноды, incident journal и lineage объединённых StableID |
| `data/speedtest_results.json` | `speedtest` | latest result и temporal history |
| `data/speedtest_schedule.json` | `speedtest` | расписание, фильтры, URL и retention |
| `data/telegram_config.json` | `telegram` | editable settings; secrets могут переопределяться env |
| `data/node_alert_state.json` | `telegram` | состояние последовательностей алертов, диагностические причины и pending low-speed/deadline retries |
| `data/remnawave_announce_config.json` | `remnawave` | versioned policy, Internal/External pairs и StableID-keyed Host mappings; входит в backup |
| `data/remnawave_announce_state.json` | `remnawave` | последнее exact managed value и announced groups; не входит в backup |
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
- Node merge не переносит `StableID → Host UUID` из Remnawave config: после смены StableID оператор вручную назначает active target тот же Host. Это сохраняет узкий transactional scope merge и исключает неявное изменение аудитории.

## Текущее состояние

Реализованы и покрыты Go-тестами: сохранение и проверка StableID при refresh, suspicious-diff guard и Xray config rollback, staged node identity merge с rollback/lineage, Xray lifecycle-lock speedtest, единый scheduled/Telegram TestConfig, temporal speedtest retention, persisted low-speed/deadline retries, node archive и incident journal, детерминированные failure codes, корреляция массовых сбоев, audience-aware Remnawave announce с ownership guard, структурированный Telegram output, retryable recovery alerts, ручные/автоматические backup и транзакционный staged restore. GitHub Actions и Dependabot в форке отключены; CI, Docker Hub description и release jobs на GitHub не выполняются. Перед релизом обязательны локальные проверки из [`AGENTS.md`](AGENTS.md).
