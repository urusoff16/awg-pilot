package com.awgpilot.app;

import android.content.Intent;
import android.content.pm.ApplicationInfo;
import android.content.pm.PackageManager;
import android.provider.Settings;
import android.webkit.JavascriptInterface;

import org.json.JSONArray;
import org.json.JSONObject;

import java.util.ArrayList;
import java.util.Collections;
import java.util.List;

/**
 * window.AWGAndroid in the page. Every call is answered asynchronously through
 * window.__awgReply(id, json); most methods go straight to the Go core, a few need Android UI.
 */
final class Bridge {
    private final MainActivity act;

    Bridge(MainActivity act) {
        this.act = act;
    }

    @JavascriptInterface
    public void call(int id, String method, String args) {
        Core.IO.execute(() -> dispatch(id, method, args));
    }

    private void dispatch(int id, String method, String args) {
        try {
            switch (method) {
                case "Connect":
                    act.withVpnConsent(granted -> Core.IO.execute(() -> act.reply(id, granted
                            ? Core.call("Connect")
                            : err(Core.t("Без разрешения Android на VPN подключиться нельзя", "Without Android's VPN permission the app cannot connect")))));
                    return;
                case "ImportConfigFile":
                    act.pickConfig((text, name) -> Core.IO.execute(() -> act.reply(id, text == null ? ok()
                            : Core.call("ImportConfigText", new JSONArray().put(text).put(name).toString()))));
                    return;
                case "ExportConfigFile": {
                    JSONObject r = new JSONObject(Core.call("ExportConfig"));
                    if (!r.optBoolean("ok")) {
                        act.reply(id, r.toString());
                        return;
                    }
                    JSONObject cfg = r.getJSONObject("result");
                    act.saveConfig(cfg.optString("name", "awgpilot") + ".conf", cfg.getString("text"),
                            error -> act.reply(id, error == null ? ok() : err(error)));
                    return;
                }
                case "OpenVpnSettings":
                    act.runOnUiThread(() -> act.startActivity(new Intent(Settings.ACTION_VPN_SETTINGS)));
                    act.reply(id, ok());
                    return;
                case "ListApps":
                    act.reply(id, new JSONObject().put("ok", true).put("result", listApps()).toString());
                    return;
                case "PickApp":
                case "SetAutostart":
                case "SetWidget":
                case "Quit":
                case "ShowWindow":
                    act.reply(id, ok()); // desktop-only features
                    return;
                case "SetLanguage": {
                    String r = Core.call(method, args);
                    Core.setLang(r);
                    act.reply(id, r);
                    return;
                }
                default:
                    act.reply(id, Core.call(method, args));
            }
        } catch (Exception e) {
            act.reply(id, err(String.valueOf(e.getMessage())));
        }
    }

    private JSONArray listApps() throws Exception {
        PackageManager pm = act.getPackageManager();
        List<ApplicationInfo> apps = pm.getInstalledApplications(0);
        List<JSONObject> out = new ArrayList<>();
        for (ApplicationInfo ai : apps) {
            if (ai.packageName.equals(act.getPackageName())) continue;
            if (pm.checkPermission(android.Manifest.permission.INTERNET, ai.packageName) != PackageManager.PERMISSION_GRANTED) continue;
            boolean system = (ai.flags & ApplicationInfo.FLAG_SYSTEM) != 0 && (ai.flags & ApplicationInfo.FLAG_UPDATED_SYSTEM_APP) == 0
                    && pm.getLaunchIntentForPackage(ai.packageName) == null;
            out.add(new JSONObject().put("pkg", ai.packageName).put("label", String.valueOf(ai.loadLabel(pm))).put("system", system));
        }
        Collections.sort(out, (a, b) -> a.optString("label").compareToIgnoreCase(b.optString("label")));
        return new JSONArray(out);
    }

    static String ok() {
        return "{\"ok\":true,\"result\":null}";
    }

    static String err(String msg) {
        try {
            return new JSONObject().put("ok", false).put("error", msg).toString();
        } catch (Exception e) {
            return "{\"ok\":false,\"error\":\"error\"}";
        }
    }
}
