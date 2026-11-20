import { load } from './common.js';

const $ = id => document.getElementById(id);

async function render() {
  const s = await load();
  const ready = s.host && s.user && s.pass;
  $('on').checked = s.enabled && !!ready;
  $('on').disabled = !ready;
  $('label').textContent = $('on').checked ? 'Включено' : 'Выключено';
  $('st').innerHTML = !ready
    ? 'Укажите сервер, логин и пароль в настройках.'
    : $('on').checked
      ? `Через сервер <b>${s.host}</b>: ${s.domains.length} сайтов и их поддомены.` + (s.geo ? '<br>Геолокация для них — Амстердам.' : '') + (s.webrtc ? '<br>WebRTC только через прокси.' : '')
      : 'Все сайты открываются напрямую.';
}

$('on').addEventListener('change', async () => {
  await chrome.storage.local.set({ enabled: $('on').checked });
  render();
});
$('opts').addEventListener('click', () => chrome.runtime.openOptionsPage());
render();
