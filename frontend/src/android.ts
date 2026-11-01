// Android: the page runs in a WebView and talks to the Go core through the Java bridge
// (window.AWGAndroid). This installs a window.go.main.App that looks like the Wails binding,
// so main.ts works unchanged; calls are async and Java answers through window.__awgReply.
type Bridge = { call(id: number, method: string, args: string): void };

export function installAndroidBridge() {
    const bridge = (window as any).AWGAndroid as Bridge;
    const pending = new Map<number, {resolve: (v: any) => void, reject: (e: Error) => void}>();
    let next = 1;
    (window as any).__awgReply = (id: number, json: string) => {
        const p = pending.get(id);
        if (!p) return;
        pending.delete(id);
        let r: any;
        try { r = JSON.parse(json); } catch { r = {ok: false, error: 'invalid core reply'}; }
        if (r.ok) p.resolve(r.result); else p.reject(new Error(r.error));
    };
    const call = (method: string, args: unknown[]) => new Promise((resolve, reject) => {
        const id = next++;
        pending.set(id, {resolve, reject});
        bridge.call(id, method, JSON.stringify(args));
    });
    const App = new Proxy({}, {get: (_t, name) => (...args: unknown[]) => call(String(name), args)});
    (window as any).go = {main: {App}};
    document.body.classList.add('android');
}
