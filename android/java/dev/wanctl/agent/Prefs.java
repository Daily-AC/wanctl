package com.***REMOVED***.wanctl;

import android.content.Context;
import android.content.SharedPreferences;

/** The handful of user choices the app owns. Everything else lives in wanctl's own config dir. */
final class Prefs {
    private static final String FILE = "wanctl";
    private static final String ENABLED = "enabled";
    private static final String BOOT = "boot";
    private static final String AUTO_TRUST = "auto_trust";
    private static final String BYPASS = "bypass";
    private static final String ELEVATION = "elevation";
    private static final String NAME = "name";

    private final SharedPreferences sp;

    Prefs(Context c) {
        sp = c.getApplicationContext().getSharedPreferences(FILE, Context.MODE_PRIVATE);
    }

    /** Whether the user wants the agent running. The service reflects this, it does not define it. */
    boolean enabled() {
        return sp.getBoolean(ENABLED, false);
    }

    void setEnabled(boolean v) {
        sp.edit().putBoolean(ENABLED, v).apply();
    }

    boolean bootStart() {
        return sp.getBoolean(BOOT, true);
    }

    void setBootStart(boolean v) {
        sp.edit().putBoolean(BOOT, v).apply();
    }

    /**
     * Off by default, and deliberately so. Without it the agent routes an
     * unknown controller's pairing request to the portal web console for a
     * human decision, which works on a headless device; --yes trades that gate
     * away for anyone holding a namespace token.
     */
    boolean autoTrust() {
        return sp.getBoolean(AUTO_TRUST, false);
    }

    void setAutoTrust(boolean v) {
        sp.edit().putBoolean(AUTO_TRUST, v).apply();
    }

    /**
     * Policy bypass, off by default.
     *
     * <p>Without this the agent runs in `normal` mode, where a command that
     * matches no rule is refused unless a human approves it — and on a device
     * with no terminal, the only place that approval can happen is the portal
     * web console. That is the right default and a usable one for a device
     * someone watches. It is not usable for an unattended box, and there is no
     * other way to say so from here: `wanctl rules` and `--mode` are local
     * commands, and an APK has no shell to type them into.
     */
    boolean bypass() {
        return sp.getBoolean(BYPASS, false);
    }

    void setBypass(boolean v) {
        sp.edit().putBoolean(BYPASS, v).apply();
    }

    /**
     * The elevation channels, off by default and separate from every other
     * switch here.
     *
     * <p>With it on, the agent may run a command through root, Shizuku, or the
     * device's own adbd instead of inside the app sandbox — which is what makes
     * {@code pm}, {@code am}, {@code input}, {@code screencap}, {@code dumpsys}
     * and {@code settings} work at all. That is a real privilege boundary, so
     * it gets its own decision rather than riding along on 自动放行所有命令:
     * turning that switch on says "this device is unattended", not "hand out
     * root". The agent enforces the same separation in its policy engine —
     * bypass mode does not authorize an elevated command (ADR 0004).
     *
     * <p>Off does not merely deny the commands, it stops the channels being
     * probed at all, so nothing here raises a root-manager consent dialog on a
     * device whose owner never asked for any of this.
     */
    boolean elevation() {
        return sp.getBoolean(ELEVATION, false);
    }

    void setElevation(boolean v) {
        sp.edit().putBoolean(ELEVATION, v).apply();
    }

    /** Empty means "let wanctl ask the property service", which yields e.g. "pa2353". */
    String deviceName() {
        return sp.getString(NAME, "").trim();
    }

    void setDeviceName(String v) {
        sp.edit().putString(NAME, v == null ? "" : v.trim()).apply();
    }
}
