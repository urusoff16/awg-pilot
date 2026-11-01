package com.awgpilot.app;

import android.annotation.TargetApi;
import android.app.PendingIntent;
import android.content.Context;
import android.content.Intent;
import android.net.VpnService;
import android.os.Build;
import android.service.quicksettings.Tile;
import android.service.quicksettings.TileService;

import org.json.JSONObject;

/** Quick settings tile: one tap turns the VPN on or off (Android 7+). */
@TargetApi(24)
public final class QsTile extends TileService {

    static void refresh(Context ctx) {
        AwgVpnService.refreshTile(ctx);
    }

    @Override
    public void onStartListening() {
        Core.IO.execute(() -> {
            boolean up = false;
            String sub = null;
            try {
                JSONObject b = new JSONObject(Core.call("Brief")).optJSONObject("result");
                if (b != null) {
                    up = b.optBoolean("up");
                    sub = up ? b.optString("server", null) : null;
                }
            } catch (Exception ignored) {
            }
            boolean on = up;
            String subtitle = sub;
            Tile t = getQsTile();
            if (t == null) return;
            t.setState(on ? Tile.STATE_ACTIVE : Tile.STATE_INACTIVE);
            t.setLabel(getString(R.string.app_name));
            if (Build.VERSION.SDK_INT >= 29) t.setSubtitle(on ? subtitle : Core.t("Выключен", "Off"));
            t.updateTile();
        });
    }

    @Override
    public void onClick() {
        Tile t = getQsTile();
        boolean on = t != null && t.getState() == Tile.STATE_ACTIVE;
        if (on) {
            Core.IO.execute(() -> {
                Core.call("Disconnect");
                refresh(this);
            });
            return;
        }
        if (VpnService.prepare(this) != null) {
            openApp(); // the system consent dialog needs an activity
            return;
        }
        if (t != null) {
            t.setState(Tile.STATE_ACTIVE);
            t.updateTile();
        }
        Core.IO.execute(() -> {
            Core.call("Connect");
            refresh(this);
        });
    }

    @SuppressWarnings("deprecation")
    private void openApp() {
        Intent i = new Intent(this, MainActivity.class).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
        if (Build.VERSION.SDK_INT >= 34) {
            startActivityAndCollapse(PendingIntent.getActivity(this, 2, i, PendingIntent.FLAG_IMMUTABLE));
        } else {
            startActivityAndCollapse(i);
        }
    }
}
