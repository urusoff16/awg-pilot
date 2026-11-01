package com.awgpilot.app;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.content.Context;
import android.content.Intent;
import android.os.Build;

import org.json.JSONObject;

import java.util.Locale;

/** Event notifications (connection lost, restored...) and the ongoing status with speed. */
final class Notifier {
    static final String EVENTS = "events";
    static final String STATUS = "status";
    static final int STATUS_ID = 1;
    private static int nextId = 100;

    private Notifier() {}

    static void createChannels(Context ctx) {
        if (Build.VERSION.SDK_INT < 26) return;
        NotificationManager nm = ctx.getSystemService(NotificationManager.class);
        NotificationChannel ev = new NotificationChannel(EVENTS, "События VPN", NotificationManager.IMPORTANCE_DEFAULT);
        ev.setDescription("Потеря связи, восстановление, подписка");
        NotificationChannel st = new NotificationChannel(STATUS, "Состояние VPN", NotificationManager.IMPORTANCE_LOW);
        st.setDescription("Скорость и сервер, пока VPN включён");
        st.setShowBadge(false);
        nm.createNotificationChannel(ev);
        nm.createNotificationChannel(st);
    }

    private static Notification.Builder builder(Context ctx, String channel) {
        Notification.Builder b = Build.VERSION.SDK_INT >= 26 ? new Notification.Builder(ctx, channel) : new Notification.Builder(ctx);
        return b.setSmallIcon(R.drawable.ic_stat)
                .setColor(0xFF3FB68B)
                .setContentIntent(openApp(ctx));
    }

    static PendingIntent openApp(Context ctx) {
        Intent i = new Intent(ctx, MainActivity.class).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK | Intent.FLAG_ACTIVITY_SINGLE_TOP);
        return PendingIntent.getActivity(ctx, 0, i, PendingIntent.FLAG_UPDATE_CURRENT | PendingIntent.FLAG_IMMUTABLE);
    }

    static void event(Context ctx, String title, String body) {
        NotificationManager nm = (NotificationManager) ctx.getSystemService(Context.NOTIFICATION_SERVICE);
        Notification n = builder(ctx, EVENTS).setContentTitle(title).setContentText(body)
                .setStyle(new Notification.BigTextStyle().bigText(body)).setAutoCancel(true).build();
        nm.notify(nextId++, n);
    }

    /** Builds the ongoing status from the core's Brief JSON. */
    static Notification status(Context ctx, JSONObject b) {
        String title = b.optString("title", "AWG Pilot");
        String text;
        if (b.optBoolean("healthy")) {
            long ping = b.optLong("ping_ms", -1);
            text = String.format(Locale.ROOT, "↓ %s  ↑ %s%s", rate(b.optDouble("rx_bps")), rate(b.optDouble("tx_bps")),
                    ping >= 0 ? "  ·  " + ping + " мс" : "");
        } else {
            text = b.optString("server", "");
        }
        String profile = b.optString("profile", "");
        Intent off = new Intent(ctx, AwgVpnService.class).setAction(AwgVpnService.ACTION_DISCONNECT);
        PendingIntent offPi = PendingIntent.getService(ctx, 1, off, PendingIntent.FLAG_UPDATE_CURRENT | PendingIntent.FLAG_IMMUTABLE);
        Notification.Builder nb = builder(ctx, STATUS).setContentTitle(title).setContentText(text)
                .setOngoing(true).setOnlyAlertOnce(true).setShowWhen(false)
                .addAction(action("Отключить", offPi));
        if (!profile.isEmpty()) nb.setSubText(profile);
        if (Build.VERSION.SDK_INT < 26) nb.setPriority(Notification.PRIORITY_LOW);
        return nb.build();
    }

    @SuppressWarnings("deprecation")
    private static Notification.Action action(String title, PendingIntent pi) {
        // Action.Builder(Icon, ...) appeared in Android 6; the int variant works since 4.4W.
        if (Build.VERSION.SDK_INT >= 23) return new Notification.Action.Builder(null, title, pi).build();
        return new Notification.Action.Builder(0, title, pi).build();
    }

    static String rate(double bps) {
        String[] u = {"Б/с", "КБ/с", "МБ/с", "ГБ/с"};
        int i = 0;
        while (bps >= 1024 && i < u.length - 1) {
            bps /= 1024;
            i++;
        }
        return i == 0 ? String.format(Locale.ROOT, "%.0f %s", bps, u[i]) : String.format(Locale.ROOT, "%.1f %s", bps, u[i]);
    }
}
