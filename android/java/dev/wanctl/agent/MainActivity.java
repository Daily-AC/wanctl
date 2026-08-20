package dev.wanctl.agent;

import android.Manifest;
import android.app.Activity;
import android.app.AlertDialog;
import android.content.ActivityNotFoundException;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.os.PowerManager;
import android.provider.Settings;
import android.text.Editable;
import android.text.TextWatcher;
import android.view.View;
import android.widget.Button;
import android.widget.EditText;
import android.widget.LinearLayout;
import android.widget.Switch;
import android.widget.TextView;
import android.widget.Toast;

import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

/**
 * The whole UI. It shows what the device is, whether it is reachable, and gives
 * the user the four decisions they actually have: run or not, come back after a
 * reboot or not, who may pair, and which credentials to hold.
 */
public final class MainActivity extends Activity {
    /**
     * The portal this device enrolls against. A branded build bakes one into
     * BuildInfo (scripts/build-apk.sh injects the same value into the Go
     * binary, so the two can never disagree); the open-source build ships
     * none, and the value is whatever the user typed in the enroll dialog.
     */
    private String portalOrigin() {
        String typed = prefs.portal();
        return typed.isEmpty() ? BuildInfo.PORTAL : typed;
    }

    private final ExecutorService io = Executors.newSingleThreadExecutor();
    private final Handler main = new Handler(Looper.getMainLooper());
    private final Runnable onStateChange = this::renderState;

    private Prefs prefs;
    private Installer installer;

    private TextView version, status, relay, fingerprint, credential, autoTrustWarn, bypassWarn, elevationWarn;
    private EditText name;
    private Switch enabled, boot, autoTrust, bypass, elevation;
    private Button battery, login, logs, update;

    private boolean binding;

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        setContentView(R.layout.activity_main);
        prefs = new Prefs(this);
        installer = new Installer(this);

        version = findViewById(R.id.version);
        status = findViewById(R.id.status);
        relay = findViewById(R.id.relay);
        fingerprint = findViewById(R.id.fingerprint);
        credential = findViewById(R.id.credential);
        autoTrustWarn = findViewById(R.id.autotrust_warn);
        bypassWarn = findViewById(R.id.bypass_warn);
        elevationWarn = findViewById(R.id.elevation_warn);
        name = findViewById(R.id.name);
        enabled = findViewById(R.id.enabled);
        boot = findViewById(R.id.boot);
        autoTrust = findViewById(R.id.autotrust);
        bypass = findViewById(R.id.bypass);
        elevation = findViewById(R.id.elevation);
        battery = findViewById(R.id.battery);
        login = findViewById(R.id.login);
        logs = findViewById(R.id.logs);
        update = findViewById(R.id.update);

        enabled.setOnCheckedChangeListener((v, checked) -> {
            if (binding) {
                return;
            }
            prefs.setEnabled(checked);
            if (checked) {
                requestNotificationPermission();
                KeeperJob.schedule(this);
                AgentService.start(this);
            } else {
                KeeperJob.cancel(this);
                AgentService.stop(this);
            }
        });
        boot.setOnCheckedChangeListener((v, checked) -> {
            if (!binding) {
                prefs.setBootStart(checked);
            }
        });
        autoTrust.setOnCheckedChangeListener((v, checked) -> {
            if (binding) {
                return;
            }
            prefs.setAutoTrust(checked);
            autoTrustWarn.setVisibility(checked ? View.VISIBLE : View.GONE);
            restartIfRunning();
        });
        bypass.setOnCheckedChangeListener((v, checked) -> {
            if (binding) {
                return;
            }
            prefs.setBypass(checked);
            bypassWarn.setVisibility(checked ? View.VISIBLE : View.GONE);
            restartIfRunning();
        });
        elevation.setOnCheckedChangeListener((v, checked) -> {
            if (binding) {
                return;
            }
            prefs.setElevation(checked);
            elevationWarn.setVisibility(checked ? View.VISIBLE : View.GONE);
            // The agent reads WANCTL_ELEVATION from its environment at startup,
            // so the running child has to be replaced for this to mean anything.
            restartIfRunning();
        });
        name.addTextChangedListener(new TextWatcher() {
            @Override
            public void beforeTextChanged(CharSequence s, int a, int b, int c) {
            }

            @Override
            public void onTextChanged(CharSequence s, int a, int b, int c) {
            }

            @Override
            public void afterTextChanged(Editable s) {
                if (!binding) {
                    prefs.setDeviceName(s.toString());
                }
            }
        });

        battery.setOnClickListener(v -> {
            PowerManager pm = getSystemService(PowerManager.class);
            if (pm.isIgnoringBatteryOptimizations(getPackageName())) {
                showOEMBackgroundHelp();
            } else {
                askBatteryExemption();
            }
        });
        login.setOnClickListener(v -> promptEnroll());
        logs.setOnClickListener(v -> startActivity(new Intent(this, LogActivity.class)));
        update.setOnClickListener(v -> installer.checkAndInstall());
    }

    @Override
    protected void onResume() {
        super.onResume();
        AgentState.get().addListener(onStateChange);
        bindPrefs();
        reconcile();
        renderState();
        probe();
    }

    /**
     * Makes the running state match the switch.
     *
     * <p>The two drift apart whenever something outside the app stops the
     * service — a low-memory kill the system chose not to redeliver, a
     * BOOT_COMPLETED the system discarded. Before this, opening the app in that
     * state showed "已停止" under a switch that said on, and the only way out
     * was to toggle it off and on again: the app knew what was wrong and made
     * the user fix it by hand.
     */
    private void reconcile() {
        if (prefs.enabled() && !AgentService.isRunning()) {
            AgentService.start(this);
        }
        KeeperJob.schedule(this);
    }

    @Override
    protected void onPause() {
        AgentState.get().removeListener(onStateChange);
        super.onPause();
    }

    @Override
    protected void onDestroy() {
        installer.close();
        io.shutdownNow();
        super.onDestroy();
    }

    // ---------------------------------------------------------------- rendering

    private void bindPrefs() {
        binding = true;
        enabled.setChecked(prefs.enabled());
        boot.setChecked(prefs.bootStart());
        autoTrust.setChecked(prefs.autoTrust());
        autoTrustWarn.setVisibility(prefs.autoTrust() ? View.VISIBLE : View.GONE);
        bypass.setChecked(prefs.bypass());
        bypassWarn.setVisibility(prefs.bypass() ? View.VISIBLE : View.GONE);
        elevation.setChecked(prefs.elevation());
        elevationWarn.setVisibility(prefs.elevation() ? View.VISIBLE : View.GONE);
        String n = prefs.deviceName();
        if (!n.equals(name.getText().toString())) {
            name.setText(n);
        }
        binding = false;

        PowerManager pm = getSystemService(PowerManager.class);
        // Deliberately still tappable once exempt: the AOSP exemption is only
        // half the story on an OEM ROM, and a greyed-out button would read as
        // "nothing left to do here" while the device is still being frozen.
        boolean exempt = pm.isIgnoringBatteryOptimizations(getPackageName());
        battery.setText(exempt ? getString(R.string.battery_exempt) : getString(R.string.btn_battery));
    }

    private void renderState() {
        AgentState s = AgentState.get();
        switch (s.phase()) {
            case ONLINE:
                status.setText(getString(R.string.state_running));
                status.setTextColor(getColor(R.color.ok));
                break;
            case STARTING:
            case RETRYING:
                status.setText(getString(R.string.state_retrying)
                        + (s.detail().isEmpty() ? "" : "  " + s.detail()));
                status.setTextColor(getColor(R.color.warn));
                break;
            case ERROR:
                status.setText(getString(R.string.state_stopped) + "  " + s.detail());
                status.setTextColor(getColor(R.color.warn));
                break;
            default:
                status.setText(getString(R.string.state_stopped));
                status.setTextColor(getColor(R.color.off));
        }
        relay.setText(s.relay().isEmpty() ? getString(R.string.state_unknown) : s.relay());
        if (!s.fingerprint().isEmpty()) {
            fingerprint.setText(s.fingerprint());
        }
    }

    /**
     * Asks the binary about itself rather than reimplementing its answers in
     * Java. The identity and the stored token live in wanctl's config dir under
     * a layout that is wanctl's business, and a second implementation of "where
     * is the token" is a second thing that can be wrong.
     */
    private void probe() {
        io.execute(() -> {
            Wanctl.Result v = Wanctl.run(this, 10, "version");
            Wanctl.Result id = Wanctl.run(this, 10, "id");
            Wanctl.Result st = Wanctl.run(this, 10, "status");
            String fp = firstLineAfter(id.out, "fingerprint:");
            String cred = firstLineAfter(st.out, "凭证:");
            main.post(() -> {
                version.setText(v.ok() ? v.out.trim() : v.message());
                if (fp != null) {
                    AgentState.get().setFingerprint(fp);
                    fingerprint.setText(fp);
                }
                credential.setText(cred != null ? cred
                        : getString(R.string.cred_no));
                boolean loggedIn = cred != null && cred.contains("已登录");
                login.setText(loggedIn ? R.string.btn_relogin : R.string.btn_login);
            });
        });
    }

    /**
     * The agent reads --yes and --mode once, at startup. A toggle that only
     * takes effect "next time" is a setting that lies: the switch says bypass
     * is off while the running agent is still allowing everything.
     */
    private void restartIfRunning() {
        if (!prefs.enabled()) {
            return;
        }
        AgentService.restart(this);
        Toast.makeText(this, "已按新设置重启 agent", Toast.LENGTH_SHORT).show();
    }

    private static String firstLineAfter(String text, String key) {
        for (String line : text.split("\n")) {
            int i = line.indexOf(key);
            if (i >= 0) {
                return line.substring(i + key.length()).trim();
            }
        }
        return null;
    }

    // ---------------------------------------------------------------- actions

    /**
     * The enrollment code is exchanged by the binary, not by the app. wanctl's
     * own `login` reads the code from stdin, which an app has no way to drive
     * meaningfully, so the binary grew a `--code` flag; the token, the portal
     * fingerprint it seeds, and the namespace binding all stay on one side of
     * the fence.
     */
    private void promptEnroll() {
        LinearLayout box = new LinearLayout(this);
        box.setOrientation(LinearLayout.VERTICAL);
        int pad = (int) (24 * getResources().getDisplayMetrics().density);
        box.setPadding(pad, pad / 2, pad, 0);

        TextView body = new TextView(this);
        body.setText(R.string.enroll_body);
        box.addView(body);

        // Builds without a baked-in portal (the open-source APK) ask for one
        // here. It persists, feeds the binary as WANCTL_PORTAL, and the open
        // button below reads the field live so "type, then tap" just works.
        final EditText portal;
        if (BuildInfo.PORTAL.isEmpty()) {
            portal = new EditText(this);
            portal.setHint(R.string.enroll_portal_hint);
            portal.setSingleLine(true);
            portal.setText(prefs.portal());
            box.addView(portal);
        } else {
            portal = null;
        }

        Button open = new Button(this);
        open.setText(R.string.enroll_open);
        open.setOnClickListener(v -> {
            String origin = savePortal(portal);
            if (origin.isEmpty()) {
                Toast.makeText(this, R.string.enroll_portal_missing, Toast.LENGTH_SHORT).show();
                return;
            }
            openUrl(origin + "/enroll");
        });
        box.addView(open);

        EditText code = new EditText(this);
        code.setHint(R.string.enroll_hint);
        code.setSingleLine(true);
        box.addView(code);

        new AlertDialog.Builder(this)
                .setTitle(R.string.enroll_title)
                .setView(box)
                .setNegativeButton(R.string.cancel, null)
                .setPositiveButton(R.string.enroll_ok, (d, w) -> {
                    savePortal(portal);
                    submitEnroll(code.getText().toString().trim());
                })
                .show();
    }

    /**
     * Persists the enroll dialog's portal field and returns the effective
     * origin. A trailing slash is dropped because the value is joined with
     * paths like {@code /enroll} and handed to the binary, which expects a
     * bare origin.
     */
    private String savePortal(EditText field) {
        if (field == null) {
            return portalOrigin();
        }
        String typed = field.getText().toString().trim();
        while (typed.endsWith("/")) {
            typed = typed.substring(0, typed.length() - 1);
        }
        prefs.setPortal(typed);
        return typed.isEmpty() ? BuildInfo.PORTAL : typed;
    }

    private void submitEnroll(String code) {
        if (code.isEmpty()) {
            return;
        }
        Toast.makeText(this, "正在验证 …", Toast.LENGTH_SHORT).show();
        io.execute(() -> {
            Wanctl.Result r = Wanctl.run(this, 60, "login", "--code", code);
            main.post(() -> {
                if (r.ok()) {
                    Toast.makeText(this, r.out.trim().isEmpty() ? "✓ 已绑定" : r.out.trim(),
                            Toast.LENGTH_LONG).show();
                    probe();
                    if (prefs.enabled()) {
                        // The agent is holding a token it was started with;
                        // a new one only takes effect on restart.
                        AgentService.restart(this);
                    }
                } else {
                    new AlertDialog.Builder(this)
                            .setTitle(R.string.enroll_title)
                            .setMessage(r.message())
                            .setPositiveButton(android.R.string.ok, null)
                            .show();
                }
            });
        });
    }

    /**
     * The OEM background managers, which the AOSP exemption does not cover.
     *
     * <p>Measured on the vivo PA2353: with the battery-optimization exemption
     * granted, autostart on, a foreground service running and a partial wake
     * lock held, the app's processes were still moved into the
     * <code>freezer:/frozen</code> cgroup about two minutes after the screen
     * went off — all eleven threads in uninterruptible sleep, the poll loop
     * simply not executing, the device gone from the relay. Setting
     * 后台耗电管理 to 允许后台高耗电 stopped it dead: eight minutes of screen-off
     * with the cgroup at <code>freezer:/</code> and the device reachable
     * throughout. Nothing an app can do substitutes for it, so the only honest
     * move is to tell the user exactly where it lives.
     */
    private void showOEMBackgroundHelp() {
        new AlertDialog.Builder(this)
                .setTitle(R.string.battery_oem)
                .setMessage(R.string.battery_oem_body)
                .setPositiveButton(android.R.string.ok, null)
                .show();
    }

    private void askBatteryExemption() {
        new AlertDialog.Builder(this)
                .setTitle(R.string.battery_title)
                .setMessage(R.string.battery_body)
                .setNeutralButton(R.string.battery_oem, (d, w) -> showOEMBackgroundHelp())
                .setNegativeButton(R.string.cancel, null)
                .setPositiveButton(android.R.string.ok, (d, w) -> {
                    Intent i = new Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS,
                            Uri.parse("package:" + getPackageName()));
                    try {
                        startActivity(i);
                    } catch (ActivityNotFoundException e) {
                        startActivity(new Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS));
                    }
                })
                .show();
    }

    private void openUrl(String url) {
        try {
            startActivity(new Intent(Intent.ACTION_VIEW, Uri.parse(url))
                    .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK));
        } catch (ActivityNotFoundException e) {
            Toast.makeText(this, url, Toast.LENGTH_LONG).show();
        }
    }

    private void requestNotificationPermission() {
        if (Build.VERSION.SDK_INT < 33) {
            return;
        }
        if (checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS)
                != PackageManager.PERMISSION_GRANTED) {
            requestPermissions(new String[]{Manifest.permission.POST_NOTIFICATIONS}, 1);
        }
    }
}
