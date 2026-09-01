# Remote Diagnostics через distributed probe-agent'ов

> Статус: защищённый manual workflow реализован полностью; из этапа 2 реализован opt-in `auto_speed_fallback` с одним агентом, cooldown/concurrency и read-only обогащением уже разрешённого Telegram speed alert.

## Текущее состояние реализации

Реализованы безопасный фундамент этапа 0 и control plane этапа 1:

- versioned session schema v2, job schema v1 и observation schema v2;
- ограниченный in-memory `DiagnosticSessionManager` без persisted state;
- binding observation по `SessionID`, `JobID`, nonce, `StableID`, generation, SHA-256 config fingerprint и server-owned test profile;
- fail-closed signature verifier и Ed25519-проверка с controller-bound enrollment/key lifecycle;
- stale, old-generation, duplicate, replay и unknown-job rejection;
- отдельный session export без execution proxy config, credentials, raw errors и произвольных URL;
- тестовый запрет зависимостей `diagnostics/` от operational packages проекта.
- persisted controller registry с безопасной v0 → v1 normalization, one-use enrollment token только в виде SHA-256 и exact expected source IP;
- create/re-enroll/revoke workflow в Admin → Settings → Agents с одноразовым персональным Compose;
- отдельные Ed25519 identity и observation keys, которые агент генерирует и хранит в persistent named volume;
- heartbeat, job poll и observation submit с identity-подписью, timestamp window и persisted monotonic sequence против replay после restart обеих сторон;
- control client, который всегда dial-ит exact controller IP, проверяет TLS hostname из URL, не использует environment proxy и не следует redirects;
- отдельные `Dockerfile.agent` и `docker-compose.agent.yml` для Linux с outbound-only network model, read-only root filesystem, non-root UID, drop всех capabilities, resource limits и persistent identity volume.
- manual session из раскрытой карточки active-ноды с выбором одного подключённого агента;
- ephemeral credential-bearing assignment queue, не входящая в session export или backup;
- семь fixed profile ID, agent-owned endpoint/profile parameters и fingerprint validation;
- временный Xray config с mode `0600` внутри tmpfs, loopback-only SOCKS inbound, embedded Xray lifecycle и обязательное удаление после job;
- proxy-check, TCP/ping evidence, direct-connectivity control, отдельная observation-подпись и generation/fingerprint recheck перед приёмом;
- cancel, sanitized JSON export и вероятностная summary без operational side effects.
- opt-in `auto_speed_fallback`, выбирающий одну healthy idle probe, с per-node cooldown, concurrency limit и bounded read-only alert enrichment.

Manager diagnostic sessions связан с отдельным manual admin workflow, agent endpoints и узким automation coordinator-ом. Он не является writer-ом availability или speedtest workflow: текущий код ничего не меняет в status/history/incidents/retries/Remnawave/speedtest. Реализованы alternative endpoint и automatic trigger только для неразрешённого speedtest country fallback; multi-agent session и availability-trigger пока не реализованы.

## Основная идея

Удалённые probe-agent'ы нужны Xray Checker как дополнительные точки выполнения диагностической проверки из других сетей, регионов и условий доступа.

Распределено только место выполнения проверки. Принятие operational-решений не распределяется: основной controller остаётся единственным источником status и единственным владельцем существующих monitoring workflows.

Результат агента является диагностическим свидетельством, а не состоянием ноды. Он не получает quorum-вес, не объединяется в общий authoritative status и не даёт агенту права влиять на систему.

## Целевое назначение

Remote Diagnostics должны помогать оператору понять причину неоднозначного локального результата.

Основной сценарий:

1. Локальная availability-проверка ноды получает `proxy_failure` или другую ошибку, причины которой недостаточно для уверенного вывода.
2. Оператор либо controller создаёт отдельную диагностическую сессию.
3. Один или несколько агентов повторяют сопоставимую проверку той же ноды из других сетевых условий.
4. Controller показывает локальный и удалённые результаты рядом и формирует только вероятностные подсказки для анализа.
5. Никакой remote result не изменяет status, history, incidents, решение об отправке Telegram alert или Remnawave; допускается только вероятностная строка в уже разрешённом alert.

Пример: локальная проверка ноды «Германия» завершилась `proxy_failure`. Если агент из другой сети получает ту же ошибку на той же стадии, становится менее вероятна проблема только локального ISP или маршрута controller-а. Если у агента всё работает, подозрение смещается в сторону локальной сети, региональной маршрутизации, ISP или DPI. Это сужает область поиска, но само по себе не доказывает конкретную причину.

## Решаемые задачи

### Проверка из других условий

Оператор получает возможность повторить проблему из другой страны, сети, ASN или hosting provider-а без ручного развёртывания полноценного второго Xray Checker.

### Отделение локальной проблемы от общей

Сравнение помогает различать:

- проблему ISP, маршрута, DNS или DPI на стороне основного controller-а;
- региональную блокировку или деградацию;
- общую ошибку Xray-конфигурации либо handshake;
- проблему сервера, порта, firewall или сети хостинга;
- отказ используемого test endpoint;
- неисправность самого probe-agent'а.

### Воспроизводимость

Диагностическая сессия фиксирует, какой `StableID`, generation, test profile и endpoint проверялись. Это позволяет сравнивать результаты, полученные для одной и той же effective-конфигурации, а не похожие проверки разных нод.

### Снижение ручной работы

Controller может автоматически создать диагностическую сессию после подходящего локального события. Оператору не требуется каждый раз самостоятельно выбирать сервер, копировать конфигурацию и собирать несопоставимые логи.

## Жёсткий инвариант изоляции

Независимо от числа агентов, совпадения ошибок и уверенности диагностической подсказки данные Remote Diagnostics не должны:

- менять локальное состояние `online`, `proxy_failure`, `offline` или `unknown`;
- записываться в общую Availability history;
- менять downtime, `DownSince`, `ProxyFailureSince` или cumulative statistics;
- открывать, изменять или закрывать node/mass incidents;
- участвовать в массовой корреляции failure codes;
- менять dashboard KPI или основные Prometheus availability metrics;
- запускать, подавлять или закрывать Telegram alerts, reminders и retries; observation может только обогатить текст уже разрешённого локальными правилами alert;
- запускать speedtest либо отменять speed-confirmation retry;
- создавать, изменять, подавлять или удалять Remnawave `announce`;
- влиять на Remnawave confirmation counters, location state или recovery hysteresis;
- менять subscription refresh, node merge, maintenance или backup/restore state;
- использоваться как основание для автоматической смены operational status в будущем без отдельного изменения архитектуры и явного решения владельца проекта.

Даже если все зарегистрированные агенты возвращают одинаковый результат, controller может написать только «ошибка воспроизведена в нескольких условиях». Он не превращает это наблюдение в `global_offline`, `global_proxy_failure` или другой общий status.

## Термины

- **Controller** — основной Xray Checker, который создаёт диагностические сессии, выдаёт задания и показывает результаты.
- **Local result** — результат существующего authoritative availability workflow controller-а.
- **Probe-agent** — удалённый ограниченный worker, который исполняет диагностическое задание controller-а.
- **Diagnostic session** — изолированный запуск диагностики одного `StableID` с локальным snapshot и remote observations.
- **Observation** — подписанный результат одного агента в рамках конкретной diagnostic session.
- **Diagnostic summary** — человекочитаемое вероятностное сравнение observations; не operational status.
- **Network condition** — metadata среды агента: region, provider/ASN или заданная оператором network group.
- **Generation** — версия effective-конфигурации, к которой привязана diagnostic session.

## Поток выполнения

```text
 Authoritative local workflow
 availability or speedtest fallback
                 |
                 v
   local result and operational decision
                 |
        manual or eligible opt-in auto
                 |
                 v
      DiagnosticSessionManager
                 |
       +---------+---------+
       |                   |
       v                   v
 Probe-agent A       Probe-agent B (future auto)
       |                   |
       +---------+---------+
                 |
         signed observations
                 |
                 v
 Admin UI / copy of an already allowed alert
                 |
                 X
      no operational state writes
```

Диагностика запускается асинхронно и не задерживает локальный availability/speedtest result или постановку confirmation retry. Incidents и Remnawave продолжают работать только по существующим локальным правилам. Уже разрешённый фоновый speed alert может bounded-время ждать observation исключительно для дополнения текста; timeout или любой agent outcome не отменяет отправку.

## Diagnostic session

Предлагаемая минимальная модель:

```text
SessionID
StableID
Trigger
ConfigGeneration
ConfigFingerprint
LocalResultSnapshot
RequestedAgents
AgentObservations
CreatedAt
ExpiresAt
State
```

### Trigger

Допустимые причины создания:

- `manual` — оператор явно нажал `Diagnose`;
- `auto_proxy_failure` — controller увидел переход локальной ноды в `proxy_failure`;
- `auto_check_endpoint` — controller хочет проверить вероятную проблему test endpoint;
- `auto_ambiguous_failure` — локальной диагностики недостаточно для уверенной классификации.
- `auto_speed_fallback` — speedtest действительно выполнил country fallback, но резервы исчерпались technical error либо финальная скорость осталась ниже threshold.

Автоматический trigger только создаёт diagnostic session. Он не меняет исходный local result и не задерживает его публикацию.

### State

Минимальные состояния сессии:

- `requested`;
- `dispatching`;
- `running`;
- `completed`;
- `partial`;
- `expired`;
- `cancelled`.

Завершение, expiration или cancellation сессии не вызывают recovery и не меняют operational state ноды.

### Deduplication и ограничения

Чтобы нестабильная нода не создавала бесконечную очередь:

- одновременно разрешена одна активная auto-session на `StableID + trigger`;
- повторный локальный результат прикрепляется к существующей сессии либо игнорируется до её завершения;
- после auto-session действует configurable cooldown; отказ, при котором session так и не стартовала — нет свободного healthy-агента либо исчерпан concurrency limit — cooldown не занимает и повторяется на следующем прогоне;
- manual session может обходить cooldown, но остаётся под global/per-agent concurrency limit;
- session имеет deadline и не ждёт недоступного агента бесконечно;
- controller ограничивает число нод и агентов в одном запуске.

## Функционал controller-а

Controller должен:

1. Регистрировать, включать, отключать и отзывать probe-agent'ов.
2. Хранить `AgentID`, display name, network condition, public key, capabilities, version и last seen.
3. Создавать manual и automatic diagnostic sessions.
4. Выбирать агента по требуемым условиям, а не по quorum-весу.
5. Формировать задания только для active `StableID` текущей effective-конфигурации.
6. Привязывать задание к `SessionID`, `JobID`, generation, config fingerprint и сроку действия.
7. Передавать минимальную конфигурацию, необходимую для выполнения проверки.
8. Проверять подпись, schema version, `SessionID`, `JobID`, nonce, generation и timestamp observation.
9. Отклонять replay, stale, duplicate и неизвестные результаты.
10. Хранить результат только внутри diagnostic subsystem.
11. Сравнивать local result и observations без изменения authoritative state.
12. Показывать вероятностные подсказки и исходные доказательства оператору.
13. Сохранять прежнее поведение проекта, когда Remote Diagnostics отключены или агенты недоступны.

Controller не должен передавать DiagnosticSessionManager ссылки или callbacks, позволяющие менять nodearchive, incidents, Telegram, speedtest, Remnawave либо subscription lifecycle.

## Функционал probe-agent'а

Probe-agent должен:

1. Сам инициировать защищённое соединение с controller-ом, чтобы не требовать публичного inbound-порта.
2. Получать только ограниченные диагностические задания controller-а.
3. Проверять срок действия задания и допустимость test profile.
4. Материализовать минимальную временную Xray-конфигурацию для назначенного `StableID`.
5. Выполнять тот же базовый proxy-check: `ip`, `status` или `download`.
6. После proxy failure выполнять TCP/ping diagnostics по тем же правилам, не используя ping как самостоятельное доказательство offline.
7. Выполнять direct connectivity control probe собственной сети.
8. При `check_endpoint` запускать разрешённый альтернативный endpoint profile.
9. Возвращать структурированную observation с точным failure code и стадией сбоя.
10. Не принимать произвольные команды, shell scripts или невалидированные URL.
11. Удалять временную конфигурацию после задания.
12. Не сохранять subscription URL, proxy credentials или полную конфигурацию ноды.
13. Ограничивать CPU, память, bandwidth, concurrency и максимальную длительность задания.
14. Отправлять heartbeat с версией, capabilities, временем и health status.

## Observation protocol

Минимальная observation содержит:

- `AgentID`;
- `SessionID`;
- `JobID` и одноразовый nonce;
- generation и config fingerprint;
- `StableID`;
- `CheckedAt`, duration и endpoint profile;
- результат `online`, `proxy_failure` или `offline` только внутри observation;
- latency и failure code;
- TCP/ping diagnostics;
- direct connectivity result;
- alternative endpoint result, если выполнялся;
- agent/schema version;
- криптографическую подпись всего payload.

Слова `online`, `proxy_failure` и `offline` внутри observation описывают только результат конкретного агента. Они не являются состоянием ноды в controller-е.

Transport authentication не заменяет anti-replay. Controller проверяет nonce, временное окно и повторное использование `JobID`, даже если соединение защищено mTLS.

Observation старой generation не применяется к новой session. Её можно показать как rejected/stale evidence в техническом журнале, но нельзя включать в diagnostic summary.

## Diagnostic summary

Controller может автоматически сформировать подсказку, но обязан показывать её как вероятностную интерпретацию.

Примеры:

| Local result | Remote observation | Возможная интерпретация |
| --- | --- | --- |
| `proxy_failure`, remote `online` | Проверка работает из другой сети | Вероятна локальная проблема ISP, маршрута, DNS или DPI. |
| Одинаковый TLS/handshake failure | Ошибка воспроизведена в нескольких условиях | Вероятна общая проблема конфигурации, сервера или фильтрации, действующей в этих сетях. |
| Proxy и TCP недоступны локально и удалённо | Ошибка воспроизведена | Возможна проблема сервера, порта, firewall либо сети хостинга. |
| Основной endpoint не работает, альтернативный работает | Нода выполняет другой proxy-check | Вероятна проблема test endpoint, а не самой ноды. |
| У агента сломана direct connectivity | Remote result недостоверен | Сначала требуется восстановить agent/network health. |
| Results расходятся без устойчивого паттерна | Недостаточно данных | Нужна дополнительная точка или ручной анализ. |

Diagnostic summary не должен использовать формулировки «причина доказана», «глобальный outage» или «нода восстановлена». Допустимые формулировки: «вероятно», «ошибка воспроизведена», «не воспроизведена», «данных недостаточно».

## Хранение данных

### Первая версия

Для первой версии рекомендуется in-memory storage:

- ограниченное количество последних sessions;
- TTL, например 24 часа;
- потеря незавершённых sessions после restart допустима;
- оператор может скопировать или скачать JSON отдельной session;
- данные не входят в backup.

Это позволяет проверить полезность функции без новой persisted schema и риска случайно связать diagnostics с основной history.

### Возможное отдельное хранение позже

Если потребуется история расследований, она должна храниться в отдельном versioned файле, например `diagnostic_sessions.json`, с собственным retention.

Этот файл:

- не является частью `node_registry.json` или `speedtest_results.json`;
- не участвует в Availability/Speedtest history и KPI;
- не переносится node merge автоматически без отдельного решения;
- не влияет на incidents и alerts;
- получает migration/normalization tests;
- по умолчанию не входит в backup до отдельного security-дизайна.

## Maintenance и retired-ноды

- Automatic diagnostics для maintenance-ноды не запускаются.
- Явная admin-команда `Diagnose` для maintenance-ноды разрешена как изолированный probe и не снимает maintenance.
- Результат помечается `maintenanceDiagnostic` и остаётся внутри session.
- После `Resume` diagnostic result не выставляет local `online`.
- Для retired `StableID` новые diagnostic sessions не создаются.
- Refresh, удаливший ноду из active set, отменяет её незавершённые задания либо помечает observations stale.

## Admin UI и API

В раскрытой карточке ноды нужна отдельная секция `Diagnostics`, а не новый вариант Availability-графика.

Минимальный UI:

- кнопка `Diagnose`;
- выбор одного или нескольких агентов/условий;
- причина automatic trigger;
- local result snapshot;
- таблица observations по агентам;
- direct connectivity и alternative endpoint results;
- diagnostic summary;
- status session и deadline;
- действия cancel, retry и download JSON.

Automatic session может показываться badge-ом `Diagnostics running`, но не меняет цвет и status основной карточки.

Agent protocol и Admin API должны быть разделены. Admin endpoints остаются под обязательной auth; agent endpoint использует отдельную взаимную аутентификацию. При реализации все новые API-схемы добавляются в `web/openapi.yaml`.

Публичный dashboard, `/api/v1/public/proxies` и `/config/<StableID>` не получают agent observations или diagnostic summaries.

## Метрики

Допустимы отдельные operational metrics подсистемы:

- agent up/last seen;
- количество diagnostic jobs;
- duration и result доставки задания;
- rejected, stale и replay observations;
- active/expired sessions;
- controller-side queue/concurrency.

Agent observations не записываются в `xray_proxy_status`, `xray_proxy_latency_ms`, Availability KPI или другие authoritative node metrics.

Raw credentials, subscription URL и произвольный error text запрещены в metric labels.

## Безопасность и модель доверия

Полноценный Xray proxy-check требует UUID/password, TLS/Reality и transport settings. Поэтому probe-agent является доверенным инфраструктурным компонентом, а не публичным volunteer worker-ом.

Минимальные требования:

1. У каждого агента отдельная отзываемая identity.
2. Enrollment использует одноразовый короткоживущий token.
3. Канал защищён mTLS либо эквивалентной взаимной аутентификацией.
4. Observation подписывается отдельным ключом агента; предпочтительна асимметричная подпись, чтобы controller хранил только public key.
5. `SessionID`, `JobID`, nonce, generation и временное окно защищают от replay и stale writes.
6. Agent работает от отдельного непривилегированного OS user-а.
7. Полученная конфигурация используется только в памяти или защищённом временном каталоге.
8. Secrets не попадают в logs, metrics, session JSON, API responses или backup.
9. Xray metrics/debug endpoints, если используются, слушают только loopback агента.
10. Revoked agent не получает новые задания, а его незавершённые results отклоняются.

## Что намеренно не реализуется

На текущем этапе отсутствуют и не планируются как скрытая часть первой версии:

- quorum;
- `FederatedAssessment`;
- общий federated status ноды;
- состояния `global_offline`, `global_proxy_failure` и `regional_degradation` как operational state;
- distributed incident correlation;
- remote results в Availability history;
- самостоятельные Telegram notifications и решения об отправке/подавлении по agent observations;
- изменение или подавление Remnawave `announce`;
- distributed speedtest;
- балансировка пользовательского трафика;
- автоматическая замена нод или Xray-конфигурации;
- произвольное удалённое выполнение команд;
- ML/AI root-cause verdicts.

Metadata network condition используется только для выбора подходящего агента и объяснения результата, а не для голосования.

## Этапы внедрения

### Этап 0. Изолированный каркас

- Создать `DiagnosticSessionManager` без write-зависимостей на monitoring subsystems.
- Определить versioned session/job/observation schemas.
- Добавить generation/config fingerprint и stale/replay rejection.
- Зафиксировать тестами отсутствие operational side effects.

### Этап 1. Manual diagnostics с одним агентом

- Реализовать enrollment, heartbeat и защищённый job protocol.
- Добавить кнопку `Diagnose` и сравнительный результат.
- Хранить sessions только в памяти.
- Не менять local status, history, incidents, Telegram и Remnawave.

### Этап 2. Automatic controller trigger (частично реализован)

- Реализован opt-in `auto_speed_fallback` после исчерпанных резервов или низкой fallback-скорости.
- Реализованы per-`StableID` deduplication/cooldown, общий concurrency limit и bounded alert wait.
- Реализованы direct connectivity и alternative endpoint probes; automatic download использует status как alternative.
- Operational retry/alert decision выполняется до ожидания агента; automatic session остаётся полностью изолированной.
- Opt-in запуск при переходе availability в `proxy_failure` остаётся отдельным следующим расширением.

### Этап 3. Несколько агентов и улучшение подсказок

- Выбирать агентов из разных network conditions.
- Показывать результаты рядом без quorum и общего status.
- Расширить diagnostic summary вероятностными интерпретациями.
- При необходимости добавить отдельный retention-bound diagnostic storage после отдельного решения.

## Критерии готовности

Функция готова к использованию, когда автоматически проверено:

1. Manual и automatic trigger создают отдельную diagnostic session.
2. Automatic trigger не задерживает и не меняет локальный availability/speedtest result или постановку retry.
3. Observation никогда не вызывает запись в nodearchive или Availability history.
4. Завершение session не открывает и не закрывает incident.
5. Ни success, ни failure агента не создают и не подавляют Telegram notification; они могут только дополнить уже разрешённый speed alert.
6. Agent result не запускает Remnawave reconcile и не меняет `announce`.
7. Agent result не создаёт и не отменяет speedtest/retry.
8. Один или несколько одинаковых remote results остаются observations, а не общим status.
9. Direct connectivity failure помечает observation недостоверной.
10. Invalid signature, replay, stale timestamp, неизвестный `JobID` и старая generation отклоняются.
11. Maintenance manual diagnostic остаётся изолированной, automatic trigger пропускается.
12. Retired `StableID` не принимает новые задания.
13. Secrets отсутствуют в logs, metrics, API, session export и backup.
14. При выключенной подсистеме все существующие workflows работают без изменений.

## Открытые решения следующих этапов

- Нужно ли заменять короткий подписанный polling на long-held request либо stream после проверки реальной нагрузки.
- Нужна ли дополнительная controller-подпись job поверх pinned TLS и identity-authenticated poll.
- Следует ли заменить передаваемый single-node Xray JSON на отдельную минимальную versioned execution schema.
- Какие local failure codes включают automatic trigger кроме `proxy_failure`.
- Набор direct connectivity и alternative endpoints.
- Нужна ли отдельная persisted diagnostic history после проверки первой версии.
- Как долго UI показывает completed/expired sessions.

До принятия этих решений реализация остаётся in-memory и opt-in. Любое будущее предложение использовать agent observations в operational workflows является отдельным изменением архитектуры, а не естественным расширением Remote Diagnostics.
