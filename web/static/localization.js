(function () {
  "use strict";

  const storageKey = "xray-checker-language";
  const supportedLanguages = new Set(["en", "ru"]);
  const ru = {
    "Total": "Всего", "Online": "Online", "Offline": "Offline", "Maintenance": "Обслуживание", "Avg Latency": "Ср. задержка",
    "Servers": "Серверы", "Search...": "Поиск...", "All": "Все", "Default": "Default",
    "Name": "Имя", "Status": "Статус", "Copy URL": "Копировать URL",
    "Metrics": "Метрики", "Health": "Health", "Docs": "Docs",
    "Latency ↑": "Задержка ↑", "Latency ↓": "Задержка ↓",
    "No proxies match your criteria": "Нет прокси, соответствующих условиям", "of": "из",
    "proxies · Config URLs return 200/503 for monitoring": "прокси · Config URL возвращают 200/503 для мониторинга",
    "Check:": "Check:", "Interval:": "Интервал:", "Timeout:": "Timeout:",
    "Powered by": "Работает на базе", "Made with ❤️ by": "Сделано с ❤️ —",
    "Telegram Chat": "Telegram-чат", "Auto": "Авто", "Copied!": "Скопировано!", "IP / server copied!": "IP / сервер скопирован!", "Logo": "Логотип",

    "Xray Checker Admin": "Админка Xray Checker",
    "Availability, speed tests and operational controls": "Availability, speedtest и управление",
    "Waiting for data": "Ожидание данных", "Last updated: n/a": "Последнее обновление: n/a",
    "Idle": "Idle", "Refresh": "Обновить", "Admin sections": "Разделы администрирования",
    "Dashboard": "Dashboard", "Nodes Overview": "Nodes Overview", "Incidents": "Инциденты",
    "Speed History": "Speed History", "Controls": "Управление", "Actions and settings": "Действия и настройки",
    "Dashboard controls": "Настройки dashboard", "Actions": "Действия", "Filters": "Фильтры",
    "Schedule": "Расписание", "Backup": "Backup", "Subscription": "Subscription",
    "Update Subscription": "Update Subscription", "Node Test URL": "Node Test URL",
    "Select one node": "Выберите одну ноду", "Selected node URL": "URL выбранной ноды",
    "Save Node URL": "Сохранить URL ноды", "Clear": "Очистить", "Run Check": "Run Check",
    "Name, server, subscription": "Имя, сервер, подписка", "Select Visible": "Выбрать видимые",
    "Save Schedule": "Сохранить расписание", "Reports": "Отчёты",
    "Sensitive Telegram values are read from environment variables only.": "Чувствительные значения Telegram читаются только из переменных окружения.",
    "Bot token": "Bot token", "Chat": "Чат", "Topic": "Топик", "Admins": "Администраторы",
    "Not set": "Не задано", "Availability alerts": "Availability-алерты", "Speed reports": "Speedtest-отчёты",
    "Report mode": "Режим отчёта", "Report rows": "Строк в отчёте", "Save Telegram": "Сохранить Telegram",
    "Bot commands": "Команды бота", "Automatic Backups": "Автоматические бэкапы",
    "Download Backup": "Скачать бэкап", "Restore on Restart": "Восстановить при перезапуске",
    "One archive is created per UTC day in": "Один архив в сутки по UTC создаётся в",
    ". Archives older than 7 days and files beyond the newest 7 are removed automatically.": ". Архивы старше 7 дней и файлы сверх 7 последних удаляются автоматически.",
    "Download persisted settings, node statistics and speed-test history as a ZIP archive. Environment variables, generated Xray configuration, geo files and Telegram credentials are excluded.": "Скачать сохранённые настройки, статистику нод и историю speedtest в ZIP-архиве. Переменные окружения, сгенерированный Xray config, geo-файлы и Telegram credentials в бэкап не входят.",
    "Upload an Xray Checker backup. It will be verified and applied on the next application restart. Files missing from the archive will be removed during restoration.": "Загрузить бэкап Xray Checker. Он пройдёт валидацию и будет применён при следующем restart приложения. Файлы, которых нет в архиве, будут удалены во время restore.",
    "Overview": "Обзор", "Servers online": "Серверов онлайн", "Avg speed": "Средняя скорость",
    "Failures": "Сбои", "Nodes": "Ноды", "Select all visible nodes": "Выбрать все видимые ноды",
    "Check": "Check", "Run": "Run", "Resume": "Возобновить", "Saving…": "Сохранение…", "Mute Selected": "Mute выбранных",
    "Unmute Selected": "Снять mute с выбранных", "Only issues": "Только проблемы", "Sort": "Сортировка",
    "Search": "Поиск", "No nodes match filters": "Нет нод, соответствующих фильтрам",
    "Protocol": "Протокол", "Country": "Страна", "Availability": "Доступность",
    "Down since": "Offline с", "Failure": "Сбой", "Measured speed": "Измеренная скорость",
    "Dates": "Даты", "From": "От", "To": "До", "Average": "Среднее", "Minimum": "Минимум",
    "Maximum": "Максимум", "Measurements": "Замеры", "Normal speed": "Нормальная скорость",
    "Below configured threshold": "Ниже заданного порога", "Error or offline": "Ошибка или офлайн",
    "Open full history": "Открыть полную историю", "Last speed test": "Последний speedtest",
    "No result": "Нет результата", "No speed test": "Нет speedtest", "No active downtime": "Нет активного downtime",
    "No current failure": "Нет текущего сбоя", "Diagnostics unavailable": "Диагностика недоступна", "Monitoring paused": "Мониторинг приостановлен",
    "Refresh Overview": "Обновить обзор", "Refresh Geo": "Обновить Geo",
    "Name, server, country, subscription": "Имя, сервер, страна, подписка", "IP / Server": "IP / Сервер", "Copy IP / server": "Копировать IP / сервер", "Open IP details": "Открыть сведения об IP",
    "Geo match": "Совпадение Geo", "Last seen": "Последняя активность", "Action": "Действие",
    "Active": "Активна", "Retired": "Выведена", "Delete": "Удалить", "Merge": "Merge",
    "Active nodes are managed by subscription": "Active-ноды управляются подпиской",
    "Merge applied": "Merge применён", "No nodes found": "Ноды не найдены", "Match": "Совпадает",
    "Partial": "Частично", "Conflict": "Конфликт", "Mismatch": "Не совпадает", "Unknown": "Неизвестно",
    "Blacklisted": "В blacklist", "Geo data not loaded": "Geo-данные не загружены",
    "Refresh Incidents": "Обновить инциденты",
    "Persisted node failures and correlated mass outages": "Сохранённые сбои нод и связанные массовые отказы",
    "Scope": "Scope", "Affected": "Затронуто", "Cause": "Причина", "Started": "Начало",
    "Duration": "Длительность", "Resolved": "Закрыт", "No incidents recorded": "Инциденты не зафиксированы",
    "Refresh History": "Обновить историю", "Node": "Нода", "Speed Stats": "Статистика speedtest",
    "successful results": "успешных результатов", "best result": "лучший результат",
    "slowest result": "самый медленный результат", "stored measurements": "сохранённых замеров",
    "Checked": "Время проверки", "Speed": "Скорость", "Downloaded": "Скачано", "Source": "Источник",
    "Error": "Ошибка", "No speed test results yet": "Результатов speedtest пока нет",
    "Select a node": "Выберите ноду", "No speed history for this node": "Для этой ноды нет speedtest history",
    "Loading": "Загрузка", "Alerts": "Алерты", "All Telegram": "Все Telegram-уведомления", "Always": "Всегда",
    "Concurrency": "Concurrency", "Timeout sec": "Таймаут, сек", "Max MB": "Макс. MB",
    "Low Mbps": "Low-speed threshold, Mbps", "History retention days": "Хранение истории, дней",
    "Interval minutes": "Интервал, мин", "Max reminder min": "Макс. интервал напоминаний, мин",
    "Reminder schedule min": "Расписание напоминаний, мин", "Group offline reminders": "Группировать напоминания об offline",
    "Node down alerts": "Node-down алерты", "Recovery alerts": "Recovery-алерты",
    "Mute": "Mute", "Unmute": "Снять mute", "Test": "Тест", "Dismiss": "Скрыть",
    "Results": "Результаты", "No nodes": "Нет нод", "No results": "Нет результатов",
    "no results": "нет результатов", "latest successful checks": "последних успешных проверок",
    "Failed checks": "Неуспешные проверки", "Max speed": "Макс. скорость", "Min speed": "Мин. скорость",
    "Avg": "Сред.", "Max": "Макс.", "Min": "Мин.", "Reason": "Причина",
    "Muted": "Mute", "Restore": "Restore", "Choose Backup ZIP": "Выбрать ZIP-бэкап",
    "Speed Test": "Speedtest", "Loading speed-test history…": "Загрузка speedtest history…",
    "No speed tests in this period": "За выбранный период нет результатов speedtest",

    "Automatic announce": "Автоматический announce", "Remnawave subscription announce": "Subscription announce в Remnawave",
    "Audience-aware outage messages in the subscription response header": "Сообщения о недоступности с учётом audience в response header подписки",
    "Sync topology & reconcile": "Синхронизировать topology и запустить reconcile", "Save settings": "Сохранить настройки",
    "Environment gate": "Environment gate", "Topology": "Topology", "Last reconcile": "Последний reconcile",
    "Enabled": "Включено", "Disabled": "Отключено", "Configured": "Настроено", "Not loaded": "Не загружено",
    "Never": "Никогда", "Policy": "Policy", "Outage minutes": "Outage, мин",
    "Full failed checks": "Неуспешных full checks", "Stable recovery minutes": "Stable recovery, мин",
    "Availability check min": "Availability check, мин", "Diagnostics refresh min": "Diagnostics refresh, мин",
    "Both the duration and full-check confirmation threshold must be reached.": "Нужно выполнить оба условия: достигнуть минимальной длительности и набрать порог подтверждений full checks.",
    "Message constructor": "Конструктор сообщений",
    "Enable the user-impact states that should keep a checker status line in the subscription announce.": "Включите влияющие на пользователей состояния, при которых status line от checker должна оставаться в subscription announce.",
    "One unavailable location": "Недоступна одна локация", "Several unavailable locations": "Недоступно несколько локаций",
    "All audience locations unavailable": "Недоступны все локации audience",
    "Part of one location unavailable": "Недоступна часть одной локации",
    "Part of several locations unavailable": "Недоступны части нескольких локаций", "Everything stable": "Всё стабильно",
    "Template": "Шаблон", "Preview": "Preview", "Long location-list fallback": "Fallback для длинного списка локаций",
    "Long partial-location-list fallback": "Fallback для длинного списка частично недоступных локаций",
    "Used when more than three whole locations are down or the rendered location list exceeds 240 characters.": "Используется, когда полностью недоступно больше трёх локаций или сформированный список превышает 240 символов.",
    "Used when parts of more than three locations are down or the rendered partial-location list exceeds 240 characters.": "Используется, когда частично недоступно больше трёх локаций или сформированный список превышает 240 символов.",
    "Audience pairs": "Пары audience", "Internal Squad determines visible Hosts; External Squad receives the message.": "Internal Squad определяет видимые Hosts, а External Squad получает сообщение.",
    "Add pair": "Добавить пару", "Mode": "Режим",
    "Mark the checker service pair as monitoring-only. It remains visible for topology checks but its External Squad is never modified.": "Пометьте сервисную пару checker как monitoring-only. Она останется видимой для topology checks, но checker никогда не изменит её External Squad.",
    "StableID to Host mapping": "Маппинг StableID → Host",
    "Host names are display-only. The persisted identity is StableID → Remnawave Host UUID.": "Имена Hosts нужны только для отображения. В persisted state идентичность хранится как StableID → UUID Remnawave Host.",
    "Checker node": "Нода checker", "Redundancy group": "Redundancy group", "Public label": "Публичная метка",
    "StableIDs with the same group key are one public location: a healthy member prevents a full outage, while a confirmed failed member uses the partial-availability scenario.": "StableID с одинаковым group key считаются одной публичной локацией: healthy member предотвращает full outage, а confirmed failed member переводит её в partial-availability scenario.",
    "Current announce state": "Текущее состояние announce",
    "Only a status shown as managed may later be changed or removed automatically; the preserved first line remains operator-owned.": "Checker может автоматически изменить или удалить только status line с пометкой managed; сохранённая первая строка остаётся operator-owned.",
    "Remove": "Удалить", "No audience pairs configured": "Пары audience не настроены", "Not mapped": "Нет mapping",
    "Unnamed": "Без имени", "No subscription": "Без подписки", "Defaults to Host UUID": "По умолчанию Host UUID",
    "Defaults to Host remark": "По умолчанию — Host remark", "No active checker nodes": "Нет active-нод checker",
    "No announce headers are currently tracked.": "Сейчас checker не отслеживает ни один announce header.",
    "Token stays in": "Token остаётся в", ". Required token scopes:": ". Нужные token scopes:",
    "Reconcile runs after a confirmed outage, a change in affected locations, a partial/total transition, stable recovery, or saved settings. Manual checks and the fast recovery loop do not count as outage confirmations by themselves. A confirmed failed member of an otherwise healthy redundancy group uses the partial-availability scenarios. Unknown state and probable": "Reconcile запускается после подтверждённого outage, изменения affected locations, перехода partial/total, stable recovery или сохранения настроек. Manual checks и fast recovery loop сами по себе не считаются подтверждениями outage. Confirmed failed member в остальном healthy redundancy group использует partial-availability scenarios. Unknown state и вероятностные",
    "incidents do not create a new message.": "инциденты не создают новое сообщение.",
    "A maintenance node is evaluated by its probe result. A separate maintenance scenario is used only when every member of the location is both offline and in maintenance.": "Maintenance-нода оценивается по результату probe. Отдельный maintenance-сценарий используется только тогда, когда все участники локации одновременно offline и находятся на обслуживании.",
    "An existing single-line": "Существующий однострочный",
    "announce stays unchanged as the base; xray-checker appends only its status line and restores the original value after recovery. A multiline value can be reclaimed only when its last suffix exactly matches the rendered target or healthy scenario. If the healthy scenario is disabled, recovery restores the base or removes a checker-only header. Other or manually changed values are never overwritten. Templates are single-line, URL-free, and limited to 240 characters after rendering.": "announce остаётся неизменной base-строкой; xray-checker добавляет только свою status line и после recovery восстанавливает исходное значение. Multiline-значение можно взять под управление, только если его последний suffix точно совпадает с rendered target или healthy scenario. Если healthy scenario отключён, при recovery checker восстанавливает base или удаляет header, принадлежащий только ему. Другие значения, включая изменённые вручную, никогда не перезаписываются. Шаблоны должны быть однострочными, без URL и не длиннее 240 символов после render.",
    "Single-location placeholders": "Placeholders для одной локации",
    "Multiple-location placeholders": "Placeholders для нескольких локаций",
    "Total-outage placeholders": "Placeholders для total outage",
    "Partial single-location placeholders": "Placeholders для partial outage одной локации",
    "Partial multiple-location placeholders": "Placeholders для partial outage нескольких локаций",
    "One location under maintenance": "Одна локация на обслуживании",
    "Several locations under maintenance": "Несколько локаций на обслуживании",
    "Maintenance single-location placeholders": "Placeholders для обслуживания одной локации",
    "Maintenance multiple-location placeholders": "Placeholders для обслуживания нескольких локаций",
    "Healthy-state placeholders": "Placeholders для healthy state",
    "Fallback placeholders": "Placeholders для fallback",
    "Partial fallback placeholders": "Placeholders для partial fallback",
    "Long maintenance-location-list fallback": "Fallback для длинного списка обслуживаемых локаций",
    "Used when more than three locations are fully offline under maintenance or the rendered list is too long.": "Используется, когда больше трёх обслуживаемых локаций полностью offline или rendered-список слишком длинный.",
    "Maintenance fallback placeholders": "Placeholders для maintenance fallback",
    "Mixed outage and maintenance fallback": "Fallback для сочетания outage и обслуживания",
    "Used when the combined outage and maintenance messages would exceed 240 characters.": "Используется, когда объединённые сообщения outage и обслуживания превысили бы 240 символов.",
    "Mixed maintenance fallback placeholders": "Placeholders для смешанного maintenance fallback",
    "Empty template — the server will restore the default on save": "Пустой шаблон — сервер восстановит значение по умолчанию при сохранении",
    "Scenario disabled — the owned checker status line is cleared for this state": "Scenario отключён — checker очищает принадлежащую ему status line для этого состояния",
    "Select Internal Squad": "Выберите Internal Squad", "Select External Squad": "Выберите External Squad",
    "Ready": "Готово", "Configure environment variables and restart xray-checker": "Настройте переменные окружения и перезапустите xray-checker",
    "Managed status line": "Managed status line", "Managed": "Managed",
    "Base announce preserved": "Base announce сохранён", "Unmanaged": "Unmanaged",
    "Existing rwEncodeBase64 announce stays on the first line": "Существующий rwEncodeBase64 announce остаётся в первой строке",
    "Existing announce is left untouched": "Существующий announce не изменяется",
    "Managed value is no longer present remotely": "Managed-значение больше не найдено в remote state",
    "Loading Remnawave settings": "Загрузка настроек Remnawave", "Saving settings": "Сохранение настроек",
    "Remnawave settings saved; reconciliation queued": "Настройки Remnawave сохранены; reconcile поставлен в очередь",
    "Loading topology and reconciling announce headers": "Загружается topology, выполняется reconcile announce headers",
    "Topology and announce headers reconciled": "Reconcile для topology и announce headers завершён",

    "Merge retired node": "Merge retired-ноды",
    "Choose the active node that owns the merged history.": "Выберите active-ноду, которая получит объединённую историю.",
    "Close merge dialog": "Закрыть merge-диалог", "Retired source": "Retired source",
    "Active target": "Active target", "Select an active node": "Выберите active-ноду",
    "From retired node": "Из retired-ноды", "Into active node": "В active-ноду",
    "Speed results": "Результаты speedtest",
    "The merge is staged now and applied on the next restart. The retired record is removed only after all persisted state loads successfully.": "Merge будет подготовлен сейчас и применён после следующего restart. Retired-запись удалится только после успешной загрузки всего persisted state.",
    "Merge staged successfully": "Merge успешно подготовлен",
    "Restart the service to apply it. After restart, Nodes Overview will verify the target lineage and show a completed confirmation.": "Перезапустите сервис, чтобы применить merge. После restart раздел «Обзор нод» проверит target lineage и покажет подтверждение завершения.",
    "Changed identity fields": "Изменившиеся identity fields", "Identity check": "Проверка identity",
    "Name and port are unchanged.": "Name и port не изменились.", "Removed nodes:": "Удаляемые ноды:",
    "Cancel": "Отмена", "Review merge": "Проверить merge", "Back": "Назад", "Stage merge": "Подготовить merge", "Done": "Готово",

    "Timeout": "Таймаут", "TLS error": "Ошибка TLS", "Connection refused": "Соединение отклонено",
    "Connection reset": "Соединение сброшено", "DNS error": "Ошибка DNS", "No response": "Нет ответа",
    "Failed": "Ошибка", "Below threshold": "Ниже порога", "Successful": "Успешно",
    "Reserve Test URL": "Резервный Test URL", "All systems operational": "Все системы работают штатно", "All monitored systems operational": "Все отслеживаемые системы работают штатно",
    "Checking…": "Проверка…", "Close details": "Закрыть подробности", "Open details": "Открыть подробности",
    "Select exactly one node": "Выберите ровно одну ноду", "Unsaved changes": "Есть несохранённые изменения",
    "Custom URL active": "Используется пользовательский URL", "Using global Test URL": "Используется глобальный Test URL",
    "Updating": "Обновление", "Update blocked": "Обновление заблокировано",
    "Select at least one node": "Выберите хотя бы одну ноду", "Selected nodes are in maintenance": "Выбранные ноды находятся в режиме обслуживания", "Node availability checked": "Доступность ноды проверена",
    "Node Test URL saved": "Test URL ноды сохранён", "Node Test URL cleared": "Test URL ноды очищен",
    "Schedule saved": "Расписание сохранено", "Telegram settings saved": "Настройки Telegram сохранены",
    "Backup download started": "Скачивание бэкапа начато", "Verifying backup...": "Проверка бэкапа...",
    "Telegram test message sent": "Тестовое сообщение Telegram отправлено", "Enabling maintenance": "Включение режима обслуживания", "Resuming monitoring": "Возобновление мониторинга", "Maintenance enabled": "Режим обслуживания включён", "Monitoring resumed": "Мониторинг возобновлён",
    "due now": "уже пора", "Latest measurement:": "Последний замер:",
    "Choose both start and end dates": "Укажите обе даты: начало и конец",
    "Start date must be before or equal to end date": "Дата начала должна быть раньше даты окончания или совпадать с ней",
    "Speed-test history in Mbps": "История speedtest в Mbps", "Speed chart period": "Период графика скорости",
    "Speed:": "Скорость:", "Downloaded:": "Скачано:", "Result:": "Результат:",
    "Muted all": "Mute: всё", "Muted alerts": "Mute: алерты", "Muted speed": "Mute: speedtest",
    "Clear visible node selection": "Очистить выбор видимых нод",
    "No active nodes to refresh": "Нет активных нод для обновления", "Refreshing Geo": "Обновление Geo",
    "Node merge completed successfully": "Node merge успешно завершён", "Node merge is staged": "Node merge подготовлен",
    "Node merge status needs verification": "Статус node merge требует проверки", "Source node not found": "Source-нода не найдена",
    "Only a retired node can be merged": "Merge доступен только для retired-ноды",
    "No compatible active node found (subscription, protocol and server must match)": "Не найдена совместимая active-нода (subscription, protocol и server должны совпадать)",
    "Choose the active node that should own the retired history.": "Выберите active-ноду, которая получит history retired-ноды.",
    "Preparing preview": "Подготовка preview", "Review the exact source, target and merged counters before staging.": "Перед staging проверьте точные source, target и итоговые counters.",
    "Staging merge": "Подготовка merge", "The selected merge is safely staged.": "Выбранный merge безопасно подготовлен.",
    "Node merge staged; restart the application to apply it": "Node merge подготовлен; перезапустите приложение для применения",
    "Node not found": "Нода не найдена", "Active nodes are managed by subscription and cannot be deleted": "Active-ноды управляются подпиской, поэтому удалить их нельзя",
    "Deleting node": "Удаление ноды", "All subscriptions": "Все подписки", "No changes": "Без изменений",
    "Apply this update anyway?": "Всё равно применить это обновление?",
    "Restore this backup on the next application restart?\n\nCurrent persisted data will be replaced.": "Восстановить этот бэкап при следующем перезапуске приложения?\n\nТекущие сохранённые данные будут заменены.",
    "Switch language": "Переключить язык"
  };

  const patterns = [
    [/^Last updated: (.+)$/u, "Последнее обновление: $1"],
    [/^Check: (.+) · Interval: (.+) · Timeout: (.+)$/u, "Проверка: $1 · Интервал: $2 · Таймаут: $3"],
    [/^Running (\d+)\/(\d+)$/u, "Выполняется $1/$2"],
    [/^(\d+) server\(s\) unavailable$/u, "Недоступно серверов: $1"],
    [/^(\d+) maintenance$/u, "на обслуживании: $1"],
    [/^(\d+) maintenance node\(s\) skipped$/u, "Пропущено нод на обслуживании: $1"],
    [/^(\d+) nodes checked$/u, "Проверено нод: $1"],
    [/^Low speed < (.+ Mbps)$/u, "Низкая скорость < $1"],
    [/^Select (.+)$/u, "Выбрать $1"],
    [/^Remnawave Host for (.+)$/u, "Remnawave Host для $1"],
    [/^Geo updated: (\d+) nodes, failed: (\d+)$/u, "Geo обновлён: нод $1, ошибок $2"],
    [/^Updated (\d+) (.+)$/u, "Обновлено: $1 $2"],
    [/^No changes \((\d+) (.+)\)$/u, "Без изменений ($1 $2)"],
    [/^Backup verified \((\d+) files\)\. Restart the application to apply it\.$/u, "Бэкап проверен ($1 файлов). Перезапустите приложение для применения."],
    [/^(\d+) nodes$/u, "$1 нод"], [/^(\d+) results$/u, "$1 результатов"],
    [/^(\d+) selected$/u, "$1 выбрано"], [/^(\d+) incidents$/u, "Инцидентов: $1"],
    [/^(\d+) muted$/u, "Mute: $1"], [/^(\d+) users$/u, "$1 пользователей"],
    [/^(\d+) successful results$/u, "$1 успешных результатов"],
    [/^(\d+) latest results$/u, "Последних результатов: $1"],
    [/^Probe · (.+)$/u, "Probe · $1"],
    [/^(\d+) latest results · next test in <1 min$/u, "Последних результатов: $1 · следующий speedtest менее чем через минуту"],
    [/^(\d+) latest results · next test in (\d+) min$/u, "Последних результатов: $1 · следующий speedtest через $2 мин"],
    [/^(\d+) latest results · next test in (\d+) h (\d+) min$/u, "Последних результатов: $1 · следующий speedtest через $2 ч $3 мин"],
    [/^(\d+) latest results · next test in (\d+) d (\d+) h (\d+) min$/u, "Последних результатов: $1 · следующий speedtest через $2 д $3 ч $4 мин"],
    [/^(\d+) latest results · next test in (.+)$/u, "Последних результатов: $1 · следующий speedtest через $2"],
    [/^(\d+) latest results · next test due now$/u, "Последних результатов: $1 · следующий speedtest уже должен запуститься"],
    [/^(\d+) successful of (\d+) results$/u, "$1 успешных из $2 результатов"],
    [/^(\d+)% available · (\d+) offline · (\d+) muted$/u, "$1% доступно · $2 offline · $3 muted"],
    [/^(\d+)% available · (\d+) offline · (\d+) maintenance · (\d+) muted$/u, "$1% доступно · $2 offline · $3 на обслуживании · $4 muted"],
    [/^(\d+) shown · (\d+) total$/u, "$1 показано · $2 всего"],
    [/^(\d+) shown · (\d+) active · (\d+) retired$/u, "$1 показано · $2 активных · $3 выведенных"],
    [/^(\d+) shown · (\d+) monitored · (\d+) maintenance · (\d+) retired$/u, "$1 показано · $2 отслеживается · $3 на обслуживании · $4 выведено"],
    [/^(\d+) incidents · (\d+) active$/u, "Инцидентов: $1 · активных: $2"],
    [/^(\d+) total$/u, "Всего: $1"],
    [/^(<?)(\d+) min$/u, "$1$2 мин"],
    [/^(\d+) h (\d+) min$/u, "$1 ч $2 мин"],
    [/^(\d+) d (\d+) h (\d+) min$/u, "$1 д $2 ч $3 мин"],
    [/^in (.+)$/u, "через $1"],
    [/^Offline · <1 min$/u, "Офлайн · <1 мин"],
    [/^Offline · (\d+) min$/u, "Офлайн · $1 мин"],
    [/^No subscription( · .+)$/u, "Без подписки$1"],
    [/^Unknown Geo data not loaded$/u, "Неизвестно; Geo-данные не загружены"],
    [/^All (\d+) · Alerts (\d+) · Speed (\d+)$/u, "Все $1 · Алерты $2 · Speedtest $3"],
    [/^(TCP|Ping) failed$/u, "$1: ошибка"],
    [/^(TCP|Ping) No$/u, "$1: нет ответа"],
    [/^Error: (.+)$/u, "Ошибка: $1"],
    [/^(\d+) compatible active node\. Name and port may differ; compare the selected target in the next step\.$/u, "$1 совместимая active-нода. Name и port могут отличаться; на следующем шаге сравните выбранный target."],
    [/^(\d+) compatible active nodes\. Name and port may differ; compare the selected target in the next step\.$/u, "$1 совместимых active-нод. Name и port могут отличаться; на следующем шаге сравните выбранный target."],
    [/^(.+) → (.+)\. Persisted lineage and history are attached to (.+)\.$/u, "$1 → $2. Persisted lineage и history привязаны к $3."],
    [/^(.+) → (.+)\. Restart the service to apply it\.$/u, "$1 → $2. Перезапустите сервис, чтобы применить merge."],
    [/^(.+) → (.+)\. Refresh Nodes Overview or inspect startup logs\.$/u, "$1 → $2. Обновите «Обзор нод» или проверьте startup logs."],
    [/^(.+) → (.+)\. Restart required\.$/u, "$1 → $2. Требуется restart."],
    [/^Delete archived node "(.+)"\?\n\nThis removes the node registry entry and saved speed-test history\.$/u, "Удалить ноду «$1» из архива?\n\nБудут удалены запись node registry и сохранённая speedtest history."],
    [/^(.+)\n\nApply this update anyway\?$/u, "$1\n\nВсё равно применить это обновление?"]
  ];

  const translatedText = new WeakMap();
  const translatedAttributes = new WeakMap();
  let language = readLanguage();

  function readLanguage() {
    try {
      const stored = localStorage.getItem(storageKey);
      return supportedLanguages.has(stored) ? stored : "en";
    } catch (_) { return "en"; }
  }

  function translate(value) {
    if (language !== "ru" || !value) return value;
    if (Object.prototype.hasOwnProperty.call(ru, value)) return ru[value];
    const normalized = value.replace(/\s+/gu, " ").trim();
    if (Object.prototype.hasOwnProperty.call(ru, normalized)) return ru[normalized];
    for (const [pattern, replacement] of patterns) {
      if (pattern.test(value)) return value.replace(pattern, replacement);
      if (pattern.test(normalized)) return normalized.replace(pattern, replacement);
    }
    return value;
  }

  function translateTextNode(node, force) {
    const parent = node.parentElement;
    if (!parent || parent.closest("script, style, code, pre, textarea, [data-i18n-skip]")) return;
    const current = node.nodeValue;
    const previous = translatedText.get(node);
    const unchanged = previous && current === previous.output;
    if (!force && unchanged) return;
    const source = force && unchanged ? previous.source : current;
    const match = /^(\s*)(.*?)(\s*)$/su.exec(source);
    if (!match || !match[2]) return;
    const output = match[1] + translate(match[2]) + match[3];
    translatedText.set(node, { source, output });
    if (current !== output) node.nodeValue = output;
  }

  function translateElementAttributes(element, force) {
    if (element.closest("[data-i18n-skip]")) return;
    const attributes = ["aria-label", "placeholder", "title", "alt"];
    let cache = translatedAttributes.get(element);
    if (!cache) { cache = {}; translatedAttributes.set(element, cache); }
    for (const name of attributes) {
      if (!element.hasAttribute(name)) continue;
      const current = element.getAttribute(name);
      const previous = cache[name];
      const unchanged = previous && current === previous.output;
      if (!force && unchanged) continue;
      const source = force && unchanged ? previous.source : current;
      const output = translate(source);
      cache[name] = { source, output };
      if (current !== output) element.setAttribute(name, output);
    }
  }

  function apply(root, force) {
    if (!root) return;
    if (root.nodeType === Node.TEXT_NODE) { translateTextNode(root, force); return; }
    if (![Node.ELEMENT_NODE, Node.DOCUMENT_NODE, Node.DOCUMENT_FRAGMENT_NODE].includes(root.nodeType)) return;
    if (root.nodeType === Node.ELEMENT_NODE) translateElementAttributes(root, force);
    for (const element of root.querySelectorAll ? root.querySelectorAll("*") : []) translateElementAttributes(element, force);
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    let node;
    while ((node = walker.nextNode())) translateTextNode(node, force);
  }

  function updateControls() {
    document.documentElement.lang = language;
    for (const button of document.querySelectorAll("[data-language]")) {
      const active = button.dataset.language === language;
      button.classList.toggle("active", active);
      button.setAttribute("aria-pressed", String(active));
    }
  }

  function setLanguage(nextLanguage) {
    if (!supportedLanguages.has(nextLanguage) || nextLanguage === language) return;
    language = nextLanguage;
    try { localStorage.setItem(storageKey, language); } catch (_) {}
    updateControls();
    apply(document, true);
    document.dispatchEvent(new CustomEvent("xray-language-changed", { detail: { language } }));
  }

  function init() {
    updateControls();
    apply(document, false);
    document.addEventListener("click", (event) => {
      const button = event.target.closest("[data-language]");
      if (button) setLanguage(button.dataset.language);
    });
    const observer = new MutationObserver((mutations) => {
      for (const mutation of mutations) {
        if (mutation.type === "characterData") apply(mutation.target, false);
        if (mutation.type === "childList") for (const node of mutation.addedNodes) apply(node, false);
        if (mutation.type === "attributes") translateElementAttributes(mutation.target, false);
      }
    });
    observer.observe(document.documentElement, { subtree: true, childList: true, characterData: true, attributes: true,
      attributeFilter: ["aria-label", "placeholder", "title", "alt"] });
  }

  window.XrayI18n = { apply, getLanguage: () => language, setLanguage, translate };
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", init, { once: true });
  else init();
})();
