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
- Ручной и плановый speedtest должны применять одинаковую логику URL: per-node URL имеет приоритет над глобальным.
- Telegram-запуск speedtest использует сохранённый `ScheduleConfig`, а не нулевой `TestConfig`.
- Прямой результат Telegram-speedtest должен возвращаться в исходные chat ID и topic ID независимо от настроек фоновых speed-report; report target не персистится и не попадает в admin API.
- Первый фоновый low-speed результат (schedule/admin) не уведомляет: повторяются только просевшие `StableID` через 30 минут, а алерт отправляется лишь при повторной проблеме. Первичные offline/error не задерживаются.
- Полный availability-check всех нод не использует TCP-гейт. Быстрый recovery-loop проверяет только уже недоступные `StableID`: обновляет TCP/ping и запускает proxy-check лишь после `TCP OK`; ping никогда не является gate.
- Быстрые recovery-checks не увеличивают Telegram `FailCount` и не сдвигают reminder schedule. Подтверждённый online передаётся immediate-recovery пути без ожидания `AlertCheckMinutes`.
- Ручная availability-проверка из admin UI/API и карточки ноды Telegram использует общий workflow; для online/unknown выполняется полный proxy-check, для offline — каскадная проверка.
- Speedtest удерживает Xray lifecycle read-lock от выбора нод до завершения теста; restart при refresh выполняется только под write-lock.
- Speedtest history хранится по возрасту; default 60 дней. Не возвращайте count-based limit.
- Downtime в node archive накопительный и не должен очищаться вместе со speedtest history.
- Активные ноды управляются подпиской; удалять вручную можно только retired-записи.
- Backup не должен включать environment, geo-файлы, `xray_config.json` и Telegram secrets/admin IDs.
- Автоматические backup: максимум один за UTC-день, максимум 7 файлов и максимум 7 суток.
- Restore всегда проходит типизированную JSON-валидацию и staging; применение — только на следующем startup с rollback. Commit допустим лишь после успешного `Load` у всех владельцев restored state.
- Restore transaction должна различать оборванное применение, неподтверждённое состояние и оборванную очистку подтверждённого commit; не удаляйте rollback-копию без commit marker.
- `/admin` и `/api/v1/admin/*` не должны становиться публичными.
- Server-mode не запускается с пустыми Basic Auth credentials; встроенные публичные credentials запрещены.
- Основной Telegram output использует Rich Messages и компактный HTML fallback. На неопределённой сетевой ошибке нельзя отправлять fallback повторно из-за риска дубля.
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
- После заметной UI-правки требуется desktop browser check, а не только парсинг шаблона. Mobile viewport и отдельная mobile browser QA для админки не требуются и не выполняются.

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
go test -race ./backup ./checker ./speedtest ./telegram ./web
```

Если host Go отсутствует, используйте Docker:

```powershell
docker build --target builder --build-arg ENABLE_UPX=false -t xray-checker-builder .
docker run --rm xray-checker-builder go test ./...
docker run --rm xray-checker-builder go vet ./...
docker build --build-arg ENABLE_UPX=false -t xray-checker:check .
```

Дополнительные проверки по области:

- subscription/identity: тесты `xray` и сценарии неоднозначных имён;
- speedtest: manual и scheduled paths, per-node URL, retention и persistence;
- backup/restore: manifest, hash, дубликаты JSON-ключей, typed schema, interrupted apply/commit, path traversal, missing files, rollback, secrets и rotation;
- Telegram: HTML/Rich HTML escaping, структура summary/details, fallback policy, сохранённый TestConfig, mute scopes, alert lifecycle и отсутствие токенов в выводе;
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
