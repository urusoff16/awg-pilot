// AWG Pilot for Chrome: the listed sites go through the user's own server (an HTTP proxy with a
// login), everything else directly. Optionally the listed sites get a fake location (Amsterdam)
// and WebRTC is kept inside the proxy.
import { load } from './common.js';

const GEO_ID = 'awg-geo';

function pac(s) {
  const list = JSON.stringify(s.domains);
  return `function FindProxyForURL(url, host) {
  host = host.toLowerCase();
  var list = ${list};
  for (var i = 0; i < list.length; i++) {
    var d = list[i];
    if (host === d || dnsDomainIs(host, "." + d)) return "PROXY ${s.host}:${Number(s.port)}";
  }
  return "DIRECT";
}`;
}

async function apply() {
  const s = await load();
  const on = s.enabled && s.host && s.user && s.pass && s.domains.length > 0;
  if (on) {
    await chrome.proxy.settings.set({ value: { mode: 'pac_script', pacScript: { data: pac(s) } }, scope: 'regular' });
  } else {
    await chrome.proxy.settings.clear({ scope: 'regular' });
  }
  if (on && s.webrtc) {
    await chrome.privacy.network.webRTCIPHandlingPolicy.set({ value: 'disable_non_proxied_udp' });
  } else {
    await chrome.privacy.network.webRTCIPHandlingPolicy.clear({});
  }
  try { await chrome.scripting.unregisterContentScripts({ ids: [GEO_ID] }); } catch (_) { /* not registered */ }
  if (on && s.geo) {
    const matches = s.domains.flatMap(d => [`*://${d}/*`, `*://*.${d}/*`]);
    await chrome.scripting.registerContentScripts([{
      id: GEO_ID, js: ['geo.js'], matches, runAt: 'document_start', world: 'MAIN', allFrames: true,
    }]);
  }
  chrome.action.setBadgeText({ text: on ? 'ON' : '' });
  chrome.action.setBadgeBackgroundColor({ color: '#2f9e78' });
}

// The proxy asks for the login: answer with the saved one (only to our own proxy).
chrome.webRequest.onAuthRequired.addListener((details, callback) => {
  load().then(s => {
    if (details.isProxy && s.enabled && details.challenger && details.challenger.host === s.host) {
      callback({ authCredentials: { username: s.user, password: s.pass } });
    } else {
      callback({});
    }
  });
}, { urls: ['<all_urls>'] }, ['asyncBlocking']);

chrome.proxy.onProxyError.addListener(e => console.warn('AWG Pilot proxy error:', e.error));
chrome.runtime.onInstalled.addListener(apply);
chrome.runtime.onStartup.addListener(apply);
chrome.storage.onChanged.addListener(apply);
