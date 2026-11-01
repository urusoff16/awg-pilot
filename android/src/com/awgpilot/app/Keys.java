package com.awgpilot.app;

import android.content.Context;
import android.content.SharedPreferences;
import android.os.Build;
import android.security.keystore.KeyGenParameterSpec;
import android.security.keystore.KeyProperties;
import android.util.Base64;
import android.util.Log;

import java.security.KeyStore;
import java.security.SecureRandom;

import javax.crypto.Cipher;
import javax.crypto.KeyGenerator;
import javax.crypto.SecretKey;
import javax.crypto.spec.GCMParameterSpec;

/**
 * The key that encrypts saved keys and configs (secrets.bin). It is random, and on Android 6+
 * it is stored wrapped by a non-exportable Android Keystore key, so a copy of the app's files
 * alone cannot be decrypted.
 */
final class Keys {
    private static final String ALIAS = "awgpilot_wrap";
    private static final String PREFS = "keys";

    private Keys() {}

    static String dataKeyHex(Context ctx) {
        SharedPreferences p = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE);
        try {
            String stored = p.getString("data", null);
            if (stored != null) return hex(unwrap(stored));
            byte[] key = new byte[32];
            new SecureRandom().nextBytes(key);
            p.edit().putString("data", wrap(key)).commit();
            return hex(key);
        } catch (Exception e) {
            Log.e(Core.TAG, "keystore", e);
            return "";
        }
    }

    private static String wrap(byte[] key) throws Exception {
        if (Build.VERSION.SDK_INT < 23) return "plain:" + Base64.encodeToString(key, Base64.NO_WRAP);
        Cipher c = Cipher.getInstance("AES/GCM/NoPadding");
        c.init(Cipher.ENCRYPT_MODE, wrapKey());
        byte[] iv = c.getIV();
        byte[] ct = c.doFinal(key);
        return "ks:" + Base64.encodeToString(iv, Base64.NO_WRAP) + ":" + Base64.encodeToString(ct, Base64.NO_WRAP);
    }

    private static byte[] unwrap(String s) throws Exception {
        if (s.startsWith("plain:")) return Base64.decode(s.substring(6), Base64.NO_WRAP);
        String[] parts = s.split(":");
        Cipher c = Cipher.getInstance("AES/GCM/NoPadding");
        c.init(Cipher.DECRYPT_MODE, wrapKey(), new GCMParameterSpec(128, Base64.decode(parts[1], Base64.NO_WRAP)));
        return c.doFinal(Base64.decode(parts[2], Base64.NO_WRAP));
    }

    private static SecretKey wrapKey() throws Exception {
        KeyStore ks = KeyStore.getInstance("AndroidKeyStore");
        ks.load(null);
        if (ks.containsAlias(ALIAS)) return (SecretKey) ks.getKey(ALIAS, null);
        KeyGenerator g = KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, "AndroidKeyStore");
        g.init(new KeyGenParameterSpec.Builder(ALIAS, KeyProperties.PURPOSE_ENCRYPT | KeyProperties.PURPOSE_DECRYPT)
                .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
                .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
                .setKeySize(256)
                .build());
        return g.generateKey();
    }

    private static String hex(byte[] b) {
        StringBuilder sb = new StringBuilder(b.length * 2);
        for (byte x : b) sb.append(String.format("%02x", x));
        return sb.toString();
    }
}
