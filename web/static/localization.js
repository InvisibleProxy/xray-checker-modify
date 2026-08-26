(function () {
  "use strict";

  const storageKey = "xray-checker-language";
  const supportedLanguages = new Set(["en", "ru"]);
  const ru = {
    "Total": "Всего", "Online": "Онлайн", "Offline": "Офлайн", "Avg Latency": "Средняя задержка",
    "Servers": "Серверы", "Search...": "Поиск...", "All": "Все", "Default": "По умолчанию",
    "Name": "Имя", "Status": "Статус", "Copy URL": "Копировать URL",
    "Metrics": "Метрики", "Health": "Состояние", "Docs": "Документация",
    "Latency ↑": "Задержка ↑", "Latency ↓": "Задержка ↓",
    "No proxies match your criteria": "Нет прокси, соответствующих условиям", "of": "из",
    "proxies · Config URLs return 200/503 for monitoring": "прокси · Config URL возвращают 200/503 для мониторинга",
    "Check:": "Проверка:", "Interval:": "Интервал:", "Timeout:": "Таймаут:",
    "Powered by": "Работает на базе", "Made with ❤️ by": "Сделано с ❤️ —",
    "Telegram Chat": "Telegram-чат", "Auto": "Авто", "Copied!": "Скопировано!", "Logo": "Логотип",

    "Xray Checker Admin": "Администрирование Xray Checker",
    "Availability, speed tests and operational controls": "Доступность, speed tests и оперативное управление",
    "Waiting for data": "Ожидание данных", "Last updated: n/a": "Последнее обновление: n/a",
    "Idle": "Ожидание", "Refresh": "Обновить", "Admin sections": "Разделы администрирования",
    "Dashboard": "Панель", "Nodes Overview": "Обзор нод", "Incidents": "Инциденты",
    "Speed History": "История скорости", "Controls": "Управление", "Actions and settings": "Действия и настройки",
    "Dashboard controls": "Управление панелью", "Actions": "Действия", "Filters": "Фильтры",
    "Schedule": "Расписание", "Backup": "Бэкап", "Subscription": "Подписка",
    "Update Subscription": "Обновить подписку", "Node Test URL": "Test URL ноды",
    "Select one node": "Выберите одну ноду", "Selected node URL": "URL выбранной ноды",
    "Save Node URL": "Сохранить URL ноды", "Clear": "Очистить", "Run Check": "Запустить проверку",
    "Name, server, subscription": "Имя, сервер, подписка", "Select Visible": "Выбрать видимые",
    "Save Schedule": "Сохранить расписание", "Reports": "Отчёты",
    "Sensitive Telegram values are read from environment variables only.": "Чувствительные значения Telegram читаются только из переменных окружения.",
    "Bot token": "Токен бота", "Chat": "Чат", "Topic": "Топик", "Admins": "Администраторы",
    "Not set": "Не задано", "Availability alerts": "Алерты доступности", "Speed reports": "Отчёты о скорости",
    "Report mode": "Режим отчёта", "Report rows": "Строк в отчёте", "Save Telegram": "Сохранить Telegram",
    "Bot commands": "Команды бота", "Automatic Backups": "Автоматические бэкапы",
    "Download Backup": "Скачать бэкап", "Restore on Restart": "Восстановить при перезапуске",
    "One archive is created per UTC day in": "Один архив в сутки по UTC создаётся в",
    ". Archives older than 7 days and files beyond the newest 7 are removed automatically.": ". Архивы старше 7 дней и файлы сверх 7 последних удаляются автоматически.",
    "Download persisted settings, node statistics and speed-test history as a ZIP archive. Environment variables, generated Xray configuration, geo files and Telegram credentials are excluded.": "Скачать сохранённые настройки, статистику нод и историю speed-test в ZIP-архиве. Переменные окружения, сгенерированная конфигурация Xray, geo-файлы и учётные данные Telegram не включаются.",
    "Upload an Xray Checker backup. It will be verified and applied on the next application restart. Files missing from the archive will be removed during restoration.": "Загрузить бэкап Xray Checker. Он будет проверен и применён при следующем перезапуске приложения. Отсутствующие в архиве файлы будут удалены при восстановлении.",
    "Overview": "Обзор", "Servers online": "Серверов онлайн", "Avg speed": "Средняя скорость",
    "Failures": "Сбои", "Nodes": "Ноды", "Select all visible nodes": "Выбрать все видимые ноды",
    "Check": "Проверить", "Run": "Запустить", "Mute Selected": "Заглушить выбранные",
    "Unmute Selected": "Снять mute с выбранных", "Only issues": "Только проблемы", "Sort": "Сортировка",
    "Search": "Поиск", "No nodes match filters": "Нет нод, соответствующих фильтрам",
    "Protocol": "Протокол", "Country": "Страна", "Availability": "Доступность",
    "Down since": "Недоступна с", "Failure": "Сбой", "Measured speed": "Измеренная скорость",
    "Dates": "Даты", "From": "От", "To": "До", "Average": "Среднее", "Minimum": "Минимум",
    "Maximum": "Максимум", "Measurements": "Замеры", "Normal speed": "Нормальная скорость",
    "Below configured threshold": "Ниже заданного порога", "Error or offline": "Ошибка или офлайн",
    "Open full history": "Открыть полную историю", "Last speed test": "Последний speed test",
    "No result": "Нет результата", "No speed test": "Нет speed test", "No active downtime": "Нет активного downtime",
    "No current failure": "Нет текущего сбоя", "Diagnostics unavailable": "Диагностика недоступна",
    "Refresh Overview": "Обновить обзор", "Refresh Geo": "Обновить Geo",
    "Name, server, country, subscription": "Имя, сервер, страна, подписка", "IP / Server": "IP / Сервер",
    "Geo match": "Совпадение Geo", "Last seen": "Последняя активность", "Action": "Действие",
    "Active": "Активна", "Retired": "Выведена", "Delete": "Удалить", "Merge": "Объединить",
    "Active nodes are managed by subscription": "Активные ноды управляются подпиской",
    "Merge applied": "Merge применён", "No nodes found": "Ноды не найдены", "Match": "Совпадает",
    "Partial": "Частично", "Conflict": "Конфликт", "Mismatch": "Не совпадает", "Unknown": "Неизвестно",
    "Blacklisted": "В blacklist", "Geo data not loaded": "Geo-данные не загружены",
    "Refresh Incidents": "Обновить инциденты",
    "Persisted node failures and correlated mass outages": "Сохранённые сбои нод и связанные массовые отказы",
    "Scope": "Область", "Affected": "Затронуто", "Cause": "Причина", "Started": "Начало",
    "Duration": "Длительность", "Resolved": "Закрыт", "No incidents recorded": "Инциденты не зафиксированы",
    "Refresh History": "Обновить историю", "Node": "Нода", "Speed Stats": "Статистика скорости",
    "successful results": "успешных результатов", "best result": "лучший результат",
    "slowest result": "самый медленный результат", "stored measurements": "сохранённых замеров",
    "Checked": "Время проверки", "Speed": "Скорость", "Downloaded": "Скачано", "Source": "Источник",
    "Error": "Ошибка", "No speed test results yet": "Результатов speed test пока нет",
    "Select a node": "Выберите ноду", "No speed history for this node": "Для этой ноды нет истории скорости",
    "Loading": "Загрузка", "Alerts": "Алерты", "All Telegram": "Весь Telegram", "Always": "Всегда",
    "Concurrency": "Параллельность", "Timeout sec": "Таймаут, сек", "Max MB": "Макс. MB",
    "Low Mbps": "Низкая скорость, Mbps", "History retention days": "Хранение истории, дней",
    "Interval minutes": "Интервал, мин", "Max reminder min": "Макс. интервал напоминаний, мин",
    "Reminder schedule min": "Расписание напоминаний, мин", "Group offline reminders": "Группировать напоминания об offline",
    "Node down alerts": "Алерты о недоступности нод", "Recovery alerts": "Алерты о восстановлении",
    "Mute": "Заглушить", "Unmute": "Снять mute", "Test": "Тест", "Dismiss": "Скрыть",
    "Results": "Результаты", "No nodes": "Нет нод", "No results": "Нет результатов",
    "no results": "нет результатов", "latest successful checks": "последних успешных проверок",
    "Failed checks": "Неуспешные проверки", "Max speed": "Макс. скорость", "Min speed": "Мин. скорость",
    "Avg": "Сред.", "Max": "Макс.", "Min": "Мин.", "Reason": "Причина",
    "Muted": "Заглушено", "Restore": "Восстановление", "Choose Backup ZIP": "Выбрать ZIP-бэкап",

    "Automatic announce": "Автоматический announce", "Remnawave subscription announce": "Subscription announce Remnawave",
    "Audience-aware outage messages in the subscription response header": "Сообщения об отказах с учётом audience в response header подписки",
    "Sync topology & reconcile": "Синхронизировать topology и выполнить reconcile", "Save settings": "Сохранить настройки",
    "Environment gate": "Проверка окружения", "Topology": "Topology", "Last reconcile": "Последний reconcile",
    "Enabled": "Включено", "Disabled": "Отключено", "Configured": "Настроено", "Not loaded": "Не загружено",
    "Never": "Никогда", "Policy": "Политика", "Outage minutes": "Минут отказа",
    "Full failed checks": "Полных неуспешных проверок", "Stable recovery minutes": "Минут стабильного восстановления",
    "Availability check min": "Проверка доступности, мин", "Diagnostics refresh min": "Обновление диагностики, мин",
    "Both the duration and full-check confirmation threshold must be reached.": "Должны быть достигнуты и длительность, и порог подтверждений полными проверками.",
    "Message constructor": "Конструктор сообщений",
    "Enable the user-impact states that should keep a checker status line in the subscription announce.": "Включите влияющие на пользователей состояния, при которых status line checker должна оставаться в subscription announce.",
    "One unavailable location": "Недоступна одна локация", "Several unavailable locations": "Недоступно несколько локаций",
    "All audience locations unavailable": "Недоступны все локации audience",
    "Part of one location unavailable": "Недоступна часть одной локации",
    "Part of several locations unavailable": "Недоступны части нескольких локаций", "Everything stable": "Всё стабильно",
    "Template": "Шаблон", "Preview": "Предпросмотр", "Long location-list fallback": "Fallback для длинного списка локаций",
    "Long partial-location-list fallback": "Fallback для длинного списка частично недоступных локаций",
    "Used when more than three whole locations are down or the rendered location list exceeds 240 characters.": "Используется, когда полностью недоступно больше трёх локаций или сформированный список превышает 240 символов.",
    "Used when parts of more than three locations are down or the rendered partial-location list exceeds 240 characters.": "Используется, когда частично недоступно больше трёх локаций или сформированный список превышает 240 символов.",
    "Audience pairs": "Пары audience", "Internal Squad determines visible Hosts; External Squad receives the message.": "Internal Squad определяет видимые Hosts; External Squad получает сообщение.",
    "Add pair": "Добавить пару", "Mode": "Режим",
    "Mark the checker service pair as monitoring-only. It remains visible for topology checks but its External Squad is never modified.": "Пометьте сервисную пару checker как monitoring-only. Она остаётся видимой для проверки topology, но её External Squad никогда не изменяется.",
    "StableID to Host mapping": "Сопоставление StableID с Host",
    "Host names are display-only. The persisted identity is StableID → Remnawave Host UUID.": "Имена Host используются только для отображения. Сохраняемая идентичность — StableID → Remnawave Host UUID.",
    "Checker node": "Нода checker", "Redundancy group": "Группа резервирования", "Public label": "Публичная метка",
    "StableIDs with the same group key are one public location: a healthy member prevents a full outage, while a confirmed failed member uses the partial-availability scenario.": "StableID с одинаковым group key образуют одну публичную локацию: исправный участник предотвращает полный отказ, а подтверждённо отказавший использует сценарий частичной доступности.",
    "Current announce state": "Текущее состояние announce",
    "Only a status shown as managed may later be changed or removed automatically; the preserved first line remains operator-owned.": "Автоматически изменяться или удаляться может только статус, помеченный как managed; сохранённая первая строка остаётся под управлением оператора.",
    "Remove": "Удалить", "No audience pairs configured": "Пары audience не настроены", "Not mapped": "Не сопоставлено",
    "Unnamed": "Без имени", "No subscription": "Без подписки", "Defaults to Host UUID": "По умолчанию Host UUID",
    "Defaults to Host remark": "По умолчанию remark Host", "No active checker nodes": "Нет активных нод checker",
    "No announce headers are currently tracked.": "Сейчас ни один announce header не отслеживается.",
    "Token stays in": "Token хранится в", ". Required token scopes:": ". Требуемые scopes token:",
    "Reconcile runs after a confirmed outage, a change in affected locations, a partial/total transition, stable recovery, or saved settings. Manual checks and the fast recovery loop do not count as outage confirmations by themselves. A confirmed failed member of an otherwise healthy redundancy group uses the partial-availability scenarios. Unknown state and probable": "Reconcile запускается после подтверждённого отказа, изменения затронутых локаций, перехода partial/total, стабильного восстановления или сохранения настроек. Ручные проверки и быстрый recovery loop сами по себе не считаются подтверждением отказа. Подтверждённо отказавший участник иначе исправной redundancy group использует сценарии частичной доступности. Unknown state и вероятностные",
    "incidents do not create a new message.": "инциденты не создают новое сообщение.",
    "An existing single-line": "Существующий однострочный",
    "announce stays unchanged as the base; xray-checker appends only its status line and restores the original value after recovery. A multiline value can be reclaimed only when its last suffix exactly matches the rendered target or healthy scenario. If the healthy scenario is disabled, recovery restores the base or removes a checker-only header. Other or manually changed values are never overwritten. Templates are single-line, URL-free, and limited to 240 characters after rendering.": "announce остаётся неизменной базой; xray-checker добавляет только свою status line и восстанавливает исходное значение после recovery. Многострочное значение можно принять под управление только при точном совпадении последнего suffix со сформированным target или healthy scenario. Если healthy scenario отключён, recovery восстанавливает базу или удаляет header, принадлежащий только checker. Другие или изменённые вручную значения никогда не перезаписываются. Шаблоны должны быть однострочными, без URL и не длиннее 240 символов после формирования.",
    "Single-location placeholders": "Placeholders для одной локации",
    "Multiple-location placeholders": "Placeholders для нескольких локаций",
    "Total-outage placeholders": "Placeholders полного отказа",
    "Partial single-location placeholders": "Placeholders частичного отказа одной локации",
    "Partial multiple-location placeholders": "Placeholders частичного отказа нескольких локаций",
    "Healthy-state placeholders": "Placeholders исправного состояния",
    "Fallback placeholders": "Fallback placeholders",
    "Partial fallback placeholders": "Partial fallback placeholders",
    "Empty template — the server will restore the default on save": "Пустой шаблон — сервер восстановит значение по умолчанию при сохранении",
    "Scenario disabled — the owned checker status line is cleared for this state": "Сценарий отключён — принадлежащая checker status line очищается для этого состояния",
    "Select Internal Squad": "Выберите Internal Squad", "Select External Squad": "Выберите External Squad",
    "Ready": "Готово", "Configure environment variables and restart xray-checker": "Настройте переменные окружения и перезапустите xray-checker",
    "Managed status line": "Управляемая status line", "Managed": "Управляется",
    "Base announce preserved": "Базовый announce сохранён", "Unmanaged": "Не управляется",
    "Existing rwEncodeBase64 announce stays on the first line": "Существующий rwEncodeBase64 announce остаётся в первой строке",
    "Existing announce is left untouched": "Существующий announce не изменяется",
    "Managed value is no longer present remotely": "Управляемого значения больше нет на удалённой стороне",
    "Loading Remnawave settings": "Загрузка настроек Remnawave", "Saving settings": "Сохранение настроек",
    "Remnawave settings saved; reconciliation queued": "Настройки Remnawave сохранены; reconciliation поставлен в очередь",
    "Loading topology and reconciling announce headers": "Загрузка topology и reconcile announce headers",
    "Topology and announce headers reconciled": "Topology и announce headers согласованы",

    "Merge retired node": "Объединить выведенную ноду",
    "Choose the active node that owns the merged history.": "Выберите активную ноду, которой будет принадлежать объединённая история.",
    "Close merge dialog": "Закрыть диалог merge", "Retired source": "Выведенный источник",
    "Active target": "Активная цель", "Select an active node": "Выберите активную ноду",
    "From retired node": "Из выведенной ноды", "Into active node": "В активную ноду",
    "Speed results": "Результаты скорости",
    "The merge is staged now and applied on the next restart. The retired record is removed only after all persisted state loads successfully.": "Merge будет подготовлен сейчас и применён при следующем перезапуске. Запись выведенной ноды удаляется только после успешной загрузки всего сохранённого состояния.",
    "Merge staged successfully": "Merge успешно подготовлен",
    "Restart the service to apply it. After restart, Nodes Overview will verify the target lineage and show a completed confirmation.": "Перезапустите сервис для применения. После перезапуска обзор нод проверит lineage цели и покажет подтверждение завершения.",
    "Cancel": "Отмена", "Review merge": "Проверить merge", "Back": "Назад", "Stage merge": "Подготовить merge", "Done": "Готово",

    "Timeout": "Таймаут", "TLS error": "Ошибка TLS", "Connection refused": "Соединение отклонено",
    "Connection reset": "Соединение сброшено", "DNS error": "Ошибка DNS", "No response": "Нет ответа",
    "Failed": "Ошибка", "Below threshold": "Ниже порога", "Successful": "Успешно",
    "Reserve Test URL": "Резервный Test URL", "All systems operational": "Все системы работают штатно",
    "Checking…": "Проверка…", "Close details": "Закрыть подробности", "Open details": "Открыть подробности",
    "Select exactly one node": "Выберите ровно одну ноду", "Unsaved changes": "Есть несохранённые изменения",
    "Custom URL active": "Используется пользовательский URL", "Using global Test URL": "Используется глобальный Test URL",
    "Updating": "Обновление", "Update blocked": "Обновление заблокировано",
    "Select at least one node": "Выберите хотя бы одну ноду", "Node availability checked": "Доступность ноды проверена",
    "Node Test URL saved": "Test URL ноды сохранён", "Node Test URL cleared": "Test URL ноды очищен",
    "Schedule saved": "Расписание сохранено", "Telegram settings saved": "Настройки Telegram сохранены",
    "Backup download started": "Скачивание бэкапа начато", "Verifying backup...": "Проверка бэкапа...",
    "Telegram test message sent": "Тестовое сообщение Telegram отправлено",
    "due now": "уже пора", "Latest measurement:": "Последний замер:",
    "Choose both start and end dates": "Укажите обе даты: начало и конец",
    "Start date must be before or equal to end date": "Дата начала должна быть раньше даты окончания или совпадать с ней",
    "Speed-test history in Mbps": "История speed-test в Mbps", "Speed chart period": "Период графика скорости",
    "Speed:": "Скорость:", "Downloaded:": "Скачано:", "Result:": "Результат:",
    "Muted all": "Заглушено всё", "Muted alerts": "Заглушены алерты", "Muted speed": "Заглушена скорость",
    "Clear visible node selection": "Очистить выбор видимых нод",
    "No active nodes to refresh": "Нет активных нод для обновления", "Refreshing Geo": "Обновление Geo",
    "Node merge completed successfully": "Merge нод успешно завершён", "Node merge is staged": "Merge нод подготовлен",
    "Node merge status needs verification": "Статус merge нод требует проверки", "Source node not found": "Исходная нода не найдена",
    "Only a retired node can be merged": "Объединить можно только выведенную ноду",
    "No compatible active node found (subscription, protocol and server must match)": "Не найдена совместимая активная нода (должны совпадать подписка, протокол и сервер)",
    "Choose the active node that should own the retired history.": "Выберите активную ноду, которой должна принадлежать история выведенной ноды.",
    "Preparing preview": "Подготовка предпросмотра", "Review the exact source, target and merged counters before staging.": "Перед подготовкой проверьте точные source, target и объединённые счётчики.",
    "Staging merge": "Подготовка merge", "The selected merge is safely staged.": "Выбранный merge безопасно подготовлен.",
    "Node merge staged; restart the application to apply it": "Merge нод подготовлен; перезапустите приложение для применения",
    "Node not found": "Нода не найдена", "Active nodes are managed by subscription and cannot be deleted": "Активные ноды управляются подпиской и не могут быть удалены",
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
    [/^Low speed < (.+ Mbps)$/u, "Низкая скорость < $1"],
    [/^Select (.+)$/u, "Выбрать $1"],
    [/^Remnawave Host for (.+)$/u, "Remnawave Host для $1"],
    [/^Geo updated: (\d+) nodes, failed: (\d+)$/u, "Geo обновлён: нод $1, ошибок $2"],
    [/^Updated (\d+) (.+)$/u, "Обновлено: $1 $2"],
    [/^No changes \((\d+) (.+)\)$/u, "Без изменений ($1 $2)"],
    [/^Backup verified \((\d+) files\)\. Restart the application to apply it\.$/u, "Бэкап проверен ($1 файлов). Перезапустите приложение для применения."],
    [/^(\d+) nodes$/u, "$1 нод"], [/^(\d+) results$/u, "$1 результатов"],
    [/^(\d+) selected$/u, "$1 выбрано"], [/^(\d+) incidents$/u, "Инцидентов: $1"],
    [/^(\d+) muted$/u, "$1 заглушено"], [/^(\d+) users$/u, "$1 пользователей"],
    [/^(\d+) successful results$/u, "$1 успешных результатов"],
    [/^(\d+) latest results$/u, "Последних результатов: $1"],
    [/^(\d+) successful of (\d+) results$/u, "$1 успешных из $2 результатов"],
    [/^(\d+)% available · (\d+) offline · (\d+) muted$/u, "$1% доступно · $2 офлайн · $3 заглушено"],
    [/^(\d+) shown · (\d+) total$/u, "$1 показано · $2 всего"],
    [/^(\d+) shown · (\d+) active · (\d+) retired$/u, "$1 показано · $2 активных · $3 выведенных"],
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
    [/^All (\d+) · Alerts (\d+) · Speed (\d+)$/u, "Все $1 · Алерты $2 · Скорость $3"],
    [/^Delete archived node "(.+)"\?\n\nThis removes the node registry entry and saved speed-test history\.$/u, "Удалить архивную ноду «$1»?\n\nБудут удалены запись реестра нод и сохранённая история speed-test."],
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
