import { load, normalize } from './common.js';

const $ = id => document.getElementById(id);

load().then(s => {
  $('host').value = s.host;
  $('port').value = s.port;
  $('user').value = s.user;
  $('pass').value = s.pass;
  $('domains').value = s.domains.join('\n');
  $('geo').checked = s.geo;
  $('webrtc').checked = s.webrtc;
});

$('save').addEventListener('click', async () => {
  const domains = [...new Set($('domains').value.split('\n').map(normalize).filter(Boolean))];
  await chrome.storage.local.set({
    host: $('host').value.trim(),
    port: Number($('port').value) || 8880,
    user: $('user').value.trim(),
    pass: $('pass').value,
    domains,
    geo: $('geo').checked,
    webrtc: $('webrtc').checked,
  });
  $('domains').value = domains.join('\n');
  $('saved').textContent = 'Сохранено';
  setTimeout(() => $('saved').textContent = '', 2500);
});
