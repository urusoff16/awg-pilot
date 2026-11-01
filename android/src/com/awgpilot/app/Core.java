package com.awgpilot.app;

import android.content.Context;
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
