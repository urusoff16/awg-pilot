import './style.css';
import * as Go from '../wailsjs/go/main/App';
import {t as tr, tf, lang, setLang, locale, translateDom, units, perSec, plural} from './i18n';

type AnyObj = Record<string, any>;

const $ = <T extends HTMLElement = HTMLElement>(id: string) => document.getElementById(id) as T;
// The language remembered from the last run paints the first frame; the core's choice wins below.
setLang(lang);
translateDom(document.body);
let state: AnyObj = {};
let busy = false;
let settingsDirty = false;
let apps: string[] = []; // editable copy of settings.vpn_only_apps
const android = document.body.classList.contains('android');
let pkgs: string[] = []; // Android: editable copy of settings.app_packages
type AppInfo = {pkg: string, label: string, system: boolean};
let appIndex: AppInfo[] | null = null; // installed apps, loaded on first use

// ---------- helpers ----------
function banner(msg: string, kind: 'ok' | 'err' = 'err') {
    const b = $('banner');
    b.textContent = msg;
    b.className = `toast ${kind}`;
    window.clearTimeout((b as any)._t);
    (b as any)._t = window.setTimeout(() => b.classList.add('hidden'), kind === 'err' ? 12000 : 5000);
    b.onclick = () => b.classList.add('hidden');
}

// ask shows the in-app dialog (native confirm/prompt look foreign in WebView2).
// Resolves to the entered text (or '' without input) on OK, null on cancel.
function ask(title: string, text: string, opts: {ok?: string, danger?: boolean, input?: string} = {}): Promise<string | null> {
    const m = $('modal'), inp = $<HTMLInputElement>('mInput');
    $('mTitle').textContent = title;
    $('mText').textContent = text;
    $('mOk').textContent = opts.ok ?? 'OK';
    m.querySelector('.sheet')!.classList.toggle('danger', !!opts.danger);
    inp.classList.toggle('hidden', opts.input === undefined);
    inp.value = opts.input ?? '';
    m.classList.remove('hidden');
    (opts.input !== undefined ? inp : $('mOk')).focus();
    return new Promise(resolve => {
        const done = (v: string | null) => {
            m.classList.add('hidden');
            m.onkeydown = $('mOk').onclick = $('mCancel').onclick = m.onclick = null;
            resolve(v);
        };
        $('mOk').onclick = () => done(opts.input !== undefined ? inp.value : '');
        $('mCancel').onclick = () => done(null);
        m.onclick = ev => { if (ev.target === m) done(null); };
        m.onkeydown = ev => {
            if (ev.key === 'Escape') done(null);
            if (ev.key === 'Enter') $('mOk').click();
        };
    });
}
const confirmed = async (title: string, text: string, ok: string, danger = false) => (await ask(tr(title), tr(text), {ok: tr(ok), danger})) !== null;

// errText shows an error from the core in the interface language (Windows passes them in Russian).
async function errText(e: any): Promise<string> {
    const msg = String(e?.message ?? e);
    if (lang !== 'en' || !/[\u0400-\u04ff]/.test(msg)) return msg;
    try { return await (window as any).go.main.App.Translate(msg); } catch { return msg; }
}

async function run(label: string, fn: () => Promise<unknown>, okMsg?: string) {
    if (busy) return;
    busy = true;
    document.body.classList.add('busy');
    try {
        await fn();
        if (okMsg) banner(tr(okMsg), 'ok');
    } catch (e: any) {
        banner(`${tr(label)}: ${await errText(e)}`);
    } finally {
        busy = false;
        document.body.classList.remove('busy');
        await refresh();
    }
}

const bytes = (n: number) => {
    const u = units();
    if (!n) return `0 ${u[0]}`;
    const i = Math.min(u.length - 1, Math.floor(Math.log(n) / Math.log(1024)));
    return `${(n / 1024 ** i).toFixed(i ? 1 : 0)} ${u[i]}`;
};

const ago = (iso?: string) => {
    if (!iso || iso.startsWith('0001')) return tr('ещё не было');
    const s = Math.max(0, Math.round((Date.now() - Date.parse(iso)) / 1000));
    if (s < 60) return tf('{0} с назад', s);
    if (s < 3600) return tf('{0} мин назад', Math.floor(s / 60));
    return tf('{0} ч назад', Math.floor(s / 3600));
};

const duration = (iso?: string) => {
    if (!iso || iso.startsWith('0001')) return '—';
    let s = Math.round((Date.now() - Date.parse(iso)) / 1000);
    const h = Math.floor(s / 3600); s -= h * 3600;
    const m = Math.floor(s / 60); s -= m * 60;
    return `${h}:${String(m).padStart(2, '0')}:${String(s).padStart(2, '0')}`;
};

const esc = (s: unknown) => String(s ?? '').replace(/[&<>"']/g, c => ({'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'}[c]!));

// ---------- render ----------
function render() {
    const t = state.tunnel ?? {};
    const s = state.settings ?? {};
    const handshakeOk = t.up && t.last_handshake && !String(t.last_handshake).startsWith('0001');
    document.body.classList.toggle('connected', !!handshakeOk);
    document.body.classList.toggle('pending', !!t.up && !handshakeOk);
    document.body.classList.toggle('blocked', !!t.blocked);

    $('statusTitle').textContent = tr(handshakeOk ? 'Подключено' : t.up ? 'Подключение…' : t.blocked ? 'Интернет заблокирован' : 'Отключено');
    $('pillText').textContent = tr(handshakeOk ? 'Защищено' : t.up ? 'Подключение…' : t.blocked ? 'Интернет закрыт' : 'Не защищено');
    const active = (state.profiles ?? []).find((p: AnyObj) => p.id === state.active_id);
    $('statusSub').textContent = !state.service_ok
        ? tr('Служба «AWG Pilot VPN» не запущена — переустановите программу')
        : !active ? tr('Добавьте ключ или .conf на вкладке «Профили и ключи»')
        : !state.has_config ? tf('Профиль «{0}»: нет конфига — нажмите «Обновить конфиг»', active.name)
        : t.up && !handshakeOk ? tr('Ожидание ответа сервера…')
        : tf('Профиль «{0}»', active.name) + (t.up && t.endpoint ? ` · ${t.endpoint.replace(/:\d+$/, '')}` : '');

    $('stEndpoint').textContent = t.endpoint || state.config_info?.endpoint || '—';
    $('stHandshake').textContent = t.up ? ago(t.last_handshake) : '—';
    $('stTraffic').textContent = t.up ? `${bytes(t.rx_bytes)} / ${bytes(t.tx_bytes)}` : '—';
    $('stUptime').textContent = t.up ? duration(t.since) : '00:00:00';
    const splitActive = (s.split?.mode && s.split.mode !== 'full') || s.split?.bypass_lan || s.split?.bypass_russia;
    $('stKill').textContent = tr(t.up ? (t.kill_switch ? 'активен' : (s.kill_switch && splitActive ? 'не при сплите' : 'выключен'))
        : !s.kill_switch ? 'выключен' : splitActive ? 'не при сплите' : 'при подключении');
    const modeName: AnyObj = {full: 'весь трафик', exclude: 'кроме списка', include: 'только список'};
    const routing = tr(modeName[s.split?.mode] ?? 'весь трафик')
        + (s.split?.bypass_russia && s.split?.mode !== 'include' ? tr(', РФ напрямую') : '')
        + (s.split?.bypass_lan ? ', LAN' : '');
    $('stRouting').textContent = routing;
    $('stRouting').title = routing;
    if (state.russia_list) $('russiaInfo').textContent = tf('Российские сайты и сервисы напрямую. Список RIPE NCC: {0}, обновляется раз в неделю', state.russia_list);
    renderProfiles(active);
    renderUsage(state.usage);
    $('stIPv6').textContent = tr(t.up ? (t.ipv6_blocked ? 'заблокирован' : (t.kill_switch ? 'закрыт' : 'не изменён')) : (s.block_ipv6 ? 'будет закрыт' : 'не изменяется'));
    const nApps = (s.vpn_only_apps ?? []).length;
    $('stApps').textContent = !nApps ? tr('нет') : t.up ? tf('{0} — через туннель', nApps) : tf('{0} — без сети до подключения', nApps);
    if (android) {
        const nPkg = (s.app_packages ?? []).length;
        $('stAppsK').textContent = tr('Приложения');
        $('stApps').textContent = s.app_mode === 'include' ? tf('{0} через VPN, остальные напрямую', nPkg)
            : s.app_mode === 'exclude' && nPkg ? tf('все, кроме {0}', nPkg) : tr('все через VPN');
    }

    renderSpeed(t.up ? state.speed : null);
    const al = t.blocked
        ? {message: tr('VPN оборвался — интернет закрыт, чтобы ничего не ушло мимо VPN. Восстанавливаю подключение, пробую другие профили.'), action: 'unlock'}
        : state.other_vpn
            ? {message: tf('Трафик идёт через другой VPN (адаптер «{0}»), а не через AWG Pilot. Отключите его, например AmneziaVPN.', state.other_vpn), action: ''}
            : state.alert;
    $('alert').classList.toggle('hidden', !al);
    if (al) {
        $('alertText').textContent = al.message;
        $('btnAlertAction').classList.toggle('hidden', al.action !== 'change_server');
        $('btnUnlock').classList.toggle('hidden', al.action !== 'unlock');
        $('btnAlertClose').classList.toggle('hidden', al.action === 'unlock');
    }

    const ci = state.config_info;
    $('stEndpoint').title = ci ? `${tr('Адрес')} ${(ci.address ?? []).join(', ')}\nDNS ${(ci.dns ?? []).join(', ')}\nMTU ${ci.mtu} · ${ci.awg}` : '';

    // countries / connection types
    const selC = $<HTMLSelectElement>('selCountry');
    const countries: AnyObj[] = state.countries ?? [];
    if (selC.options.length - 1 !== countries.length) {
        selC.innerHTML = `<option value="">${tr('По умолчанию')}</option>` + countries.map(c => `<option value="${esc(c.iso_country_code)}">${esc(c.country_label)}</option>`).join('');
    }
    if (document.activeElement !== selC) selC.value = s.country ?? '';
    // Some providers assign the server themselves; then there is nothing to choose.
    selC.closest('label')!.classList.toggle('hidden', countries.length === 0);
    selC.closest('.row')!.classList.toggle('hidden', countries.length === 0 && (state.connections ?? []).length <= 1);
    // A profile imported from a config has no provider to refresh it or change the server.
    $('btnDownload').parentElement!.classList.toggle('hidden', !state.has_key);
    const selK = $<HTMLSelectElement>('selKeyType');
    const conns: AnyObj[] = state.connections ?? [];
    if (conns.length && selK.options.length !== conns.length) {
        selK.innerHTML = conns.map(c => `<option value="${esc(c.key_type)}">${esc(c.localized_title?.ru ?? c.title)}</option>`).join('');
    }
    if (document.activeElement !== selK) selK.value = s.key_type ?? '';
    selK.closest('label')!.classList.toggle('hidden', conns.length <= 1);

    // account
    $('keyInfo').textContent = state.has_key ? tf('Ключ {0} → {1}', state.masked_key, state.api_endpoint) : tr('У активного профиля нет ключа vpn:// — статус подписки недоступен.');
    const a = state.account;
    $('account').innerHTML = a ? [
        [tr('Статус'), a.simplified_status],
        [tr('Оплачено до'), a.paid_until ? new Date(a.paid_until).toLocaleDateString(locale()) : '—'],
        [tr('Устройства'), tf('{0} своб. из {1}', a.number_of_available_keys, a.key_limit)],
        [tr('Автопродление'), tr(a.auto_renewal ? 'да' : 'нет')],
        ...(a.alias ? [[tr('Название'), a.alias]] : []),
    ].map(([k, v]) => `<div><span>${esc(k)}</span><b title="${esc(v)}">${esc(v)}</b></div>`).join('') : '';
    if (state.has_key && !a) $('keyInfo').textContent += tr(' · нажмите «Обновить статус»');

    $('foot').textContent = tf('AWG Pilot {0} · движок AmneziaWG 3.1', state.version ?? '') + (state.service_ok ? '' : tr(' · служба не запущена'));
    $('railVer').textContent = state.version ? `v${state.version}` : '';
    $('logo').title = `AWG Pilot v${state.version ?? ''}`;
    const devText: AnyObj = {saved: 'сохранён', generated: 'создан в AWG Pilot'};
    $('devInfo').textContent = state.device_source ? tf('ID устройства: {0}', tr(devText[state.device_source] ?? state.device_source))
        : tr('ID устройства не выбран — без него запросы к провайдеру не отправляются.');

    if (!settingsDirty) fillSettings(s);
    $<HTMLInputElement>('chkAutostart').checked = !!state.autostart;
    if (document.activeElement !== $('chkWidget')) $<HTMLInputElement>('chkWidget').checked = !!s.show_widget;
}

const rate = (bps: number) => `${bytes(bps)}${perSec()}`;

function renderSpeed(sp: AnyObj | null) {
    $('spRx').textContent = sp ? rate(sp.rx_bps) : '—';
    $('spTx').textContent = sp ? rate(sp.tx_bps) : '—';
    $('spPing').textContent = !sp ? '—' : sp.ping_ms >= 0 ? tf('{0} мс', sp.ping_ms) : tr('нет ответа');
    const hist: AnyObj[] = sp?.history ?? [];
    const W = 600, H = 90, N = 120;
    const peak = Math.max(1, ...hist.map(p => Math.max(p.rx, p.tx)));
    const pts = (key: 'rx' | 'tx') => hist.map((p, i) =>
        `${(W * (N - hist.length + i) / (N - 1)).toFixed(1)},${(H - 3 - (H - 8) * p[key] / peak).toFixed(1)}`).join(' ');
    const rx = pts('rx');
    $('lineRx').setAttribute('points', rx);
    $('lineTx').setAttribute('points', pts('tx'));
    $('areaRx').setAttribute('d', hist.length > 1
        ? `M${(W * (N - hist.length) / (N - 1)).toFixed(1)},${H} L${rx.replace(/ /g, ' L')} L${W},${H} Z` : '');
    $('chartPeak').textContent = hist.length ? rate(peak) : '—';
    $('chart').parentElement!.classList.toggle('empty', hist.length < 2);
}

let profSig = '';
function renderProfiles(active: AnyObj | undefined) {
    const list: AnyObj[] = state.profiles ?? [];
    const sig = JSON.stringify([list, state.active_id]);
    if (sig === profSig) return; // keep focus/hover stable between polls
    profSig = sig;
    const ic = (name: string) => `<svg><use href="#i-${name}"/></svg>`;
    $('profList').innerHTML = !list.length ? `<li class="empty">${tr('Профилей пока нет — вставьте ключ vpn:// или импортируйте .conf')}</li>`
        : list.map(p => {
            const what = [p.has_key ? tf('ключ {0}', esc(p.masked_key)) : '', p.has_config ? tf('конфиг: {0}', esc(p.config_for || tr('есть'))) : tr('нет конфига')].filter(Boolean).join(' · ');
            const isActive = active && p.id === active.id;
            const id = esc(p.id);
            return `<li class="${isActive ? 'active' : ''}">
                <svg class="li-ic"><use href="#i-${p.has_key ? 'key' : 'file'}"/></svg>
                <span class="li-main"><span class="li-name">${esc(p.name)}</span><span class="li-sub" title="${what}">${what}</span></span>
                <span class="li-act">
                    ${isActive ? `<span class="badge">${tr('активный')}</span>` : `<button class="icon ok" data-act="use" data-id="${id}" data-tip="${tr('Сделать активным')}" aria-label="${tr('Сделать активным')}">${ic('check')}</button>`}
                    <button class="icon" data-act="rename" data-id="${id}" data-tip="${tr('Переименовать')}" aria-label="${tr('Переименовать')}">${ic('edit')}</button>
                    <button class="icon del" data-act="delete" data-id="${id}" data-tip="${tr('Удалить')}" aria-label="${tr('Удалить')}">${ic('trash')}</button>
                </span></li>`;
        }).join('');
}

function renderUsage(u: AnyObj | undefined) {
    const pair = (d: AnyObj | undefined) => d ? bytes((d.rx ?? 0) + (d.tx ?? 0)) : '—';
    const sub = (d: AnyObj | undefined) => d ? `↓ ${bytes(d.rx ?? 0)} · ↑ ${bytes(d.tx ?? 0)}` : '';
    $('usToday').textContent = pair(u?.today); $('usTodaySub').textContent = sub(u?.today);
    $('usWeek').textContent = pair(u?.week); $('usWeekSub').textContent = sub(u?.week);
    $('usMonth').textContent = pair(u?.month); $('usMonthSub').textContent = sub(u?.month);
    $('usAll').textContent = pair(u?.all); $('usAllSub').textContent = u?.since ? tf('с {0}', new Date(u.since).toLocaleDateString(locale())) : '';
    const days: AnyObj[] = u?.last30 ?? [];
    const W = 600, H = 140, bw = W / 30;
    const peak = Math.max(1, ...days.map(d => d.rx + d.tx));
    $('usChart').innerHTML = days.map((d, i) => {
        const hRx = (H - 14) * d.rx / peak, hTx = (H - 14) * d.tx / peak;
        const x = i * bw + 2, w = bw - 4;
        const title = `${new Date(d.date).toLocaleDateString(locale())}: ↓ ${bytes(d.rx)}, ↑ ${bytes(d.tx)}`;
        return `<g><title>${esc(title)}</title>`
            + `<rect x="${x}" y="${H - hRx - hTx}" width="${w}" height="${hRx}" rx="2" class="bar-rx"/>`
            + `<rect x="${x}" y="${H - hTx}" width="${w}" height="${hTx}" rx="2" class="bar-tx"/></g>`;
    }).join('');
    $('usPeak').textContent = days.length ? bytes(peak) : '—';
}

// ---------- skins ----------
// The skin is saved in the settings; the local copy only paints the first frame in the right colours.
let skinSaving = false;
function applySkin(name: string) {
    if (name && !document.querySelector(`.skin[data-skin="${name}"]`)) name = ''; // a skin from an older version
    if (name) document.documentElement.dataset.skin = name; else delete document.documentElement.dataset.skin;
    document.querySelectorAll<HTMLElement>('.skin').forEach(b => b.classList.toggle('active', (b.dataset.skin ?? '') === name));
    try { localStorage.setItem('awg-skin', name); } catch { /* storage off: the settings still keep it */ }
}
try { applySkin(localStorage.getItem('awg-skin') ?? ''); } catch { /* ignore */ }
$('skins').addEventListener('click', ev => {
    const b = (ev.target as HTMLElement).closest<HTMLElement>('.skin');
    if (!b) return;
    const name = b.dataset.skin ?? '';
    applySkin(name);
    if (state.settings) state.settings.skin = name;
    skinSaving = true;
    (window as any).go.main.App.SetSkin(name).catch((e: any) => banner(String(e?.message ?? e))).finally(() => skinSaving = false);
});

// ---------- traffic by program (desktop) ----------
let flowRep: AnyObj | null = null;
let flowAt = 0;
let flowLoading = false;
const flowOpen = new Set<string>();

async function loadFlows(force = false) {
    if (flowLoading || (!force && Date.now() - flowAt < 5000)) return;
    flowLoading = true;
    try {
        // Android records package names; the installed-app list gives their labels and icons
        if (android && !appIndex) appIndex = await (window as any).go.main.App.ListApps() as AppInfo[];
        const sel = $<HTMLSelectElement>('flowDay');
        flowRep = await Go.TrafficLog(sel.value || '') as AnyObj;
        flowAt = Date.now();
        renderFlows();
    } catch (e: any) {
        flowAt = Date.now();
        $('flowSum').textContent = String(e?.message ?? e);
    } finally {
        flowLoading = false;
    }
}

// when shows the time of a connection today, or its date for older ones.
const when = (unix: number) => {
    const t = new Date(unix * 1000);
    return t.toDateString() === new Date().toDateString()
        ? t.toLocaleTimeString(locale(), {hour: '2-digit', minute: '2-digit'})
        : t.toLocaleDateString(locale(), {day: '2-digit', month: '2-digit'});
};

function renderFlows() {
    const r = flowRep;
    if (!r) return;
    $<HTMLInputElement>('chkFlows').checked = !!r.enabled;
    const rows: AnyObj[] = r.rows ?? [];
    $('flowBody').classList.toggle('hidden', !r.enabled && rows.length === 0);
    const sel = $<HTMLSelectElement>('flowDay');
    const days: string[] = r.days ?? [];
    const periods = [['', 'Сегодня'], ['week', '7 дней'], ['month', '30 дней'], ['6m', '6 месяцев'], ['12m', '12 месяцев'], ['all', 'Всё время']];
    const opts = periods.map(([v, t]) => `<option value="${v}">${tr(t)}</option>`).join('')
        + (days.length > 1 ? `<optgroup label="${tr('По дням')}">` + days.slice(1).map(d => `<option value="${esc(d)}">${esc(new Date(d).toLocaleDateString(locale()))}</option>`).join('') + '</optgroup>' : '');
    if (sel.dataset.opts !== opts) {
        const v = sel.value;
        sel.innerHTML = opts;
        sel.dataset.opts = opts;
        sel.value = v;
        if (sel.selectedIndex < 0) sel.selectedIndex = 0;
    }
    const sort = document.querySelector<HTMLInputElement>('input[name=flowsort]:checked')?.value ?? 'tx';
    const val = (x: AnyObj) => sort === 'tx' ? x.tx : sort === 'rx' ? x.rx : x.tx + x.rx;
    type App = {name: string, tx: number, rx: number, conns: number, dest: AnyObj[]};
    const byApp = new Map<string, App>();
    let tx = 0, rx = 0;
    for (const row of rows) {
        let a = byApp.get(row.app);
        if (!a) byApp.set(row.app, a = {name: row.app, tx: 0, rx: 0, conns: 0, dest: []});
        a.tx += row.tx; a.rx += row.rx; a.conns += row.conns; a.dest.push(row);
        tx += row.tx; rx += row.rx;
    }
    const list = [...byApp.values()].sort((a, b) => val(b) - val(a) || a.name.localeCompare(b.name));
    $('flowSum').textContent = rows.length ? `${plural(list.length, ['программа', 'программы', 'программ'], ['program', 'programs'])} · ↑ ${bytes(tx)} · ↓ ${bytes(rx)}` : '';
    const top = Math.max(1, ...list.map(val));
    const num = (x: AnyObj) => `<span class="fnum"><span class="tx">↑ ${bytes(x.tx)}</span><br><span class="rx">↓ ${bytes(x.rx)}</span></span>`;
    const ul = $('flowList');
    if (!list.length) {
        ul.innerHTML = `<li class="empty">${tr(r.enabled ? 'Пока ничего не записано — подключите VPN' : 'Журнал выключен')}</li>`;
        return;
    }
    ul.innerHTML = list.map(a => {
        const dest = a.dest.sort((x, y) => val(y) - val(x));
        const shown = dest.slice(0, 40).map(d => `<div><span class="fh" title="${esc(d.host)}">${esc(d.host)}<small>${esc(d.proto)} ${d.port || ''} · ${tf('{0} соед.', d.conns)} · ${esc(when(d.last))}</small></span>${num(d)}</div>`).join('');
        const more = dest.length > 40 ? `<div><span class="fh"><small>${tr('и ещё') + ' ' + plural(dest.length - 40, ['адрес', 'адреса', 'адресов'], ['address', 'addresses'])}</small></span></div>` : '';
        const pkg = android && /^[a-z][\w]*(\.[\w]+)+$/i.test(a.name);
        const label = pkg ? appLabel(a.name) : a.name;
        const sub = (pkg && label !== a.name ? a.name + ' · ' : '') + `${plural(dest.length, ['адрес', 'адреса', 'адресов'], ['address', 'addresses'])} · ${plural(a.conns, ['соединение', 'соединения', 'соединений'], ['connection', 'connections'])}`;
        return `<li class="${flowOpen.has(a.name) ? 'open' : ''}" data-app="${esc(a.name)}">`
            + `<div class="fhead">${pkg ? appIcon(a.name) : ''}<span class="li-main"><span class="li-name" title="${esc(a.name)}">${esc(label)}</span>`
            + `<span class="li-sub">${esc(sub)}</span></span>${num(a)}</div>`
            + `<div class="fbar"><i style="width:${(100 * val(a) / top).toFixed(1)}%"></i></div>`
            + `<div class="fdest">${shown}${more}</div></li>`;
    }).join('');
}

function renderApps() {
    const ul = $('appList');
    ul.innerHTML = apps.length
        ? apps.map((p, i) => `<li><span class="li-main"><span class="li-name">${esc(p.split('\\').pop())}</span><span class="li-sub" title="${esc(p)}">${esc(p)}</span></span>`
            + `<button class="icon del x" data-i="${i}" data-tip="${tr('Убрать из списка')}" aria-label="${tr('Убрать')}"><svg><use href="#i-x"/></svg></button></li>`).join('')
        : `<li class="empty">${tr('Список пуст')}</li>`;
    ul.querySelectorAll<HTMLButtonElement>('button.x').forEach(b => b.onclick = () => {
        apps.splice(Number(b.dataset.i), 1);
        settingsDirty = true;
        renderApps();
    });
}

const appIcon = (pkg: string) => `<img class="li-ic app" src="/appicon/${encodeURIComponent(pkg)}" alt="" loading="lazy" onerror="this.style.visibility='hidden'"/>`;
const appLabel = (pkg: string) => appIndex?.find(a => a.pkg === pkg)?.label ?? pkg;

function renderPkgs() {
    const ul = $('pkgList');
    ul.innerHTML = pkgs.length
        ? pkgs.map((p, i) => `<li>${appIcon(p)}<span class="li-main"><span class="li-name">${esc(appLabel(p))}</span><span class="li-sub">${esc(p)}</span></span>`
            + `<button class="icon del x" data-i="${i}" aria-label="${tr('Убрать')}"><svg><use href="#i-x"/></svg></button></li>`).join('')
        : `<li class="empty">${tr('Приложения не выбраны')}</li>`;
    ul.querySelectorAll<HTMLButtonElement>('button.x').forEach(b => b.onclick = () => {
        pkgs.splice(Number(b.dataset.i), 1);
        settingsDirty = true;
        renderPkgs();
    });
    const mode = document.querySelector<HTMLInputElement>('input[name=appmode]:checked')?.value ?? 'all';
    $('pkgList').classList.toggle('hidden', mode === 'all');
    $('btnPickApps').classList.toggle('hidden', mode === 'all');
    $('appModeHint').textContent = tr(mode === 'all' ? 'Все приложения идут через VPN.'
        : mode === 'include' ? 'Через VPN идут только выбранные приложения, остальные — напрямую.'
        : 'Выбранные приложения идут напрямую, все остальные — через VPN.');
}

function fillSettings(s: AnyObj) {
    apps = [...(s.vpn_only_apps ?? [])];
    renderApps();
    pkgs = [...(s.app_packages ?? [])];
    const appMode = s.app_mode || 'all';
    document.querySelectorAll<HTMLInputElement>('input[name=appmode]').forEach(r => r.checked = r.value === appMode);
    if (android) renderPkgs();
    $<HTMLInputElement>('chkIPv6').checked = !!s.block_ipv6;
    $<HTMLInputElement>('chkAutoChange').checked = !!s.auto_change_server;
    $<HTMLInputElement>('chkLockdown').checked = !!s.lockdown_on_drop;
    $<HTMLInputElement>('chkNotify').checked = !!s.notifications;
    $<HTMLInputElement>('chkSubscription').checked = !!s.check_subscription;
    $<HTMLInputElement>('chkTray').checked = !!s.close_to_tray;
    $<HTMLInputElement>('chkKill').checked = !!s.kill_switch;
    $<HTMLInputElement>('chkReconnect').checked = !!s.reconnect;
    $<HTMLInputElement>('chkAuto').checked = !!s.auto_connect;
    $<HTMLSelectElement>('selDNS').value = s.dns_preset || 'provider';
    $<HTMLInputElement>('inpDNS').value = (s.custom_dns ?? []).join(', ');
    $<HTMLInputElement>('inpDNS').disabled = (s.dns_preset || 'provider') !== 'custom';
    $<HTMLInputElement>('inpMTU').value = String(s.mtu ?? 0);
    const mode = s.split?.mode || 'full';
    document.querySelectorAll<HTMLInputElement>('input[name=mode]').forEach(r => r.checked = r.value === mode);
    $<HTMLInputElement>('chkLan').checked = !!s.split?.bypass_lan;
    $<HTMLInputElement>('chkRussia').checked = !!s.split?.bypass_russia;
    $<HTMLTextAreaElement>('txtEntries').value = (s.split?.entries ?? []).join('\n');
}

function syncSkin() {
    const name = state.settings?.skin ?? '';
    if (!skinSaving && (document.documentElement.dataset.skin ?? '') !== name) applySkin(name);
}

function collectSettings(): AnyObj {
    const s = {...(state.settings ?? {})};
    s.kill_switch = $<HTMLInputElement>('chkKill').checked;
    s.block_ipv6 = $<HTMLInputElement>('chkIPv6').checked;
    s.auto_change_server = $<HTMLInputElement>('chkAutoChange').checked;
    s.lockdown_on_drop = $<HTMLInputElement>('chkLockdown').checked;
    s.notifications = $<HTMLInputElement>('chkNotify').checked;
    s.check_subscription = $<HTMLInputElement>('chkSubscription').checked;
    s.close_to_tray = $<HTMLInputElement>('chkTray').checked;
    s.vpn_only_apps = [...apps];
    s.app_packages = [...pkgs];
    s.app_mode = document.querySelector<HTMLInputElement>('input[name=appmode]:checked')?.value ?? 'all';
    s.reconnect = $<HTMLInputElement>('chkReconnect').checked;
    s.auto_connect = $<HTMLInputElement>('chkAuto').checked;
    s.dns_preset = $<HTMLSelectElement>('selDNS').value;
    s.custom_dns = $<HTMLInputElement>('inpDNS').value.split(/[,\s]+/).map(x => x.trim()).filter(Boolean);
    s.mtu = Number($<HTMLInputElement>('inpMTU').value) || 0;
    s.country = $<HTMLSelectElement>('selCountry').value;
    s.key_type = $<HTMLSelectElement>('selKeyType').value;
    const mode = document.querySelector<HTMLInputElement>('input[name=mode]:checked')?.value ?? 'full';
    s.split = {
        mode,
        bypass_lan: $<HTMLInputElement>('chkLan').checked,
        bypass_russia: $<HTMLInputElement>('chkRussia').checked,
        entries: $<HTMLTextAreaElement>('txtEntries').value.split('\n').map(x => x.trim()).filter(Boolean),
    };
    return s;
}

let firstState = true;
async function refresh() {
    try {
        state = await Go.GetState() as AnyObj;
        if (state.lang && state.lang !== lang) { // system language or a choice made elsewhere
            setLang(state.lang);
            location.reload();
            return;
        }
        syncSkin();
        render();
        if (firstState) {
            firstState = false;
            const t = state.tunnel ?? {};
            if (android && state.settings?.auto_connect && state.has_config && !t.up) $('btnPower').click();
        }
        if (!$('tab-stats').classList.contains('hidden')) loadFlows();
        if (!$('tab-logs').classList.contains('hidden')) {
            $('logs').textContent = ((await Go.GetLogs()) ?? []).join('\n');
        }
    } catch (e) {
        console.error(e);
    }
}

// ---------- wiring ----------
document.querySelectorAll<HTMLButtonElement>('.tab[data-tab]').forEach(btn => btn.addEventListener('click', () => {
    document.querySelectorAll('.tab[data-tab]').forEach(b => b.classList.toggle('active', b === btn));
    document.querySelectorAll('.page').forEach(p => p.classList.toggle('hidden', p.id !== `tab-${btn.dataset.tab}`));
    $('pageTitle').textContent = btn.dataset.tip ?? '';
    document.querySelector('.scroll')!.scrollTop = 0;
    if (btn.dataset.tab === 'stats') loadFlows(true);
    refresh();
}));

['chkKill', 'chkIPv6', 'chkTray', 'chkReconnect', 'chkAuto', 'chkAutoChange', 'chkLockdown', 'chkNotify', 'chkSubscription', 'selDNS', 'inpDNS', 'inpMTU', 'chkLan', 'chkRussia', 'txtEntries'].forEach(id =>
    $(id).addEventListener('input', () => {
        settingsDirty = true;
        $<HTMLInputElement>('inpDNS').disabled = $<HTMLSelectElement>('selDNS').value !== 'custom';
    }));
document.querySelectorAll('input[name=mode]').forEach(r => r.addEventListener('change', () => settingsDirty = true));
document.querySelectorAll('input[name=appmode]').forEach(r => r.addEventListener('change', () => {
    settingsDirty = true;
    renderPkgs();
}));

const saveSettings = () => run('Настройки', async () => {
    await Go.SaveSettings(collectSettings() as any);
    settingsDirty = false;
}, 'Сохранено. Изменения применятся при следующем подключении.');
$('btnSaveSettings').onclick = saveSettings;
$('btnSaveSplit').onclick = saveSettings;
['selCountry', 'selKeyType'].forEach(id => $(id).addEventListener('change', () =>
    run('Настройки', () => Go.SaveSettings(collectSettings() as any))));

$('btnQuit').onclick = async () => {
    if (state.tunnel?.up && !await confirmed('Выйти из AWG Pilot?', 'VPN будет отключён.', 'Выйти')) return;
    Go.Quit();
};
$('btnAddApp').onclick = async () => {
    try {
        const p = await Go.PickApp();
        if (p && !apps.some(x => x.toLowerCase() === p.toLowerCase())) {
            apps.push(p);
            settingsDirty = true;
            renderApps();
        }
    } catch (e: any) {
        banner(tf('Выбор программы: {0}', await errText(e)));
    }
};
$('chkAutostart').addEventListener('change', () => {
    const on = $<HTMLInputElement>('chkAutostart').checked;
    run('Автозапуск', () => Go.SetAutostart(on), on ? 'AWG Pilot будет запускаться вместе с Windows' : 'Автозапуск выключен');
});

$('btnPower').onclick = () => {
    if (busy) return;
    const connecting = !state.tunnel?.up;
    document.body.classList.toggle('connecting', connecting);
    run('Подключение', async () => {
        try {
            if (connecting) await Go.Connect(); else await Go.Disconnect();
        } finally {
            document.body.classList.remove('connecting');
        }
    });
};
$('btnDownload').onclick = () => run('Получение конфига', async () => {
    const wasUp = state.tunnel?.up;
    try {
        await Go.DownloadConfig(false);
    } catch (e: any) {
        // Like the official client: the key is bound to other devices up to the limit; offer to move it here.
        if (!/лимит устройств|device limit/.test(String(e?.message ?? e))) throw e;
        if (!await confirmed('Ключ используется на другом устройстве', 'Все места для устройств в подписке заняты. Использовать ключ на этом устройстве? Провайдер перенесёт его сюда, и на другом устройстве VPN может перестать работать.', 'Использовать здесь', true)) throw new Error(tr('отменено — ключ остался на другом устройстве'));
        await Go.DownloadConfig(true);
    }
    if (wasUp && !android) { await Go.Disconnect(); await Go.Connect(); } // Android's core reconnects by itself
}, 'Конфиг обновлён');
const requestOtherServer = async () => {
    if (!await confirmed('Запросить другой сервер?', 'Следующая смена будет доступна только через сутки.', 'Запросить')) return;
    run('Смена сервера', () => Go.RequestOtherServer(), 'Получен другой сервер');
};
$('btnChange').onclick = requestOtherServer;
$('btnAlertAction').onclick = requestOtherServer;
$('btnAlertClose').onclick = () => run('Баннер', () => Go.DismissAlert());
$('btnUnlock').onclick = async () => {
    if (!await confirmed('Разблокировать интернет?', 'Трафик пойдёт напрямую, мимо VPN, пока VPN не подключится.', 'Разблокировать', true)) return;
    run('Разблокировка', () => Go.Unlock(), 'Интернет разблокирован');
};
$('chkWidget').addEventListener('change', () => {
    const on = $<HTMLInputElement>('chkWidget').checked;
    run('Окошко статуса', () => Go.SetWidget(on));
});

$('btnSaveKey').onclick = () => run('Ключ', async () => {
    await Go.AddKey($<HTMLInputElement>('inpKey').value);
    $<HTMLInputElement>('inpKey').value = '';
}, 'Ключ добавлен как профиль');
$('btnDevNew').onclick = async () => {
    if (!await confirmed('Создать новый ID устройства?', 'Провайдер может засчитать его как ещё одно устройство (занять место из лимита).', 'Создать', true)) return;
    run('ID устройства', () => Go.NewDeviceID(), 'Создан новый ID устройства');
};
$('flowList').addEventListener('click', ev => {
    const head = (ev.target as HTMLElement).closest('.fhead');
    const li = head?.closest<HTMLLIElement>('li[data-app]');
    if (!li) return;
    const app = li.dataset.app!;
    if (flowOpen.has(app)) flowOpen.delete(app); else flowOpen.add(app);
    li.classList.toggle('open', flowOpen.has(app));
});
$('flowDay').addEventListener('change', () => loadFlows(true));
document.querySelectorAll<HTMLInputElement>('input[name=flowsort]').forEach(i => i.addEventListener('change', renderFlows));
$('chkFlows').addEventListener('change', () => {
    const on = $<HTMLInputElement>('chkFlows').checked;
    run('Журнал трафика', async () => {
        await Go.SetTrafficLog(on);
        await loadFlows(true);
    }, on ? 'Журнал трафика включён' : 'Журнал трафика выключен');
});
$('btnFlowClear').onclick = async () => {
    if (!await confirmed('Очистить журнал трафика?', 'Записи по программам за все дни будут удалены.', 'Очистить', true)) return;
    run('Журнал трафика', async () => {
        await Go.ClearTrafficLog();
        await loadFlows(true);
    }, 'Журнал очищен');
};
$('btnResetUsage').onclick = async () => {
    if (!await confirmed('Очистить статистику?', 'Вся история трафика будет удалена.', 'Очистить', true)) return;
    run('Статистика', () => Go.ResetUsage(), 'Статистика очищена');
};
// profile actions use event delegation: the list is re-rendered every refresh
$('profList').addEventListener('click', async ev => {
    const btn = (ev.target as HTMLElement).closest<HTMLButtonElement>('button[data-act]');
    if (!btn) return;
    const id = btn.dataset.id!;
    const p = (state.profiles ?? []).find((x: AnyObj) => x.id === id);
    if (!p) return;
    if (btn.dataset.act === 'use') {
        run('Профиль', () => Go.SelectProfile(id), tf('Выбран профиль «{0}»', p.name));
    } else if (btn.dataset.act === 'rename') {
        const name = await ask(tr('Переименовать профиль'), '', {ok: tr('Сохранить'), input: p.name});
        if (name !== null) run('Переименование', () => Go.RenameProfile(id, name));
    } else if (btn.dataset.act === 'delete') {
        if (!await confirmed(tf('Удалить профиль «{0}»?', p.name), 'Ключ и конфиг профиля будут удалены с этого устройства.', 'Удалить', true)) return;
        run('Удаление', () => Go.DeleteProfile(id), 'Профиль удалён');
    }
});
$('btnRefresh').onclick = () => run('Статус подписки', () => Go.RefreshAccount());
$('btnImportFile').onclick = () => run('Импорт', () => Go.ImportConfigFile());
$('btnExport').onclick = () => run('Экспорт', () => Go.ExportConfigFile());
$('btnRebind').onclick = async () => {
    if (!await confirmed('Привязать ключ к этому устройству?', 'Провайдер перепривяжет ключ к этому устройству и выдаст новый конфиг. Другое устройство с этим ключом может перестать работать.', 'Привязать', true)) return;
    run('Перепривязка', () => Go.DownloadConfig(true), 'Ключ привязан к этому устройству, конфиг обновлён');
};

$('btnLang').onclick = async () => {
    const next = lang === 'en' ? 'ru' : 'en';
    try {
        await (window as any).go.main.App.SetLanguage(next);
    } catch (e) {
        banner(await errText(e));
        return;
    }
    setLang(next);
    location.reload();
};

refresh();
window.setInterval(refresh, 2000);

// ---------- Android ----------
if (android) {
    const call = (name: string, ...args: unknown[]) => (window as any).go.main.App[name](...args);
    $('btnSysVpn').onclick = () => call('OpenVpnSettings');

    const picker = $('picker');
    let chosen = new Set<string>();
    const renderPicker = () => {
        const q = $<HTMLInputElement>('pkSearch').value.trim().toLowerCase();
        const sys = $<HTMLInputElement>('pkSystem').checked;
        const list = (appIndex ?? []).filter(a => (sys || !a.system || chosen.has(a.pkg))
            && (!q || a.label.toLowerCase().includes(q) || a.pkg.toLowerCase().includes(q)));
        $('pkAll').innerHTML = list.length ? list.map(a => `<li data-pkg="${esc(a.pkg)}" class="${chosen.has(a.pkg) ? 'active' : ''}">${appIcon(a.pkg)}`
            + `<span class="li-main"><span class="li-name">${esc(a.label)}</span><span class="li-sub">${esc(a.pkg)}</span></span>`
            + `<span class="tick"><svg><use href="#i-check"/></svg></span></li>`).join('')
            : `<li class="empty">${tr('Ничего не найдено')}</li>`;
    };
    $('pkAll').addEventListener('click', ev => {
        const li = (ev.target as HTMLElement).closest<HTMLElement>('li[data-pkg]');
        if (!li) return;
        const pkg = li.dataset.pkg!;
        if (chosen.has(pkg)) chosen.delete(pkg); else chosen.add(pkg);
        li.classList.toggle('active', chosen.has(pkg));
    });
    $('pkSearch').addEventListener('input', renderPicker);
    $('pkSystem').addEventListener('change', renderPicker);
    $('btnPickApps').onclick = async () => {
        chosen = new Set(pkgs);
        picker.classList.remove('hidden');
        $('pkAll').innerHTML = `<li class="empty">${tr('Загрузка списка приложений…')}</li>`;
        try {
            if (!appIndex) appIndex = (await call('ListApps')) as AppInfo[];
            renderPicker();
        } catch (e: any) {
            $('pkAll').innerHTML = `<li class="empty">${esc(e?.message ?? e)}</li>`;
        }
    };
    $('pkCancel').onclick = () => picker.classList.add('hidden');
    $('pkOk').onclick = () => {
        pkgs = [...chosen];
        settingsDirty = true;
        picker.classList.add('hidden');
        renderPkgs();
    };

    // The system back button: close a dialog, then return to the main tab; false lets Android go back.
    (window as any).__awgBack = () => {
        if (!picker.classList.contains('hidden')) { picker.classList.add('hidden'); return true; }
        if (!$('modal').classList.contains('hidden')) { $('mCancel').click(); return true; }
        if (!$('speedSheet').classList.contains('hidden')) { $('stClose').click(); return true; }
        const home = document.querySelector<HTMLButtonElement>('.tab[data-tab=connect]')!;
        if (!home.classList.contains('active')) { home.click(); return true; }
        return false;
    };
}

// ---------- speed test ----------
let speedPoll = 0;
const mbit = (v: number) => v >= 100 ? v.toFixed(0) : v >= 10 ? v.toFixed(1) : v.toFixed(2);
function renderSpeedTest(st: AnyObj | undefined) {
    const sheet = $('speedSheet').querySelector('.sheet')!;
    const running = !!st?.running;
    sheet.classList.toggle('running', running);
    const phaseName: AnyObj = {connect: 'Подключение к серверу теста…', connected: 'Измеряю загрузку…', download: 'Измеряю загрузку…', upload: 'Измеряю отдачу…'};
    const countries: AnyObj = {FI: 'Финляндия', NL: 'Нидерланды', DE: 'Германия', SE: 'Швеция', EE: 'Эстония', LV: 'Латвия', LT: 'Литва', PL: 'Польша',
        FR: 'Франция', GB: 'Великобритания', US: 'США', KZ: 'Казахстан', TR: 'Турция', AM: 'Армения', GE: 'Грузия', RU: 'Россия'};
    $('stExit').innerHTML = st?.ip ? tf('Замер шёл с адреса <b>{0}</b>', esc(st.ip)) + (st.loc ? ` · ${esc(tr(countries[st.loc] ?? st.loc))}` : '')
        + tr(st.loc === 'RU' ? ' — похоже, мимо VPN' : ' — это выход VPN, не ваш домашний адрес') : '';
    $('stPhase').textContent = running ? tr(phaseName[st!.phase] ?? '…')
        : st?.error ? tf('Ошибка: {0}', st.error) : st?.at && !String(st.at).startsWith('0001') ? tr('Готово') : '';
    $('stNow').textContent = running ? (st!.phase === 'connect' || st!.phase === 'connected' ? '…' : mbit(st!.mbps ?? 0))
        : st?.down_mbps ? mbit(st.down_mbps) : '—';
    const down = running && st!.phase === 'download' ? st!.mbps : st?.down_mbps;
    const up = running && st!.phase === 'upload' ? st!.mbps : st?.up_mbps;
    $('stDown').textContent = down ? tf('{0} Мбит/с', mbit(down)) : running && st!.phase !== 'connect' ? '…' : '—';
    $('stUp').textContent = up ? tf('{0} Мбит/с', mbit(up)) : '—';
    const ping = state.speed?.ping_ms;
    $('stPing').textContent = ping >= 0 ? tf('{0} мс', ping) : '—';
    $('stDown').parentElement!.classList.toggle('now', running && st!.phase === 'download');
    $('stUp').parentElement!.classList.toggle('now', running && st!.phase === 'upload');
    ($('stAgain') as HTMLButtonElement).disabled = running;
}
async function pollSpeed() {
    window.clearTimeout(speedPoll);
    try {
        state = await Go.GetState() as AnyObj;
    } catch { /* keep polling */ }
    renderSpeedTest(state.speedtest);
    if (state.speedtest?.running && !$('speedSheet').classList.contains('hidden')) speedPoll = window.setTimeout(pollSpeed, 400);
}
async function startSpeedTest() {
    $('speedSheet').classList.remove('hidden');
    try {
        await (window as any).go.main.App.SpeedTest(); // via window.go: works before the bindings are regenerated
    } catch (e: any) {
        renderSpeedTest({error: e?.message ?? String(e), at: new Date().toISOString()});
        return;
    }
    pollSpeed();
}
$('btnSpeed').onclick = startSpeedTest;
$('stAgain').onclick = startSpeedTest;
$('stClose').onclick = () => { $('speedSheet').classList.add('hidden'); window.clearTimeout(speedPoll); };
