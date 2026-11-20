// Runs in the page itself, before its scripts: the geolocation API reports central Amsterdam,
// as it would for someone browsing from the Netherlands. Only on the sites routed via the server.
(() => {
  if (!navigator.geolocation || navigator.geolocation.__awg) return;
  const pos = () => ({
    coords: { latitude: 52.3676, longitude: 4.9041, accuracy: 30, altitude: null, altitudeAccuracy: null, heading: null, speed: null },
    timestamp: Date.now(),
  });
  const geo = navigator.geolocation;
  let id = 0;
  geo.getCurrentPosition = function (ok) { setTimeout(() => ok && ok(pos()), 50); };
  geo.watchPosition = function (ok) { setTimeout(() => ok && ok(pos()), 50); return ++id; };
  geo.clearWatch = function () {};
  Object.defineProperty(geo, '__awg', { value: true });
  // A site that asks first whether it may use the location hears "granted": no prompt, no real data.
  if (navigator.permissions && navigator.permissions.query) {
    const query = navigator.permissions.query.bind(navigator.permissions);
    navigator.permissions.query = d => (d && d.name === 'geolocation')
      ? Promise.resolve({ state: 'granted', status: 'granted', onchange: null, addEventListener() {}, removeEventListener() {} })
      : query(d);
  }
})();
