// Settings shared by the service worker, the popup and the options page.
export const DEFAULTS = {
  enabled: false,
  host: '',
  port: 8880,
  user: '',
  pass: '',
  // Sites that go through the server; each also covers its subdomains.
  domains: [
    'google.com', 'gemini.google', 'googleapis.com', 'gstatic.com', 'googleusercontent.com', 'withgoogle.com',
    'chatgpt.com', 'openai.com', 'oaiusercontent.com', 'oaistatic.com',
    'claude.ai', 'anthropic.com', 'claudeusercontent.com',
  ],
  geo: true,     // report Amsterdam to the sites above instead of the real location
  webrtc: true,  // no WebRTC traffic outside the proxy (it would show the real address)
};

export async function load() {
  const s = await chrome.storage.local.get(DEFAULTS);
  return { ...DEFAULTS, ...s };
}

// normalize turns user input ("https://Gemini.Google.com/app", "*.foo.com") into a domain.
export function normalize(d) {
  d = String(d).trim().toLowerCase().replace(/^[a-z]+:\/\//, '').replace(/[/:?#].*$/, '').replace(/^\*\./, '').replace(/^\.+|\.+$/g, '');
  return /^[a-z0-9.-]+$/.test(d) && d.includes('.') || /^[a-z0-9-]+$/.test(d) ? d : '';
}
