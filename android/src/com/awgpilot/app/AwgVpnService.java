package com.awgpilot.app;

import android.app.NotificationManager;
import android.content.Context;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.net.ConnectivityManager;
import android.net.IpPrefix;
import android.net.Network;
import android.net.NetworkCapabilities;
import android.net.NetworkRequest;
import android.net.VpnService;
import android.os.Build;
import android.os.Handler;
import android.os.Looper;
import android.os.ParcelFileDescriptor;
import android.util.Log;

import org.json.JSONArray;
import org.json.JSONObject;

import java.net.InetAddress;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;

/**
 * Owns the Android VPN interface. The Go core decides when to connect; it calls
 * establishFromCore() with a plan and runs the AmneziaWG device on the returned descriptor.
 */
public final class AwgVpnService extends VpnService {
    static final String ACTION_CONNECT = "com.awgpilot.app.CONNECT";
    static final String ACTION_DISCONNECT = "com.awgpilot.app.DISCONNECT";
    private static final String ACTION_HOLD = "com.awgpilot.app.HOLD";

    private static volatile AwgVpnService instance;
    private static volatile CountDownLatch started = new CountDownLatch(1);

    private final Handler main = new Handler(Looper.getMainLooper());
    private ConnectivityManager.NetworkCallback netCb;
    private boolean statusShown;
    private final Runnable statusTick = new Runnable() {
        @Override
        public void run() {
            Core.IO.execute(AwgVpnService.this::updateStatus);
            main.postDelayed(this, 2000);
        }
    };

    // ---- called by the core (any thread) --------------------------------------------------

    static String establishFromCore(Context ctx, String planJson) {
        try {
            if (VpnService.prepare(ctx) != null) return "err:Нет разрешения на VPN — откройте AWG Pilot и нажмите кнопку подключения";
            AwgVpnService s = instance;
            if (s == null) {
                started = new CountDownLatch(1);
                ctx.startService(new Intent(ctx, AwgVpnService.class).setAction(ACTION_HOLD));
                started.await(5, TimeUnit.SECONDS);
                s = instance;
                if (s == null) return "err:служба VPN не запустилась";
            }
            return s.establish(new JSONObject(planJson));
        } catch (Exception e) {
            Log.e(Core.TAG, "establish", e);
            return "err:" + e.getMessage();
        }
    }

    static void releaseFromCore(Context ctx) {
        AwgVpnService s = instance;
        if (s != null) s.main.post(s::shutdown);
    }

    private String establish(JSONObject p) throws Exception {
        Builder b = new Builder();
        b.setSession(p.optString("session", "AWG Pilot"));
        b.setMtu(p.optInt("mtu", 1280));
        for (String a : strings(p, "addresses")) {
            String[] ip = a.split("/");
            b.addAddress(ip[0], Integer.parseInt(ip[1]));
        }
        for (String d : strings(p, "dns")) b.addDnsServer(d);
        for (String r : strings(p, "routes")) {
            String[] ip = r.split("/");
            b.addRoute(ip[0], Integer.parseInt(ip[1]));
        }
        if (Build.VERSION.SDK_INT >= 33) {
            for (String r : strings(p, "exclude_routes")) {
                String[] ip = r.split("/");
                b.excludeRoute(new IpPrefix(InetAddress.getByName(ip[0]), Integer.parseInt(ip[1])));
            }
        }
        // The app itself always bypasses its VPN: provider requests and the tunnel's own UDP
        // socket must keep working while the server is dead.
        String mode = p.optString("app_mode", "all");
        if (mode.equals("include")) {
            int added = 0;
            for (String pkg : strings(p, "apps")) {
                try {
                    b.addAllowedApplication(pkg);
                    added++;
                } catch (PackageManager.NameNotFoundException ignored) {
                }
            }
            if (added == 0) return "err:ни одно из выбранных приложений не установлено";
        } else {
            b.addDisallowedApplication(getPackageName());
            if (mode.equals("exclude")) {
                for (String pkg : strings(p, "apps")) {
                    try {
                        b.addDisallowedApplication(pkg);
                    } catch (PackageManager.NameNotFoundException ignored) {
                    }
                }
            }
        }
        if (Build.VERSION.SDK_INT >= 29) b.setMetered(false);
        b.setBlocking(true);
        b.setConfigureIntent(Notifier.openApp(this));
        ParcelFileDescriptor pfd = b.establish();
        if (pfd == null) return "err:Android не разрешил VPN — включите его в приложении ещё раз";
        main.post(this::startStatus);
        return "fd:" + pfd.detachFd();
    }

    private static String[] strings(JSONObject p, String key) {
        JSONArray a = p.optJSONArray(key);
        if (a == null) return new String[0];
        String[] out = new String[a.length()];
        for (int i = 0; i < out.length; i++) out[i] = a.optString(i);
        return out;
    }

    // ---- service lifecycle ----------------------------------------------------------------

    @Override
    public void onCreate() {
        super.onCreate();
        instance = this;
        watchNetwork();
        started.countDown();
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        String action = intent == null ? null : intent.getAction();
        if (action == null || SERVICE_INTERFACE.equals(action) || ACTION_CONNECT.equals(action)) {
            // Always-on VPN (boot, system settings) or the quick tile: connect with saved settings.
            Core.IO.execute(() -> Core.call("Connect"));
        } else if (ACTION_DISCONNECT.equals(action)) {
            Core.IO.execute(() -> Core.call("Disconnect"));
        }
        return START_NOT_STICKY;
    }

    @Override
    public void onRevoke() {
        Core.IO.execute(() -> Core.call("Revoked"));
        shutdown();
    }

    @Override
    public void onDestroy() {
        main.removeCallbacks(statusTick);
        if (netCb != null) {
            try {
                ((ConnectivityManager) getSystemService(CONNECTIVITY_SERVICE)).unregisterNetworkCallback(netCb);
            } catch (Exception ignored) {
            }
        }
        if (instance == this) instance = null;
        super.onDestroy();
    }

    private void shutdown() {
        main.removeCallbacks(statusTick);
        leaveForeground();
        ((NotificationManager) getSystemService(NOTIFICATION_SERVICE)).cancel(Notifier.STATUS_ID);
        statusShown = false;
        refreshTile(this);
        stopSelf();
    }

    // ---- status notification and network changes ------------------------------------------

    private boolean foreground;

    /**
     * Android 8+ may stop a service that was started in the background (always-on VPN at boot)
     * unless it runs in the foreground, which requires a visible notification while connected.
     */
    private void enterForeground() {
        if (foreground) return;
        try {
            JSONObject b = new JSONObject().put("title", "Подключение…");
            android.app.Notification n = Notifier.status(this, b);
            if (Build.VERSION.SDK_INT >= 34) {
                startForeground(Notifier.STATUS_ID, n, android.content.pm.ServiceInfo.FOREGROUND_SERVICE_TYPE_SYSTEM_EXEMPTED);
            } else {
                startForeground(Notifier.STATUS_ID, n);
            }
            foreground = true;
            statusShown = true;
        } catch (Throwable t) {
            // the plain notification still works; only the extra protection is missing
            Log.w(Core.TAG, "startForeground", t);
        }
    }

    @SuppressWarnings("deprecation")
    private void leaveForeground() {
        if (!foreground) return;
        foreground = false;
        if (Build.VERSION.SDK_INT >= 24) stopForeground(STOP_FOREGROUND_REMOVE);
        else stopForeground(true);
    }

    private void startStatus() {
        enterForeground();
        main.removeCallbacks(statusTick);
        main.post(statusTick);
        refreshTile(this);
    }

    private void updateStatus() {
        try {
            JSONObject b = new JSONObject(Core.call("Brief")).optJSONObject("result");
            NotificationManager nm = (NotificationManager) getSystemService(NOTIFICATION_SERVICE);
            boolean up = b != null && b.optBoolean("up");
            // While in the foreground the notification must stay, whatever the setting says.
            if (!up || (!foreground && !b.optBoolean("notify", true))) {
                if (statusShown && !foreground) nm.cancel(Notifier.STATUS_ID);
                if (!foreground) statusShown = false;
                return;
            }
            nm.notify(Notifier.STATUS_ID, Notifier.status(this, b));
            statusShown = true;
        } catch (Exception e) {
            Log.w(Core.TAG, "status", e);
        }
    }

    private void watchNetwork() {
        ConnectivityManager cm = (ConnectivityManager) getSystemService(CONNECTIVITY_SERVICE);
        netCb = new ConnectivityManager.NetworkCallback() {
            private Network last;
            private boolean lost;

            @Override
            public void onAvailable(Network n) {
                // a switch (Wi-Fi to mobile and back) or the network returning after none at all
                if (lost || (last != null && !last.equals(n))) Core.IO.execute(() -> Core.call("NetworkChanged"));
                last = n;
                lost = false;
            }

            @Override
            public void onLost(Network n) {
                if (!n.equals(last)) return; // an old network going away after a switch
                lost = true;
                Core.IO.execute(() -> Core.call("NetworkLost"));
            }
        };
        try {
            if (Build.VERSION.SDK_INT >= 24) {
                cm.registerDefaultNetworkCallback(netCb);
            } else {
                cm.registerNetworkCallback(new NetworkRequest.Builder()
                        .addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
                        .addCapability(NetworkCapabilities.NET_CAPABILITY_NOT_VPN).build(), netCb);
            }
        } catch (Exception e) {
            netCb = null;
        }
    }

    /**
     * Asks the quick settings tile to update. QsTile extends TileService (Android 7+), so on older
     * systems it must not even be resolved: the class is named by string, behind a version check.
     */
    static void refreshTile(android.content.Context ctx) {
        if (Build.VERSION.SDK_INT >= 24) {
            android.service.quicksettings.TileService.requestListeningState(ctx,
                    new android.content.ComponentName(ctx.getPackageName(), "com.awgpilot.app.QsTile"));
        }
    }

    static boolean running() {
        return instance != null;
    }
}
