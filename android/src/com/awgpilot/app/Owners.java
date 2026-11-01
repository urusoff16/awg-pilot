package com.awgpilot.app;

import android.content.Context;
import android.content.pm.PackageManager;
import android.net.ConnectivityManager;
import android.os.Build;

import java.io.BufferedReader;
import java.io.FileReader;
import java.net.InetAddress;
import java.net.InetSocketAddress;
import java.util.concurrent.ConcurrentHashMap;

/**
 * Finds the app behind a connection in the tunnel for the traffic log. Android 10+ answers this
 * for the active VPN app (getConnectionOwnerUid); older systems still expose /proc/net with uids.
 */
final class Owners {
    private static final ConcurrentHashMap<Integer, String> names = new ConcurrentHashMap<>();

    private Owners() {}

    static String find(Context ctx, int proto, String local, int lport, String remote, int rport) {
        int uid = -1;
        try {
            if (Build.VERSION.SDK_INT >= 29) {
                ConnectivityManager cm = (ConnectivityManager) ctx.getSystemService(Context.CONNECTIVITY_SERVICE);
                uid = cm.getConnectionOwnerUid(proto,
                        new InetSocketAddress(InetAddress.getByName(local), lport),
                        new InetSocketAddress(InetAddress.getByName(remote), rport));
            } else {
                uid = procUid(proto == 6 ? "tcp" : "udp", lport);
            }
        } catch (Exception e) {
            return "";
        }
        return uid < 0 ? "" : name(ctx, uid);
    }

    private static String name(Context ctx, int uid) {
        String n = names.get(uid);
        if (n != null) return n;
        PackageManager pm = ctx.getPackageManager();
        String[] pkgs = pm.getPackagesForUid(uid);
        if (pkgs != null && pkgs.length > 0) {
            n = pkgs[0];
            // apps sharing one uid (Google services): prefer one that has a launcher entry
            for (String p : pkgs) {
                if (pm.getLaunchIntentForPackage(p) != null) {
                    n = p;
                    break;
                }
            }
        } else if (uid == 0) {
            n = "root";
        } else {
            n = pm.getNameForUid(uid);
            if (n == null) n = "uid " + uid;
        }
        names.put(uid, n);
        return n;
    }

    /** Android 9 and older: uid from /proc/net/{tcp,tcp6,udp,udp6} by local port. */
    private static int procUid(String proto, int lport) {
        for (String f : new String[]{proto, proto + "6"}) {
            try (BufferedReader r = new BufferedReader(new FileReader("/proc/net/" + f))) {
                r.readLine(); // header
                String line;
                while ((line = r.readLine()) != null) {
                    String[] c = line.trim().split("\\s+");
                    if (c.length < 8) continue;
                    int colon = c[1].lastIndexOf(':');
                    if (colon < 0) continue;
                    if (Integer.parseInt(c[1].substring(colon + 1), 16) == lport) return Integer.parseInt(c[7]);
                }
            } catch (Exception ignored) {
            }
        }
        return -1;
    }
}
