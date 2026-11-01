// Interface language. The page is written in Russian; with English on, translateDom() replaces
// the static texts once at start and t()/tf() translate the texts made in code. Switching the
// language reloads the page, so nothing has to be translated back.

export type Lang = 'ru' | 'en';

const KEY = 'awg-lang';
export let lang: Lang = (() => {
    try { return localStorage.getItem(KEY) === 'en' ? 'en' : 'ru'; } catch { return 'ru'; }
})();

export function setLang(l: Lang) {
    lang = l;
    document.documentElement.lang = l;
    try { localStorage.setItem(KEY, l); } catch { /* storage off: the core still knows */ }
}

export const locale = () => (lang === 'en' ? 'en-GB' : 'ru-RU');

/** t returns the English text for a Russian one when English is on. */
export function t(ru: string): string {
    return lang === 'en' ? EN[ru] ?? ru : ru;
}

/** tf fills {0}, {1}… after translating the template. */
export function tf(ru: string, ...args: unknown[]): string {
    return t(ru).replace(/\{(\d+)\}/g, (_, i) => String(args[Number(i)] ?? ''));
}

const ATTRS = ['data-tip', 'data-short', 'placeholder', 'title', 'aria-label', 'label'];

/** translateDom replaces the Russian texts and attributes under root. */
export function translateDom(root: ParentNode) {
    if (lang !== 'en') return;
    const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
    for (let n = walker.nextNode(); n; n = walker.nextNode()) {
        const v = n.nodeValue ?? '';
        const key = v.replace(/\s+/g, ' ').trim();
        if (key && EN[key]) n.nodeValue = v.replace(v.trim(), EN[key]);
    }
    root.querySelectorAll<HTMLElement>('*').forEach(el => {
        for (const a of ATTRS) {
            const v = el.getAttribute(a);
            if (v && EN[v.trim()]) el.setAttribute(a, EN[v.trim()]);
        }
    });
}

const EN: Record<string, string> = {
    // ---- rail and header ----
    'Подключение': 'Connection',
    'Профили': 'Profiles',
    'Профили и ключи': 'Keys and profiles',
    'Сплит': 'Split',
    'Раздельный туннель': 'Split tunnel',
    'Трафик': 'Traffic',
    'Статистика': 'Statistics',
    'Настройки': 'Settings',
    'Журнал': 'Log',
    'Выход: отключить VPN и закрыть': 'Quit: disconnect the VPN and close',
    'Выход': 'Quit',
    'Язык: русский / English': 'Language: English / русский',
    'Язык': 'Language',
    'Не защищено': 'Not protected',
    'Защищено': 'Protected',
    'Интернет закрыт': 'Internet closed',

    // ---- connection page ----
    'Подключить / отключить': 'Connect / disconnect',
    'Отключено': 'Disconnected',
    'Подключено': 'Connected',
    'Подключение…': 'Connecting…',
    'Интернет заблокирован': 'Internet blocked',
    'Другой сервер': 'Another server',
    'Разблокировать интернет': 'Unblock the internet',
    'Скрыть': 'Hide',
    'Загрузка': 'Download',
    'Отдача': 'Upload',
    'Пинг': 'Ping',
    'Скорость за 2 минуты': 'Speed over 2 minutes',
    'загрузка': 'download',
    'отдача': 'upload',
    'пик': 'peak',
    'Тест скорости': 'Speed test',
    'Сервер': 'Server',
    'Рукопожатие': 'Handshake',
    'Трафик ↓/↑': 'Traffic ↓/↑',
    'Маршрутизация': 'Routing',
    'Программы только через VPN': 'Programs only through the VPN',
    'Приложения': 'Apps',
    'Страна': 'Country',
    'По умолчанию': 'Default',
    'Режим': 'Mode',
    'Умный режим': 'Smart mode',
    'Получить у провайдера свежий конфиг': 'Get a fresh config from the provider',
    'Обновить конфиг': 'Refresh config',
    'Сервер назначает провайдер; другой можно запросить раз в сутки': 'The provider assigns the server; another one can be requested once a day',
    'Служба «AWG Pilot VPN» не запущена — переустановите программу': 'The "AWG Pilot VPN" service is not running: reinstall the program',
    'Добавьте ключ или .conf на вкладке «Профили и ключи»': 'Add a key or a .conf on the "Keys and profiles" tab',
    'Профиль «{0}»: нет конфига — нажмите «Обновить конфиг»': 'Profile "{0}": no config, press "Refresh config"',
    'Ожидание ответа сервера…': 'Waiting for the server…',
    'Профиль «{0}»': 'Profile "{0}"',
    'ещё не было': 'not yet',
    '{0} с назад': '{0} s ago',
    '{0} мин назад': '{0} min ago',
    '{0} ч назад': '{0} h ago',
    'активен': 'on',
    'не при сплите': 'not with split',
    'выключен': 'off',
    'при подключении': 'when connected',
    'весь трафик': 'all traffic',
    'кроме списка': 'except the list',
    'только список': 'only the list',
    ', РФ напрямую': ', Russia direct',
    'Российские сайты и сервисы напрямую. Список RIPE NCC: {0}, обновляется раз в неделю': 'Russian sites and services go direct. RIPE NCC list: {0}, updated weekly',
    'заблокирован': 'blocked',
    'закрыт': 'closed',
    'не изменён': 'unchanged',
    'будет закрыт': 'will be closed',
    'не изменяется': 'not changed',
    'нет': 'none',
    '{0} — через туннель': '{0}: through the tunnel',
    '{0} — без сети до подключения': '{0}: offline until connected',
    '{0} через VPN, остальные напрямую': '{0} through the VPN, the rest direct',
    'все, кроме {0}': 'all except {0}',
    'все через VPN': 'all through the VPN',
    'VPN оборвался — интернет закрыт, чтобы ничего не ушло мимо VPN. Восстанавливаю подключение, пробую другие профили.': 'The VPN dropped: the internet is closed so that nothing leaks past the VPN. Restoring the connection and trying other profiles.',
    'Трафик идёт через другой VPN (адаптер «{0}»), а не через AWG Pilot. Отключите его, например AmneziaVPN.': 'Traffic goes through another VPN (adapter "{0}"), not AWG Pilot. Turn it off, for example AmneziaVPN.',
    'Адрес': 'Address',
    'нет ответа': 'no answer',
    '{0} мс': '{0} ms',

    // ---- keys and profiles ----
    'Вставьте ключ vpn://…': 'Paste a vpn://… key',
    'Добавить': 'Add',
    'Импорт .conf / .vpn': 'Import .conf / .vpn',
    'Экспорт .conf': 'Export .conf',
    'Каждый ключ или конфиг — отдельный профиль.': 'Every key or config is a separate profile.',
    'Хранятся только на этом ПК, зашифрованы Windows DPAPI. Переключать можно и из трея.': 'Stored only on this PC, encrypted with Windows DPAPI. You can switch them from the tray too.',
    'Хранятся только на этом телефоне, зашифрованы ключом из Android Keystore.': 'Stored only on this phone, encrypted with a key from the Android Keystore.',
    'Подписка': 'Subscription',
    'Обновить статус': 'Refresh status',
    'Устройство': 'Device',
    'Новый ID устройства…': 'New device ID…',
    'Привязать ключ к этому устройству…': 'Move the key to this device…',
    'Провайдер привязывает ключ к устройству. Новый ID может занять ещё одно место из лимита.': 'The provider binds the key to a device. A new ID may take another slot of the limit.',
    'Ключ {0} → {1}': 'Key {0} → {1}',
    'У активного профиля нет ключа vpn:// — статус подписки недоступен.': 'The active profile has no vpn:// key: the subscription status is unavailable.',
    'Статус': 'Status',
    'Оплачено до': 'Paid until',
    'Устройства': 'Devices',
    '{0} своб. из {1}': '{0} free of {1}',
    'Автопродление': 'Auto-renewal',
    'да': 'yes',
    'Название': 'Name',
    ' · нажмите «Обновить статус»': ' · press "Refresh status"',
    'AWG Pilot {0} · движок AmneziaWG 3.1': 'AWG Pilot {0} · AmneziaWG 3.1 engine',
    ' · служба не запущена': ' · service not running',
    'сохранён': 'saved',
    'создан в AWG Pilot': 'created in AWG Pilot',
    'ID устройства: {0}': 'Device ID: {0}',
    'ID устройства не выбран — без него запросы к провайдеру не отправляются.': 'No device ID chosen: without it no requests go to the provider.',
    'Профилей пока нет — вставьте ключ vpn:// или импортируйте .conf': 'No profiles yet: paste a vpn:// key or import a .conf',
    'ключ {0}': 'key {0}',
    'конфиг: {0}': 'config: {0}',
    'есть': 'yes',
    'нет конфига': 'no config',
    'активный': 'active',
    'Сделать активным': 'Make active',
    'Переименовать': 'Rename',
    'Удалить': 'Delete',

    // ---- split tunnel ----
    'Что идёт через VPN': 'What goes through the VPN',
    'Весь трафик': 'All traffic',
    'Кроме списка': 'Except the list',
    'Только список': 'Only the list',
    'Россия мимо VPN': 'Russia bypasses the VPN',
    'Российские сайты и сервисы напрямую (список сетей RIPE NCC, обновляется раз в неделю)': 'Russian sites and services go direct (RIPE NCC network list, updated weekly)',
    'Локальная сеть мимо VPN': 'Local network bypasses the VPN',
    '192.168.x, 10.x и т.п.': '192.168.x, 10.x and so on',
    'Адреса, подсети и домены, по одному в строке': 'Addresses, subnets and domains, one per line',
    'Адреса, подсети и домены, по одному в строке\nexample.com\n192.168.0.0/16': 'Addresses, subnets and domains, one per line\nexample.com\n192.168.0.0/16',
    'Домены превращаются в IP при подключении. Kill switch действует только в режиме «Весь трафик».': 'Domains turn into IPs when connecting. The kill switch works only in "All traffic" mode.',
    'Все': 'All',
    'Выбранные': 'Chosen',
    'Кроме них': 'All but these',
    'Выбрать приложения': 'Choose apps',
    'Добавить программу': 'Add a program',
    'Без VPN у этих программ нет сети совсем, в том числе на время переподключения. Правила держит служба, они действуют и при закрытом окне.': 'Without the VPN these programs have no network at all, even while reconnecting. The service holds the rules, so they work with the window closed too.',
    'Сохранить': 'Save',
    'Убрать из списка': 'Remove from the list',
    'Убрать': 'Remove',
    'Список пуст': 'The list is empty',
    'Приложения не выбраны': 'No apps chosen',
    'Все приложения идут через VPN.': 'All apps go through the VPN.',
    'Через VPN идут только выбранные приложения, остальные — напрямую.': 'Only the chosen apps go through the VPN, the rest go direct.',
    'Выбранные приложения идут напрямую, все остальные — через VPN.': 'The chosen apps go direct, all others through the VPN.',

    // ---- statistics and traffic log ----
    'Сегодня': 'Today',
    '7 дней': '7 days',
    'Этот месяц': 'This month',
    'Всего': 'Total',
    'Трафик по дням за 30 дней': 'Traffic per day over 30 days',
    '30 дней · макс.': '30 days · max',
    'Очистить статистику': 'Clear statistics',
    'с {0}': 'since {0}',
    'Кто передаёт трафик через VPN': 'Who sends traffic through the VPN',
    'Записывать трафик по программам': 'Record traffic by program',
    'Какая программа или приложение, куда и сколько отправили и получили. Журнал хранится только на этом устройстве': 'Which program or app sent and received how much, and where. The log stays on this device only',
    'День': 'Day',
    '↑ отдача': '↑ upload',
    '↓ загрузка': '↓ download',
    'всего': 'total',
    'Учитывается трафик внутри туннеля, без шифрования и служебных пакетов VPN, поэтому сумма немного меньше, чем в статистике выше. Имя сайта берётся из DNS-ответов и TLS, иначе показан IP-адрес.': 'Traffic inside the tunnel is counted, without encryption and VPN service packets, so the total is a little lower than in the statistics above. Site names come from DNS answers and TLS, otherwise the IP address is shown.',
    'Очистить журнал': 'Clear the log',
    '30 дней': '30 days',
    '6 месяцев': '6 months',
    '12 месяцев': '12 months',
    'Всё время': 'All time',
    'По дням': 'By day',
    '{0} программ · ↑ {1} · ↓ {2}': '{0} programs · ↑ {1} · ↓ {2}',
    'Пока ничего не записано — подключите VPN': 'Nothing recorded yet: connect the VPN',
    'Журнал выключен': 'The log is off',
    '{0} соед.': '{0} conn.',
    'и ещё': 'and more:',
    '{0} адресов · {1} соединений': '{0} addresses · {1} connections',

    // ---- settings ----
    'Оформление': 'Appearance',
    'Ночь': 'Night',
    'Мята': 'Mint',
    'Океан': 'Ocean',
    'Коралл': 'Coral',
    'Лаванда': 'Lavender',
    'Лимон': 'Lemon',
    'Графит': 'Graphite',
    'Полночь': 'Midnight',
    'Лес': 'Forest',
    'Малина': 'Raspberry',
    'Арктика': 'Arctic',
    'Защита': 'Protection',
    'Блокировать интернет без VPN': 'Block the internet without the VPN',
    'Постоянная VPN и блокировка подключений без VPN включаются в настройках Android': 'Always-on VPN and blocking connections without the VPN are turned on in Android settings',
    'Открыть': 'Open',
    'Отключать интернет при обрыве VPN': 'Cut the internet when the VPN drops',
    'Пока VPN не восстановится (пробуются и другие профили) или пока вы не нажмёте «Отключить»': 'Until the VPN is back (other profiles are tried too) or until you press "Disconnect"',
    'Блокировать интернет мимо VPN, пока туннель включён': 'Block the internet past the VPN while the tunnel is on',
    'Блокировать IPv6': 'Block IPv6',
    'Защита от утечки мимо VPN': 'Protection against leaks past the VPN',
    'Автопереподключение': 'Auto-reconnect',
    'Если сервер не отвечает 3 минуты': 'If the server does not answer for 3 minutes',
    'Сам запрашивать другой сервер': 'Request another server automatically',
    'Если не помогли переподключение и свежий конфиг (раз в сутки)': 'If reconnecting and a fresh config did not help (once a day)',
    'Запуск и окна': 'Startup and windows',
    'Запуск': 'Startup',
    'Запускать вместе с Windows': 'Start with Windows',
    'Свёрнутым в трей': 'Minimized to the tray',
    'Подключаться при запуске': 'Connect on start',
    'Окошко статуса поверх окон': 'Status window on top',
    'Зелёное — VPN работает, красное — связь прервана': 'Green means the VPN works, red means the link is down',
    'Закрытие сворачивает в трей': 'Closing minimizes to the tray',
    'Выход — через меню значка в трее': 'Quit from the tray icon menu',
    'Уведомления': 'Notifications',
    'Потеря связи, восстановление, скорость в шторке': 'Lost link, recovery, speed in the notification shade',
    'Следить за подпиской': 'Watch the subscription',
    'Проверка раз в 6 часов, предупреждение за 3 дня': 'Checked every 6 hours, warning 3 days ahead',
    'Сеть': 'Network',
    'Провайдера (из конфига)': "Provider's (from the config)",
    'AdGuard (без рекламы)': 'AdGuard (no ads)',
    'Свои серверы': 'Custom servers',
    'Свои DNS': 'Custom DNS',
    '0 — из конфига': '0 = from the config',
    'Применится при следующем подключении': 'Applies on the next connection',

    // ---- dialogs ----
    'Выбор приложений': 'Choose apps',
    'Поиск': 'Search',
    'Показывать системные': 'Show system apps',
    'Отмена': 'Cancel',
    'Готово': 'Done',
    'Ничего не найдено': 'Nothing found',
    'Загрузка списка приложений…': 'Loading the app list…',
    'Тест скорости через VPN': 'Speed test through the VPN',
    'Подключение к серверу теста…': 'Connecting to the test server…',
    'Измеряю загрузку…': 'Measuring download…',
    'Измеряю отдачу…': 'Measuring upload…',
    'Мбит/с': 'Mbit/s',
    'Сервер теста — Cloudflare (speed.cloudflare.com). Тест расходует около 100–300 МБ трафика.': 'The test server is Cloudflare (speed.cloudflare.com). A test uses about 100–300 MB of traffic.',
    'Закрыть': 'Close',
    'Ещё раз': 'Again',
    'Замер шёл с адреса <b>{0}</b>': 'Measured from <b>{0}</b>',
    ' — похоже, мимо VPN': ': looks like it bypassed the VPN',
    ' — это выход VPN, не ваш домашний адрес': ': this is the VPN exit, not your home address',
    'Ошибка: {0}': 'Error: {0}',
    '{0} Мбит/с': '{0} Mbit/s',
    'Финляндия': 'Finland', 'Нидерланды': 'Netherlands', 'Германия': 'Germany', 'Швеция': 'Sweden', 'Эстония': 'Estonia',
    'Латвия': 'Latvia', 'Литва': 'Lithuania', 'Польша': 'Poland', 'Франция': 'France', 'Великобритания': 'United Kingdom',
    'США': 'USA', 'Казахстан': 'Kazakhstan', 'Турция': 'Turkey', 'Армения': 'Armenia', 'Грузия': 'Georgia', 'Россия': 'Russia',

    // ---- actions and messages ----
    'Сохранено. Изменения применятся при следующем подключении.': 'Saved. Changes apply on the next connection.',
    'Выйти из AWG Pilot?': 'Quit AWG Pilot?',
    'VPN будет отключён.': 'The VPN will be disconnected.',
    'Выйти': 'Quit',
    'Выбор программы: {0}': 'Choosing a program: {0}',
    'Автозапуск': 'Autostart',
    'AWG Pilot будет запускаться вместе с Windows': 'AWG Pilot will start with Windows',
    'Автозапуск выключен': 'Autostart is off',
    'Получение конфига': 'Getting the config',
    'Ключ используется на другом устройстве': 'The key is used on another device',
    'Все места для устройств в подписке заняты. Использовать ключ на этом устройстве? Провайдер перенесёт его сюда, и на другом устройстве VPN может перестать работать.': 'All device slots of the subscription are taken. Use the key on this device? The provider will move it here, and the VPN may stop working on the other device.',
    'Использовать здесь': 'Use here',
    'отменено — ключ остался на другом устройстве': 'cancelled: the key stayed on the other device',
    'Конфиг обновлён': 'Config updated',
    'Запросить другой сервер?': 'Request another server?',
    'Следующая смена будет доступна только через сутки.': 'The next change will be available only in 24 hours.',
    'Запросить': 'Request',
    'Смена сервера': 'Server change',
    'Получен другой сервер': 'Got another server',
    'Баннер': 'Banner',
    'Разблокировать интернет?': 'Unblock the internet?',
    'Трафик пойдёт напрямую, мимо VPN, пока VPN не подключится.': 'Traffic will go direct, past the VPN, until the VPN connects.',
    'Разблокировать': 'Unblock',
    'Разблокировка': 'Unblocking',
    'Интернет разблокирован': 'Internet unblocked',
    'Окошко статуса': 'Status window',
    'Ключ': 'Key',
    'Ключ добавлен как профиль': 'Key added as a profile',
    'Создать новый ID устройства?': 'Create a new device ID?',
    'Провайдер может засчитать его как ещё одно устройство (занять место из лимита).': 'The provider may count it as another device (take a slot of the limit).',
    'Создать': 'Create',
    'ID устройства': 'Device ID',
    'Создан новый ID устройства': 'New device ID created',
    'Журнал трафика': 'Traffic log',
    'Журнал трафика включён': 'Traffic log on',
    'Журнал трафика выключен': 'Traffic log off',
    'Очистить журнал трафика?': 'Clear the traffic log?',
    'Записи по программам за все дни будут удалены.': 'Per-program records for all days will be deleted.',
    'Очистить': 'Clear',
    'Журнал очищен': 'Log cleared',
    'Очистить статистику?': 'Clear statistics?',
    'Вся история трафика будет удалена.': 'The whole traffic history will be deleted.',
    'Статистика очищена': 'Statistics cleared',
    'Профиль': 'Profile',
    'Выбран профиль «{0}»': 'Profile "{0}" selected',
    'Переименовать профиль': 'Rename profile',
    'Переименование': 'Renaming',
    'Удалить профиль «{0}»?': 'Delete profile "{0}"?',
    'Ключ и конфиг профиля будут удалены с этого устройства.': "The profile's key and config will be deleted from this device.",
    'Удаление': 'Deleting',
    'Профиль удалён': 'Profile deleted',
    'Статус подписки': 'Subscription status',
    'Импорт': 'Import',
    'Экспорт': 'Export',
    'Привязать ключ к этому устройству?': 'Move the key to this device?',
    'Провайдер перепривяжет ключ к этому устройству и выдаст новый конфиг. Другое устройство с этим ключом может перестать работать.': 'The provider will move the key to this device and issue a new config. Another device with this key may stop working.',
    'Привязать': 'Move',
    'Перепривязка': 'Moving the key',
    'Ключ привязан к этому устройству, конфиг обновлён': 'The key is bound to this device, config updated',
    'Тема': 'Theme',
    'Ошибка': 'Error',

    // ---- status window ----
    'Двойной щелчок — открыть AWG Pilot': 'Double-click to open AWG Pilot',
    'Скрыть окошко': 'Hide the window',
    'макс {0}': 'max {0}',
    'Нет связи со службой': 'No link to the service',
};

/** plural picks the word form for n: Russian one/few/many ("1 адрес, 2 адреса, 5 адресов"), English one/other. */
export function plural(n: number, ru: [string, string, string], en: [string, string]): string {
    if (lang === 'en') return `${n} ${n === 1 ? en[0] : en[1]}`;
    const d = n % 10, h = n % 100;
    const form = d === 1 && h !== 11 ? ru[0] : d >= 2 && d <= 4 && (h < 12 || h > 14) ? ru[1] : ru[2];
    return `${n} ${form}`;
}

/** unit names for bytes(): B, KB… in English. */
export const units = () => (lang === 'en' ? ['B', 'KB', 'MB', 'GB', 'TB'] : ['Б', 'КБ', 'МБ', 'ГБ', 'ТБ']);
export const perSec = () => (lang === 'en' ? '/s' : '/с');
