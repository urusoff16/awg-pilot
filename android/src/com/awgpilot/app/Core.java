package com.awgpilot.app;

import android.content.Context;
import android.net.ConnectivityManager;
import android.net.Network;
import android.net.NetworkCapabilities;
import android.net.NetworkRequest;
import android.os.Build;
import android.util.Log;

import java.util.Locale;
import java.util.TimeZone;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

/** Bridge to the Go core (libawgpilot.so). The core calls back into the static methods below. */
public final class Core {
    static final String TAG = "AWGPilot";
    static final ExecutorService IO = Executors.newCachedThreadPool();
    private static Context ctx;
    /** The interface language is English (anything but a Russian system, unless chosen in the app). */
    static volatile boolean english;

    static {
        System.loadLibrary("awgpilot");
    }

    private Core() {}

    private static native String nativeInit(String dir, String keyHex, String tz, int sdk);

    private static native String nativeCall(String method, String argsJson);

    static synchronized void init(Context c) {
        if (ctx != null) return;
        ctx = c.getApplicationContext();
        String err = nativeInit(ctx.getFilesDir().getAbsolutePath(), Keys.dataKeyHex(ctx),
                TimeZone.getDefault().getID(), Build.VERSION.SDK_INT);
        if (!err.isEmpty()) Log.e(TAG, "core init: " + err);
        setLang(call("SetSystemLanguage", "[\"" + Locale.getDefault().toLanguageTag() + "\"]"));
        watchNetKind();
    }

    /**
     * Tells the core what kind of network the phone is on (Wi-Fi, mobile, cable) whenever it
     * changes: subscriptions choose their servers by it. The app is outside its own VPN, so its
     * default network is the physical one.
     */
    private static void watchNetKind() {
        final ConnectivityManager cm = (ConnectivityManager) ctx.getSystemService(Context.CONNECTIVITY_SERVICE);
        ConnectivityManager.NetworkCallback cb = new ConnectivityManager.NetworkCallback() {
            @Override
            public void onAvailable(Network n) {
                report(kindOf(cm.getNetworkCapabilities(n)));
            }

            @Override
            public void onCapabilitiesChanged(Network n, NetworkCapabilities caps) {
                report(kindOf(caps));
            }

            @Override
            public void onLost(Network n) {
                report("");
            }
        };
        try {
            if (Build.VERSION.SDK_INT >= 24) {
                cm.registerDefaultNetworkCallback(cb);
            } else {
                cm.registerNetworkCallback(new NetworkRequest.Builder()
                        .addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
                        .addCapability(NetworkCapabilities.NET_CAPABILITY_NOT_VPN).build(), cb);
                if (Build.VERSION.SDK_INT >= 23) {
                    Network n = cm.getActiveNetwork();
                    if (n != null) report(kindOf(cm.getNetworkCapabilities(n)));
                }
            }
        } catch (Exception e) {
            Log.w(TAG, "network kind", e);
        }
    }

    private static volatile String lastKind;

    private static void report(String kind) {
        if (kind.equals(lastKind)) return;
        lastKind = kind;
        IO.execute(() -> call("SetNetKind", "[\"" + kind + "\"]"));
    }

    static String kindOf(NetworkCapabilities c) {
        if (c == null) return "";
        if (c.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR)) return "mobile";
        if (c.hasTransport(NetworkCapabilities.TRANSPORT_WIFI)) return "wifi";
        if (c.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET)) return "wired";
        if (Build.VERSION.SDK_INT >= 31 && c.hasTransport(NetworkCapabilities.TRANSPORT_USB)) return "wired";
        return "";
    }

    /** Takes the language from a core reply ({"ok":true,"result":"en"}) and renames the channels. */
    static void setLang(String reply) {
        english = reply != null && reply.contains("\"result\":\"en\"");
        Notifier.createChannels(ctx);
    }

    /** Picks the text for the interface language. */
    static String t(String ru, String en) {
        return english ? en : ru;
    }

    /** Runs a core method; blocks on network calls, so never call it on the main thread. */
    static String call(String method, String argsJson) {
        return nativeCall(method, argsJson == null ? "[]" : argsJson);
    }

    static String call(String method) {
        return call(method, "[]");
    }

    // ---- called from Go -------------------------------------------------------------------

    /** Builds the VPN interface; returns "fd:N" or "err:message". */
    static String establish(String planJson) {
        return AwgVpnService.establishFromCore(ctx, planJson);
    }

    static void release() {
        AwgVpnService.releaseFromCore(ctx);
    }

    static void notify(String title, String body) {
        Notifier.event(ctx, title, body);
    }

    /** Package name of the app behind a tunnel connection, "" when unknown. */
    static String owner(int proto, String local, int lport, String remote, int rport) {
        return Owners.find(ctx, proto, local, lport, remote, rport);
    }
}
