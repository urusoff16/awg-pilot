package com.awgpilot.app;

import android.Manifest;
import android.app.Activity;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.database.Cursor;
import android.graphics.Bitmap;
import android.graphics.Canvas;
import android.graphics.Insets;
import android.graphics.drawable.Drawable;
import android.net.Uri;
import android.net.VpnService;
import android.os.Build;
import android.os.Bundle;
import android.provider.OpenableColumns;
import android.util.LruCache;
import android.view.View;
import android.view.WindowInsets;
import android.webkit.WebResourceRequest;
import android.webkit.WebResourceResponse;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.FrameLayout;

import org.json.JSONObject;

import java.io.ByteArrayInputStream;
import java.io.ByteArrayOutputStream;
import java.io.InputStream;
import java.io.OutputStream;
import java.nio.charset.StandardCharsets;

/** Hosts the shared web UI (the same page as the Windows app) in a full-screen WebView. */
public final class MainActivity extends Activity {
    private static final String HOST = "app.awgpilot.invalid"; // never resolves: served from the APK
    private static final String HOME = "https://" + HOST + "/index.html";
    private static final int REQ_VPN = 1, REQ_IMPORT = 2, REQ_EXPORT = 3, REQ_NOTIFY = 4;
    private static final int BG = 0xFF0E1218;

    interface BoolCb { void done(boolean v); }
    interface ConfigCb { void done(String text, String name); }
    interface ErrCb { void done(String error); }

    private WebView web;
    private BoolCb vpnCb;
    private ConfigCb importCb;
    private ErrCb exportCb;
    private String exportText;
    private final LruCache<String, byte[]> icons = new LruCache<>(256);

    @Override
    protected void onCreate(Bundle state) {
        super.onCreate(state);
        Core.init(this);
        FrameLayout root = new FrameLayout(this);
        root.setBackgroundColor(BG);
        web = new WebView(this);
        web.setBackgroundColor(BG);
        web.setOverScrollMode(View.OVER_SCROLL_NEVER);
        root.addView(web, new FrameLayout.LayoutParams(-1, -1));
        setContentView(root);
        edgeToEdge(root);

        WebSettings s = web.getSettings();
        s.setJavaScriptEnabled(true);
        s.setDomStorageEnabled(true);
        s.setTextZoom(100); // the layout is sized to fill the screen exactly
        s.setSupportZoom(false);
        s.setAllowFileAccess(false);
        s.setAllowContentAccess(false);
        s.setMixedContentMode(WebSettings.MIXED_CONTENT_NEVER_ALLOW);
        web.addJavascriptInterface(new Bridge(this), "AWGAndroid");
        web.setWebViewClient(new WebViewClient() {
            @Override
            public WebResourceResponse shouldInterceptRequest(WebView view, WebResourceRequest req) {
                Uri u = req.getUrl();
                return HOST.equals(u.getHost()) ? serve(u.getPath()) : null;
            }

            @Override
            public boolean shouldOverrideUrlLoading(WebView view, WebResourceRequest req) {
                Uri u = req.getUrl();
                if (HOST.equals(u.getHost())) return false;
                try {
                    startActivity(new Intent(Intent.ACTION_VIEW, u)); // links open in the browser
                } catch (Exception ignored) {
                }
                return true;
            }
        });
        web.loadUrl(HOME);

        if (Build.VERSION.SDK_INT >= 33 && checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
                && !getSharedPreferences("ui", MODE_PRIVATE).getBoolean("asked_notify", false)) {
            getSharedPreferences("ui", MODE_PRIVATE).edit().putBoolean("asked_notify", true).apply();
            requestPermissions(new String[]{Manifest.permission.POST_NOTIFICATIONS}, REQ_NOTIFY);
        }
    }

    /** Draws behind the system bars and pads the page by their size (and the keyboard's). */
    @SuppressWarnings("deprecation")
    private void edgeToEdge(View root) {
        if (Build.VERSION.SDK_INT >= 30) {
            getWindow().setDecorFitsSystemWindows(false);
        } else {
            getWindow().getDecorView().setSystemUiVisibility(View.SYSTEM_UI_FLAG_LAYOUT_STABLE
                    | View.SYSTEM_UI_FLAG_LAYOUT_FULLSCREEN | View.SYSTEM_UI_FLAG_LAYOUT_HIDE_NAVIGATION);
        }
        if (Build.VERSION.SDK_INT < 35) {
            getWindow().setStatusBarColor(BG);
            getWindow().setNavigationBarColor(BG);
        }
        root.setOnApplyWindowInsetsListener((v, in) -> {
            if (Build.VERSION.SDK_INT >= 30) {
                Insets i = in.getInsets(WindowInsets.Type.systemBars() | WindowInsets.Type.displayCutout() | WindowInsets.Type.ime());
                v.setPadding(i.left, i.top, i.right, i.bottom);
                return WindowInsets.CONSUMED;
            }
            v.setPadding(in.getSystemWindowInsetLeft(), in.getSystemWindowInsetTop(), in.getSystemWindowInsetRight(), in.getSystemWindowInsetBottom());
            return in.consumeSystemWindowInsets();
        });
    }

    private WebResourceResponse serve(String path) {
        try {
            if (path == null || path.equals("/")) path = "/index.html";
            if (path.startsWith("/appicon/")) {
                byte[] png = appIcon(Uri.decode(path.substring(9)));
                return png == null ? notFound() : new WebResourceResponse("image/png", null, new ByteArrayInputStream(png));
            }
            if (path.contains("..")) return notFound();
            InputStream in = getAssets().open("web" + path);
            return new WebResourceResponse(mime(path), mime(path).startsWith("text/") || path.endsWith(".js") ? "utf-8" : null, in);
        } catch (Exception e) {
            return notFound();
        }
    }

    private static WebResourceResponse notFound() {
        WebResourceResponse r = new WebResourceResponse("text/plain", "utf-8", new ByteArrayInputStream(new byte[0]));
        r.setStatusCodeAndReasonPhrase(404, "Not Found");
        return r;
    }

    private static String mime(String p) {
        if (p.endsWith(".html")) return "text/html";
        if (p.endsWith(".js")) return "application/javascript";
        if (p.endsWith(".css")) return "text/css";
        if (p.endsWith(".svg")) return "image/svg+xml";
        if (p.endsWith(".png")) return "image/png";
        if (p.endsWith(".json")) return "application/json";
        if (p.endsWith(".woff2")) return "font/woff2";
        return "application/octet-stream";
    }

    private byte[] appIcon(String pkg) throws Exception {
        byte[] cached = icons.get(pkg);
        if (cached != null) return cached;
        Drawable d = getPackageManager().getApplicationIcon(pkg);
        int px = Math.round(40 * getResources().getDisplayMetrics().density);
        Bitmap bmp = Bitmap.createBitmap(px, px, Bitmap.Config.ARGB_8888);
        Canvas c = new Canvas(bmp);
        d.setBounds(0, 0, px, px);
        d.draw(c);
        ByteArrayOutputStream out = new ByteArrayOutputStream();
        bmp.compress(Bitmap.CompressFormat.PNG, 100, out);
        byte[] png = out.toByteArray();
        icons.put(pkg, png);
        return png;
    }

    // ---- bridge helpers (any thread) -------------------------------------------------------

    void reply(int id, String json) {
        String js = "window.__awgReply(" + id + "," + JSONObject.quote(json) + ")";
        runOnUiThread(() -> web.evaluateJavascript(js, null));
    }

    void withVpnConsent(BoolCb cb) {
        runOnUiThread(() -> {
            Intent i = VpnService.prepare(this);
            if (i == null) {
                cb.done(true);
                return;
            }
            vpnCb = cb;
            try {
                startActivityForResult(i, REQ_VPN);
            } catch (Exception e) {
                vpnCb = null;
                cb.done(false);
            }
        });
    }

    void pickConfig(ConfigCb cb) {
        runOnUiThread(() -> {
            importCb = cb;
            Intent i = new Intent(Intent.ACTION_OPEN_DOCUMENT).addCategory(Intent.CATEGORY_OPENABLE).setType("*/*");
            startActivityForResult(i, REQ_IMPORT);
        });
    }

    void saveConfig(String name, String text, ErrCb cb) {
        runOnUiThread(() -> {
            exportCb = cb;
            exportText = text;
            Intent i = new Intent(Intent.ACTION_CREATE_DOCUMENT).addCategory(Intent.CATEGORY_OPENABLE)
                    .setType("application/octet-stream").putExtra(Intent.EXTRA_TITLE, name);
            startActivityForResult(i, REQ_EXPORT);
        });
    }

    @Override
    protected void onActivityResult(int req, int res, Intent data) {
        super.onActivityResult(req, res, data);
        if (req == REQ_VPN && vpnCb != null) {
            BoolCb cb = vpnCb;
            vpnCb = null;
            cb.done(res == RESULT_OK);
        } else if (req == REQ_IMPORT && importCb != null) {
            ConfigCb cb = importCb;
            importCb = null;
            Uri u = res == RESULT_OK && data != null ? data.getData() : null;
            if (u == null) {
                cb.done(null, null);
                return;
            }
            Core.IO.execute(() -> {
                try (InputStream in = getContentResolver().openInputStream(u)) {
                    ByteArrayOutputStream out = new ByteArrayOutputStream();
                    byte[] buf = new byte[8192];
                    for (int n; (n = in.read(buf)) > 0 && out.size() < 256 * 1024; ) out.write(buf, 0, n);
                    cb.done(new String(out.toByteArray(), StandardCharsets.UTF_8), displayName(u));
                } catch (Exception e) {
                    cb.done("", displayName(u)); // the core reports the parse error
                }
            });
        } else if (req == REQ_EXPORT && exportCb != null) {
            ErrCb cb = exportCb;
            String text = exportText;
            exportCb = null;
            exportText = null;
            Uri u = res == RESULT_OK && data != null ? data.getData() : null;
            if (u == null) {
                cb.done(null);
                return;
            }
            Core.IO.execute(() -> {
                try (OutputStream out = getContentResolver().openOutputStream(u)) {
                    out.write(text.getBytes(StandardCharsets.UTF_8));
                    cb.done(null);
                } catch (Exception e) {
                    cb.done("не удалось записать файл: " + e.getMessage());
                }
            });
        }
    }

    private String displayName(Uri u) {
        try (Cursor c = getContentResolver().query(u, new String[]{OpenableColumns.DISPLAY_NAME}, null, null, null)) {
            if (c != null && c.moveToFirst()) return c.getString(0);
        } catch (Exception ignored) {
        }
        return "";
    }

    // ---- lifecycle -------------------------------------------------------------------------

    @Override
    @SuppressWarnings("deprecation")
    public void onBackPressed() {
        web.evaluateJavascript("window.__awgBack ? window.__awgBack() : false", v -> {
            if (!"true".equals(v)) moveTaskToBack(true);
        });
    }

    @Override
    protected void onPause() {
        super.onPause();
        web.onPause();
    }

    @Override
    protected void onResume() {
        super.onResume();
        web.onResume();
    }

    @Override
    protected void onDestroy() {
        web.destroy();
        super.onDestroy();
    }
}
