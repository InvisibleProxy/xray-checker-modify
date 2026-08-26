# AGENTS.md

Правила для агентов и разработчиков, меняющих этот репозиторий.

## Общение

- Общайтесь с владельцем проекта по-русски, неформально и прямо.
- Не соглашайтесь автоматически: указывайте на риски и ошибки с доказательствами из кода или тестов.
- Не выдавайте предположение за подтверждённое поведение.

## Источники истины

При конфликте используйте порядок:

1. текущий код и успешно выполняющиеся тесты;
2. `PROJECT_CONTEXT.md` — архитектура, workflow и бизнес-правила форка;
3. `README.md` — эксплуатация и пользовательские сценарии;
4. `web/openapi.yaml` — публичный API-контракт;
5. `docs/` и upstream-документация — справочная информация.

Не копируйте подробные описания между документами. Обновляйте файл, которому принадлежит изменившийся аспект.

## Критические инварианты

- Реальный git root — текущий каталог `xray-checker`, а не внешний каталог `xray-chacker`.
- `StableID`, а не `Name`, является ключом статусов, speedtest, node archive, mute и персонального Test URL.
- Не нормализуйте и не переименовывайте имена из подписки без отдельного требования. Суффиксы Remnawave вроде `^~2~^` принадлежат источнику.
- При refresh сначала вызывайте `xray.PreserveStableIDs`, затем `xray.ValidateStableIDs` и только потом сравнивайте effective-конфигурации.
- Пустой или дублирующийся без учёта регистра `StableID` должен останавливать startup/refresh до создания StableID-keyed maps и restart Xray.
- Ручное и плановое обновление подписки должны использовать один workflow и не выполняться параллельно.
- Пустой refresh или удаление минимум трёх и не менее 50% прежних нод считается подозрительным: scheduled update блокируется, manual требует explicit force, привязанный fingerprint к previewed candidate; candidate Xray config не должен уничтожать last-known-good при неудачном startup.
- Ручной и плановый speedtest должны применять одинаковую логику URL: per-node URL имеет приоритет над глобальным.
- Country fallback выбирается только по `ClaimedCountryCode` из node archive; GeoIP не участвует. Техническая ошибка либо скорость основного URL ниже текущего Telegram low-speed threshold переключает замер на fallback; нулевой threshold отключает переключение по скорости. Fallback не ниже threshold не создаёт автоматический Telegram report/alert. Медленный fallback ставится в обычный 30-минутный low-speed retry; исключение — результат ниже threshold после `context deadline exceeded` основного URL, который должен уведомляться сразу и использовать отдельный 5-минутный retry.
- Telegram-запуск speedtest использует сохранённый `ScheduleConfig`, а не нулевой `TestConfig`.
- Прямой результат Telegram-speedtest должен возвращаться в исходные chat ID и topic ID независимо от настроек фоновых speed-report; report target не персистится и не попадает в admin API.
- Первый фоновый low-speed результат (schedule/admin) сначала проверяется через country fallback и не уведомляет. Только если fallback тоже медленный либо доступные fallback URL завершаются ошибкой, просевшие `StableID` повторяются через 30 минут, а алерт отправляется лишь при повторной проблеме. `context deadline exceeded` в `Error` или `PrimaryError` запускает отдельный повтор затронутых `StableID` через 5 минут и не задерживает исходное уведомление; low-speed fallback после timeout и low-speed результат 5-минутного повтора уведомляются сразу, без новой 30-минутной задержки.
- Pending speed-test retry персистится с типом, due time, исходным TestConfig и `StableID`, восстанавливается после restart и очищается для исчезнувших нод. Legacy-запись без типа считается low-speed retry; low-speed и deadline retry дедуплицируются независимо.
- Полный availability-check всех нод не использует TCP-гейт. Быстрый recovery-loop проверяет только уже недоступные `StableID`: обновляет TCP/ping и запускает proxy-check лишь после `TCP OK`; ping никогда не является gate.
- Переход в offline и `DownSince` сохраняются до дополнительной TCP/ping-диагностики. Для непривилегированного ICMP `udp4`/`udp6` не сопоставляйте reply по Echo ID: Linux может переписать его; используйте тип ответа, sequence и уникальный payload.
- Быстрые recovery-checks не увеличивают Telegram `FailCount` и не сдвигают reminder schedule. Подтверждённый online передаётся immediate-recovery пути без ожидания `AlertCheckMinutes`.
- Ручная availability-проверка из admin UI/API и карточки ноды Telegram использует общий workflow; для online/unknown выполняется полный proxy-check, для offline — каскадная проверка.
- В admin UI действия `Check` и `Run` должны оставаться доступны одновременно в строке каждой ноды и в групповой панели выбранных нод. Клик по этим кнопкам или checkbox не должен срабатывать как раскрытие строки; раскрывается только неинтерактивная область шапки либо отдельная стрелка. Master-checkbox в заголовке `Nodes` выбирает только текущие видимые `StableID` и обязан корректно отражать checked/indeterminate/disabled после фильтрации и изменения выбора.
- Повторный render admin dashboard при неизменном порядке видимых `StableID` должен обновлять карточки на месте, не заменяя весь список через `innerHTML`. Несколько карточек могут быть раскрыты одновременно; их history/range/request-state независимы, а polling и выбор строк не должны сбрасывать раскрытие или перезапускать графики.
- Speedtest удерживает Xray lifecycle read-lock от выбора нод до завершения теста; restart при refresh выполняется только под write-lock.
- Speedtest history хранится по возрасту; default 60 дней. Не возвращайте count-based limit.
- График speedtest в admin UI строится только по сохранённым замерам Mbps и не интерполирует пропуски. Пунктирный gap bridge и error-зона являются только визуальными индикаторами отсутствующих результатов и не участвуют в статистике. Success percentage выбранного периода использует все history results как знаменатель; успешен только завершённый замер с Mbps не ниже low-speed threshold, а low-speed/offline/error считаются неуспешными. Диапазоны history API используют `from` включительно и `to` не включительно; обе границы передаются в RFC3339.
- Downtime в node archive накопительный и не должен очищаться вместе со speedtest history.
- GeoCheck выполняется только для active-нод. Retired StableID должен игнорироваться и при явном запросе через admin API.
- Incident journal хранит node и mass records; массовая корреляция требует одинакового failure code, минимум 3 и минимум 50% active scope. `check_endpoint` всегда маркируется как вероятностный вывод.
- Remnawave announce mapping всегда строится по `StableID → Host UUID`; имя checker-ноды и Host remark остаются только display-полями. Несколько StableID могут ссылаться на один Host, а redundancy нескольких Hosts задаётся явным `GroupKey`. GroupKey агрегируется только внутри уже отфильтрованной audience: одинаковый ключ разных тарифов не делает их Hosts резервами друг друга.
- Host не принадлежит External Squad напрямую. Audience определяется через Host inbound, membership inbound во внутреннем скваде, `excludedInternalSquads` и явную persisted пару Internal → External. Disabled/hidden Hosts исключаются; сервисная пара checker обязана быть `MonitoringOnly` и не может стать target.
- Новый Remnawave announce требует и минимального downtime, и нескольких полных availability-итераций. Manual check и быстрый recovery-loop не увеличивают confirmation counter; active массовый `check_endpoint` не создаёт новый пользовательский announce. Redundancy group с healthy и подтверждённо offline members имеет отдельное partial-состояние; full outage имеет приоритет. Улучшения `down → partial` и `partial/down → healthy` требуют отдельного стабильного окна и не публикуют специального recovery-сообщения.
- Remnawave write сначала перечитывает External Squads и case-insensitively merge-ит только `announce` в полной карте `responseHeadersAdd`. Существующий однострочный `rwEncodeBase64:` можно принять как opaque base: status добавляется через `\n`, а после очистки base восстанавливается байт-в-байт. Многострочное значение разрешено принять только при точном совпадении его suffix с rendered target/healthy message; preceding base сохраняется точно. Не принимайте под управление plain/многострочное неизвестное значение и не меняйте remote value, не совпадающее с persisted ownership state. Runtime v1/v2 мигрирует без получения новых прав на неизвестный remote header.
- Remnawave user messages строятся только из versioned scenario templates, а не строковых литералов в reconcile workflow. Разрешайте лишь контекстные `{location}`, `{locations}`, `{unavailable}`, `{affected}`, `{total}`; unknown braces, URL, `{{...}}`, line breaks и output длиннее 240 runes отклоняются. Disabled scenario очищает только exact-owned suffix. Config v0-v2 получает default rules, legacy `NormalMessage` мигрирует в healthy scenario; v1 API save без `messages` и v2 save без partial rules не должны сбрасывать сохранённые templates.
- Remnawave API token читается только из env и не попадает в persisted config, admin API, логи или backup. Минимальные scopes: `hosts:list`, `internal-squads:list`, `external-squads:list`, `external-squads:update`; последний scope всё равно даёт endpoint-wide write access.
- Failure code строится из proxy-check и прямых TCP/ping diagnostics; отсутствие ICMP reply не является самостоятельным доказательством offline.
- Активные ноды управляются подпиской; удалять вручную можно только retired-записи.
- Node merge разрешён только retired source → выбранный администратором active target после preview и совпадения нормализованных `SubName`, protocol и server. Name и port могут измениться, но preview обязан явно показать расхождения; confirmation token должен быть привязан к конкретному persisted candidate, а UI не должен выбирать target автоматически.
- Node merge применяется только на startup к `node_registry.json` и `speedtest_results.json`. До успешного `Load` speedtest, node archive и Telegram исходные файлы сохраняются для byte-for-byte rollback; incident ID не меняются, source StableID re-keyed, а lineage прежних ID не теряется.
- Node merge не переносит Remnawave `StableID → Host UUID`; после merge mapping active target меняется оператором вручную. Не расширяйте merge-транзакцию на Remnawave config без отдельного дизайна rollback и проверки аудитории.
- Backup restore и node merge не должны одновременно находиться в pending/applied/rollback state; их web staging обязан удерживать общий transaction gate до публикации операции.
- Backup не должен включать environment, geo-файлы, `xray_config.json`, Telegram secrets/admin IDs, Remnawave API token и `remnawave_announce_state.json`. Versioned `remnawave_announce_config.json` с non-secret policy/mappings входит в typed backup.
- `Caddyfile.example` является источником шаблона reverse proxy; локальный `Caddyfile` игнорируется и не должен попадать в коммиты.
- Автоматические backup: максимум один за UTC-день, максимум 7 файлов и максимум 7 суток.
- Restore всегда проходит типизированную JSON-валидацию и staging; применение — только на следующем startup с rollback. Commit допустим лишь после успешного `Load` у всех владельцев restored state.
- Restore transaction должна различать оборванное применение, неподтверждённое состояние и оборванную очистку подтверждённого commit; не удаляйте rollback-копию без commit marker.
- `/admin` и `/api/v1/admin/*` не должны становиться публичными.
- Server-mode не запускается с пустыми Basic Auth credentials; встроенные публичные credentials запрещены.
- Основной Telegram output использует Rich Messages и компактный HTML fallback. На неопределённой сетевой ошибке нельзя отправлять fallback повторно из-за риска дубля.
- Интерактивные Telegram-экраны «Проблемы» и «Замеры» и кнопки speedtest history показывают только результаты active `StableID`; retired history остаётся в persisted state и админке.
- Recovery alert удаляется из persisted state только после успешной отправки соответствующего уведомления.
- `xray_config.json` — генерируемый runtime-файл, не источник истины и не часть backup.

## Правила изменений

- Перед правкой просмотрите `git status` и не затрагивайте несвязанные пользовательские изменения.
- Для поиска используйте `rg`/`rg --files`.
- Для редактирования файлов используйте patch, а не перезапись shell-хаками.
- Не выполняйте `git reset --hard`, `git checkout --`, recursive delete или другие разрушительные команды без явного запроса.
- Не коммитьте secrets, `.env`, runtime `data/`, `geo/`, `xray_config.json`, логи и backup ZIP.
- Новая persisted-схема должна иметь безопасную нормализацию старых файлов и тест миграции.
- Новые map по нодам должны использовать `StableID`; отдельно учитывайте возможные ID-коллизии.
- Изменение persisted Remnawave policy/mappings требует migration/normalization test; ownership runtime не переносится через backup/restore между инсталляциями.
- После заметной UI-правки требуется desktop browser check, а не только парсинг шаблона. Mobile viewport и отдельная mobile browser QA для админки не требуются и не выполняются.
- GitHub Actions и Dependabot в этом форке отключены. Не добавляйте `.github/workflows/` и `.github/dependabot.yml`; обязательные проверки выполняются локально перед push.

## Обязательные проверки

Минимум для Go-изменений:

```powershell
$goFiles = Get-ChildItem -Recurse -Filter *.go | Select-Object -ExpandProperty FullName
gofmt -w $goFiles
go test ./...
go vet ./...
```

Для конкурентного кода в `checker`, `speedtest`, `telegram`, `backup` или `web` дополнительно:

```powershell
go test -race ./backup ./checker ./nodemerge ./speedtest ./telegram ./web
```

Если host Go отсутствует, используйте Docker:

```powershell
docker build --target builder --build-arg ENABLE_UPX=false -t xray-checker-builder .
docker run --rm xray-checker-builder go test ./...
docker run --rm xray-checker-builder go vet ./...
docker build --build-arg ENABLE_UPX=false -t xray-checker:check .
```

Дополнительные проверки по области:

- subscription/identity: тесты `xray` и `nodemerge`, сценарии неоднозначных имён, stale confirmation, suspicious diff, interrupted apply/commit и rollback rejected candidate;
- speedtest: manual и scheduled paths, per-node URL, retention и persistence;
- backup/restore: manifest, hash, дубликаты JSON-ключей, typed schema, interrupted apply/commit, path traversal, missing files, rollback, secrets и rotation;
- Telegram: HTML/Rich HTML escaping, структура summary/details, fallback policy, сохранённый TestConfig, persisted low-speed/deadline retries, mass incident grouping, mute scopes, alert lifecycle и отсутствие токенов в выводе;
- Remnawave: API paths/scopes, bearer secrecy, topology exclusions, Internal → External audience pairs, StableID mapping, redundancy groups, full-check confirmation, `check_endpoint` suppression, recovery hysteresis, scenario enable/disable, placeholder validation/rendering/fallback, config migration, header merge/ownership conflicts и отсутствие token/raw inbound в admin snapshot;
- API: обновить `web/openapi.yaml`, проверить все локальные `$ref` и handler tests;
- UI: только desktop viewport, filters, selection, admin auth и отсутствие ошибок в console; mobile browser check не запускать;
- Dockerfile/Compose: builder build и финальный production image.

## Документация при изменениях

- `README.md` обновляется при изменении установки, запуска, env, URL или пользовательского сценария.
- `PROJECT_CONTEXT.md` обновляется при изменении архитектуры, workflow, persisted state, бизнес-правила или ограничения.
- `AGENTS.md` обновляется при появлении нового инварианта, обязательной проверки или запрета.
- `web/openapi.yaml` обновляется вместе с API.
- `docs/` меняется только для сайта upstream/многоязычного руководства; не дублируйте туда внутренний контекст форка без отдельного решения.

Изменение архитектуры, workflow или интерфейса не считается завершённым, пока соответствующая документация не синхронизирована с кодом.
