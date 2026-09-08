package dev.wanctl.agent;

import android.Manifest;
import android.animation.ValueAnimator;
import android.app.Activity;
import android.app.AlertDialog;
import android.content.ActivityNotFoundException;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.content.res.ColorStateList;
import android.graphics.Color;
import android.graphics.Typeface;
import android.graphics.drawable.GradientDrawable;
import android.graphics.drawable.RippleDrawable;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.os.PowerManager;
import android.provider.Settings;
import android.view.Gravity;
import android.view.View;
import android.view.animation.DecelerateInterpolator;
import android.widget.Button;
import android.widget.EditText;
import android.widget.ImageView;
import android.widget.LinearLayout;
import android.widget.ScrollView;
import android.widget.Switch;
import android.widget.TextView;

import java.util.UUID;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

/** Native, dependency-free setup: permissions, instance, sign-in, one run control. */
public final class MainActivity extends Activity {
    private static final String HOSTED_PORTAL = "https://wanctl.z10.dev";
    private static final String HOSTED_RELAY = "https://wanctl-relay.z10.dev";
    private static final int INK = Color.rgb(29, 29, 31), MUTED = Color.rgb(105, 105, 110);
    private static final int BLUE = Color.rgb(0, 102, 204), CANVAS = Color.rgb(250, 250, 252);
    private final ExecutorService io = Executors.newSingleThreadExecutor();
    private final Handler main = new Handler(Looper.getMainLooper());
    private final Runnable onStateChange = this::renderState;
    private Prefs prefs;
    private Installer installer;
    private LinearLayout body, footer;
    private Runnable navigateBack;
    private boolean settingsFlow;
    private String pendingPage = "";
    private TextView homeCaption;
    private ConnectionMark connectionMark;
    private TextView status;
    private Button power;
    private String page = "",
            configuredRelay = "",
            configuredPortal = "",
            fingerprint = "",
            deviceId = "",
            credential = "";
    private boolean ready, loggedIn, busy, probing, permissionsFromSettings;
    private EditText relayField, portalField;
    private String draftRelay, draftPortal;
    private ValueAnimator animation;

    @Override
    protected void onCreate(Bundle state) {
        super.onCreate(state);
        if (getActionBar() != null) getActionBar().hide();
        prefs = new Prefs(this);
        installer = new Installer(this);
        if (state != null) {
            pendingPage = state.getString("page", "");
            settingsFlow = state.getBoolean("settings_flow");
            draftRelay = state.getString("draft_relay");
            draftPortal = state.getString("draft_portal");
            permissionsFromSettings = state.getBoolean("permissions_from_settings");
        }
        route();
        receiveLogin(getIntent());
    }

    @Override
    protected void onSaveInstanceState(Bundle state) {
        state.putString("page", page);
        state.putBoolean("settings_flow", settingsFlow);
        state.putBoolean("permissions_from_settings", permissionsFromSettings);
        if (page.equals("custom") && relayField != null) {
            state.putString("draft_relay", relayField.getText().toString());
            state.putString("draft_portal", portalField.getText().toString());
        }
        super.onSaveInstanceState(state);
    }

    @Override
    protected void onNewIntent(Intent intent) {
        super.onNewIntent(intent);
        setIntent(intent);
        receiveLogin(intent);
    }

    @Override
    protected void onResume() {
        super.onResume();
        AgentState.get().addListener(onStateChange);
        if (page.equals("permissions")) showPermissions();
        if (connectionMark != null) connectionMark.resume();
        probe();
    }

    @Override
    protected void onPause() {
        AgentState.get().removeListener(onStateChange);
        stopAnimation();
        if (connectionMark != null) connectionMark.pause();
        super.onPause();
    }

    @Override
    protected void onDestroy() {
        stopAnimation();
        installer.close();
        io.shutdownNow();
        main.removeCallbacksAndMessages(null);
        super.onDestroy();
    }

    @Override
    public void onBackPressed() {
        if (busy) return;
        if (navigateBack != null) navigateBack.run();
        else super.onBackPressed();
    }

    private void route() {
        if (!prefs.setupDone()) {
            showPermissions();
            return;
        }
        if (!ready) {
            screen("loading", "wanctl", null);
            title("正在读取配置", "稍等片刻。");
            action("重新读取", false, this::probe);
            return;
        }
        if (page.equals("permissions")) {
            showPermissions();
            return;
        }
        if (page.equals("advanced")) {
            showAdvanced();
            return;
        }
        if (page.equals("details")) {
            showDetails();
            return;
        }
        if (page.equals("login")) {
            showLogin();
            return;
        }
        if (page.equals("settings")) {
            showSettings();
            return;
        }
        if (page.equals("custom")) {
            showCustom();
            return;
        }
        if (page.equals("instances")) {
            showInstances();
            return;
        }
        if (configuredRelay.isEmpty() || configuredPortal.isEmpty()) showInstances();
        else if (!loggedIn) showLogin();
        else showHome();
    }

    private int dp(float n) {
        return Math.round(n * getResources().getDisplayMetrics().density);
    }

    private GradientDrawable shape(int color, int radius) {
        GradientDrawable d = new GradientDrawable();
        d.setColor(color);
        d.setCornerRadius(dp(radius));
        return d;
    }

    private void screen(String next, String label, Runnable back) {
        stopAnimation();
        page = next;
        navigateBack = back;
        status = null;
        homeCaption = null;
        connectionMark = null;
        power = null;
        LinearLayout outer = new LinearLayout(this);
        outer.setOrientation(LinearLayout.VERTICAL);
        outer.setBackgroundColor(CANVAS);
        // API 35 edge-to-edge: keep every target clear of cutouts and system gestures.
        outer.setOnApplyWindowInsetsListener(
                (v, insets) -> {
                    android.graphics.Insets safe =
                            insets.getInsets(
                                    android.view.WindowInsets.Type.systemBars()
                                            | android.view.WindowInsets.Type.displayCutout());
                    v.setPadding(safe.left, safe.top, safe.right, safe.bottom);
                    return insets;
                });
        if (Build.VERSION.SDK_INT < 30) {
            outer.setOnApplyWindowInsetsListener(null);
            outer.setFitsSystemWindows(true);
        }
        LinearLayout header = new LinearLayout(this);
        header.setGravity(Gravity.CENTER_VERTICAL);
        header.setPadding(dp(16), dp(4), dp(16), dp(4));
        if (back != null) {
            Button arrow = button("‹", false, back);
            arrow.setTextSize(30);
            arrow.setTextColor(INK);
            arrow.setPadding(0, 0, 0, 0);
            arrow.setContentDescription("返回");
            header.addView(arrow, new LinearLayout.LayoutParams(dp(48), dp(48)));
        }
        TextView brand = text(label, next.equals("home") ? 22 : 18, INK);
        brand.setTypeface(Typeface.create("sans-serif-medium", Typeface.NORMAL));
        brand.setGravity(Gravity.CENTER_VERTICAL);
        if (back == null) brand.setPadding(dp(8), 0, 0, 0);
        header.addView(brand, new LinearLayout.LayoutParams(0, dp(52), 1));
        if (next.equals("home")) {
            Button setting = button("设置", false, this::showSettings);
            setting.setTextColor(INK);
            header.addView(setting, new LinearLayout.LayoutParams(dp(64), dp(48)));
        }
        outer.addView(header);
        ScrollView scroll = new ScrollView(this);
        scroll.setFillViewport(true);
        scroll.setClipToPadding(false);
        body = new LinearLayout(this);
        body.setOrientation(LinearLayout.VERTICAL);
        body.setPadding(dp(24), dp(20), dp(24), dp(24));
        scroll.addView(body, new ScrollView.LayoutParams(-1, -1));
        outer.addView(scroll, new LinearLayout.LayoutParams(-1, 0, 1));
        footer = new LinearLayout(this);
        footer.setOrientation(LinearLayout.VERTICAL);
        footer.setPadding(dp(24), dp(8), dp(24), dp(20));
        footer.setVisibility(View.GONE);
        outer.addView(footer, new LinearLayout.LayoutParams(-1, -2));
        setContentView(outer);
        outer.requestApplyInsets();
    }

    private TextView text(String value, int size, int color) {
        TextView t = new TextView(this);
        t.setText(value);
        t.setTextSize(size);
        t.setTextColor(color);
        t.setLineSpacing(dp(3), 1.12f);
        return t;
    }

    private void title(String heading, String detail) {
        TextView h = text(heading, 28, INK);
        h.setTypeface(Typeface.create("sans-serif-medium", 0));
        body.addView(h);
        gap(12);
        body.addView(text(detail, 16, MUTED));
        gap(24);
    }

    private void gap(int size) {
        View v = new View(this);
        body.addView(v, new LinearLayout.LayoutParams(1, dp(size)));
    }

    private void spring() {
        body.addView(new View(this), new LinearLayout.LayoutParams(1, 0, 1));
    }

    private Button button(String label, boolean primary, Runnable action) {
        Button b = new Button(this);
        b.setText(label);
        b.setTextSize(16);
        b.setAllCaps(false);
        b.setTextColor(primary ? Color.WHITE : BLUE);
        b.setPadding(dp(16), dp(12), dp(16), dp(12));
        b.setMinHeight(dp(52));
        b.setMinimumHeight(dp(52));
        b.setStateListAnimator(null);
        b.setElevation(0);
        b.setBackground(
                new RippleDrawable(
                        ColorStateList.valueOf(0x220066CC),
                        shape(primary ? BLUE : Color.TRANSPARENT, 28),
                        null));
        b.setOnClickListener(
                v -> {
                    if (!busy) action.run();
                });
        return b;
    }

    private Button action(String label, boolean primary, Runnable run) {
        Button b = button(label, primary, run);
        LinearLayout.LayoutParams p = new LinearLayout.LayoutParams(-1, -2);
        p.topMargin = dp(10);
        body.addView(b, p);
        return b;
    }

    private Button footerAction(String label, boolean primary, Runnable run) {
        footer.setVisibility(View.VISIBLE);
        Button b = button(label, primary, run);
        LinearLayout.LayoutParams p = new LinearLayout.LayoutParams(-1, -2);
        p.topMargin = dp(4);
        footer.addView(b, p);
        return b;
    }

    private LinearLayout group(String label) {
        if (!label.isEmpty()) {
            TextView heading = text(label, 13, MUTED);
            heading.setPadding(dp(4), dp(16), 0, dp(8));
            body.addView(heading);
        }
        LinearLayout group = new LinearLayout(this);
        group.setOrientation(LinearLayout.VERTICAL);
        group.setBackground(shape(Color.WHITE, 16));
        body.addView(group, new LinearLayout.LayoutParams(-1, -2));
        return group;
    }

    private void separator(LinearLayout group) {
        if (group.getChildCount() == 0) return;
        View line = new View(this);
        line.setBackgroundColor(0xFFF0F0F3);
        LinearLayout.LayoutParams p = new LinearLayout.LayoutParams(-1, dp(1));
        p.leftMargin = dp(16);
        p.rightMargin = dp(16);
        group.addView(line, p);
    }

    private LinearLayout listRow(LinearLayout group) {
        separator(group);
        LinearLayout row = new LinearLayout(this);
        row.setGravity(Gravity.CENTER_VERTICAL);
        row.setPadding(dp(16), dp(12), dp(16), dp(12));
        row.setMinimumHeight(dp(56));
        group.addView(row, new LinearLayout.LayoutParams(-1, -2));
        return row;
    }

    private void row(LinearLayout group, String label, String value, Runnable action) {
        LinearLayout row = listRow(group);
        TextView name = text(label, 16, INK);
        row.addView(name, new LinearLayout.LayoutParams(0, -2, 1));
        if (!value.isEmpty()) {
            TextView detail = text(value, 14, MUTED);
            detail.setMaxWidth(dp(130));
            detail.setMaxLines(1);
            detail.setEllipsize(android.text.TextUtils.TruncateAt.END);
            detail.setPadding(dp(8), 0, 0, 0);
            row.addView(detail);
        }
        if (action != null) {
            TextView chevron = text("›", 22, MUTED);
            chevron.setPadding(dp(10), 0, 0, 0);
            row.addView(chevron);
            row.setBackground(
                    new RippleDrawable(
                            ColorStateList.valueOf(0x11000000), null, shape(Color.WHITE, 12)));
            row.setFocusable(true);
            row.setOnClickListener(
                    v -> {
                        if (!busy) action.run();
                    });
        }
    }

    private void note(String message) {
        TextView note = text(message, 14, MUTED);
        note.setPadding(dp(4), dp(10), dp(4), dp(8));
        body.addView(note);
    }

    private EditText field(String label, String value) {
        body.addView(text(label, 14, MUTED));
        EditText e = new EditText(this);
        e.setSingleLine(true);
        e.setTextSize(16);
        e.setTextColor(INK);
        e.setText(value);
        e.setInputType(
                android.text.InputType.TYPE_CLASS_TEXT
                        | android.text.InputType.TYPE_TEXT_VARIATION_URI);
        e.setImportantForAutofill(View.IMPORTANT_FOR_AUTOFILL_NO);
        e.setPadding(dp(12), dp(12), dp(12), dp(12));
        e.setBackground(shape(0xFFF0F0F3, 12));
        LinearLayout.LayoutParams p = new LinearLayout.LayoutParams(-1, dp(56));
        p.topMargin = dp(8);
        p.bottomMargin = dp(20);
        body.addView(e, p);
        return e;
    }

    private void mark(boolean animate) {
        ImageView mark = new ImageView(this);
        mark.setImageResource(R.drawable.ic_launcher_foreground);
        mark.setBackground(shape(INK, 16));
        LinearLayout.LayoutParams p = new LinearLayout.LayoutParams(dp(64), dp(64));
        p.gravity = Gravity.CENTER_HORIZONTAL;
        p.topMargin = dp(16);
        p.bottomMargin = dp(24);
        body.addView(mark, p);
        if (animate && ValueAnimator.areAnimatorsEnabled()) {
            animation = ValueAnimator.ofFloat(.94f, 1f);
            animation.setDuration(1600);
            animation.setRepeatCount(ValueAnimator.INFINITE);
            animation.setRepeatMode(ValueAnimator.REVERSE);
            animation.setInterpolator(new DecelerateInterpolator());
            animation.addUpdateListener(
                    v -> {
                        float x = (float) v.getAnimatedValue();
                        mark.setScaleX(x);
                        mark.setScaleY(x);
                    });
            animation.start();
        }
    }

    private void stopAnimation() {
        if (animation != null) {
            animation.cancel();
            animation = null;
        }
    }

    private boolean notificationsGranted() {
        return getSystemService(android.app.NotificationManager.class).areNotificationsEnabled();
    }

    private boolean batteryGranted() {
        return getSystemService(PowerManager.class)
                .isIgnoringBatteryOptimizations(getPackageName());
    }

    private void showPermissions() {
        boolean fromSettings = permissionsFromSettings;
        screen(
                "permissions",
                fromSettings ? "通知与后台运行" : "初次设置",
                fromSettings ? this::showSettings : null);
        if (!fromSettings) {
            title("保持连接", "开启通知和后台运行，让连接在息屏后也能继续。");
            SetupGuide guide = new SetupGuide(this, notificationsGranted(), batteryGranted());
            body.addView(guide, new LinearLayout.LayoutParams(-1, dp(140)));
            gap(16);
        }
        LinearLayout permissions = group(fromSettings ? "运行权限" : "");
        row(
                permissions,
                "运行通知",
                notificationsGranted() ? "已开启" : "未开启",
                this::requestNotifications);
        row(
                permissions,
                "电池优化豁免",
                batteryGranted() ? "已允许" : "未允许",
                () -> {
                    if (batteryGranted())
                        openSettings(
                                new Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS));
                    else askBatteryExemption();
                });
        note("运行通知显示连接状态；电池优化豁免减少系统对后台连接的限制。");
        LinearLayout background = group("系统设置");
        row(background, "后台活动与自启动", "设置指引", this::showOEMBackgroundHelp);
        note("部分手机还需在系统中允许后台活动与自启动，具体选项以本机为准。");
        if (!fromSettings)
            footerAction(
                    notificationsGranted() && batteryGranted() ? "继续" : "暂时跳过",
                    true,
                    () -> {
                        prefs.setSetupDone();
                        page = "";
                        route();
                    });
    }

    private void requestNotifications() {
        if (Build.VERSION.SDK_INT >= 33
                && checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS)
                        != PackageManager.PERMISSION_GRANTED
                && !prefs.notificationAsked()) {
            prefs.setNotificationAsked();
            requestPermissions(new String[] {Manifest.permission.POST_NOTIFICATIONS}, 1);
        } else
            openSettings(
                    new Intent(Settings.ACTION_APP_NOTIFICATION_SETTINGS)
                            .putExtra(Settings.EXTRA_APP_PACKAGE, getPackageName()));
    }

    @Override
    public void onRequestPermissionsResult(int request, String[] permissions, int[] results) {
        super.onRequestPermissionsResult(request, permissions, results);
        if (page.equals("permissions")) showPermissions();
    }

    private String serviceName() {
        if (configuredPortal.isEmpty()) return "未配置";
        return HOSTED_PORTAL.equals(configuredPortal) && HOSTED_RELAY.equals(configuredRelay)
                ? "官方服务"
                : "自建服务";
    }

    private void showInstances() {
        screen(
                "instances",
                settingsFlow ? "服务" : "选择服务",
                settingsFlow
                        ? this::showSettings
                        : () -> {
                            permissionsFromSettings = false;
                            showPermissions();
                        });
        title(settingsFlow ? "当前连接" : "连接到哪里？", "使用官方服务，或连接自己的部署。");
        LinearLayout choices = group("");
        row(
                choices,
                "官方服务",
                serviceName().equals("官方服务") ? "当前使用" : "",
                () -> saveInstance(HOSTED_RELAY, HOSTED_PORTAL));
        row(choices, "自建服务", serviceName().equals("自建服务") ? "当前使用" : "", this::showCustom);
        note("官方服务使用 GitHub 登录，目前需要邀请。登录后可申请访问。");
        if (!configuredPortal.isEmpty()) note("当前门户：" + Uri.parse(configuredPortal).getHost());
    }

    private void showCustom() {
        screen("custom", "自建服务", this::showInstances);
        title("服务地址", "填写部署提供的中继和门户地址。");
        EditText relay = field("中继地址", draftRelay == null ? configuredRelay : draftRelay);
        relayField = relay;
        EditText portal =
                field(
                        "门户地址",
                        draftPortal == null
                                ? (configuredPortal.isEmpty() ? BuildInfo.PORTAL : configuredPortal)
                                : draftPortal);
        portalField = portal;
        relay.setHint("https://relay.example.com");
        portal.setHint("https://wanctl.example.com");
        footerAction(
                settingsFlow ? "保存" : "保存并继续",
                true,
                () -> saveInstance(relay.getText().toString(), portal.getText().toString()));
    }

    private String origin(String raw) {
        String s = raw.trim().replaceAll("/+$", "");
        Uri u = Uri.parse(s);
        if (!("https".equals(u.getScheme()) || "http".equals(u.getScheme()))
                || u.getHost() == null
                || u.getUserInfo() != null
                || u.getQuery() != null
                || u.getFragment() != null
                || (u.getPath() != null && !u.getPath().isEmpty()))
            throw new IllegalArgumentException("请填写完整的 http:// 或 https:// 服务地址，不要带路径、账号或查询参数。");
        return s;
    }

    private void saveInstance(String relayInput, String portalInput) {
        final String r, p;
        try {
            r = origin(relayInput);
            p = origin(portalInput);
        } catch (IllegalArgumentException e) {
            error("地址不完整", e.getMessage());
            return;
        }
        boolean changed = !r.equals(configuredRelay) || !p.equals(configuredPortal);
        Runnable save =
                () -> {
                    busy = true;
                    if (changed) {
                        prefs.setEnabled(false);
                        KeeperJob.cancel(this);
                        AgentService.stop(this);
                    }
                    io.execute(
                            () -> {
                                Wanctl.Result result =
                                        Wanctl.run(
                                                this,
                                                10,
                                                "config",
                                                "set",
                                                "relay=" + r,
                                                "portal=" + p);
                                if (result.ok() && changed) result = Wanctl.run(this, 10, "logout");
                                Wanctl.Result done = result;
                                main.post(
                                        () -> {
                                            busy = false;
                                            if (!done.ok()) {
                                                error("配置未保存", done.message());
                                                return;
                                            }
                                            prefs.setPortal(p);
                                            prefs.setLoginState("");
                                            configuredRelay = r;
                                            configuredPortal = p;
                                            if (changed) loggedIn = false;
                                            draftRelay = null;
                                            draftPortal = null;
                                            if (settingsFlow && !changed) showSettings();
                                            else {
                                                settingsFlow = false;
                                                page = "";
                                                route();
                                            }
                                        });
                            });
                };
        if (changed && loggedIn)
            new AlertDialog.Builder(this)
                    .setTitle("切换服务？")
                    .setMessage("将停用当前连接，并在新服务重新登录。")
                    .setNegativeButton("取消", null)
                    .setPositiveButton("切换", (d, w) -> save.run())
                    .show();
        else save.run();
    }

    private void showLogin() {
        screen(
                "login",
                settingsFlow ? "重新登录" : "登录",
                settingsFlow ? this::showSettings : this::showInstances);
        title(settingsFlow ? "重新授权设备" : "连接你的设备", "在浏览器中完成授权，再返回 wanctl。");
        LinearLayout service = group("连接服务");
        row(service, serviceName(), Uri.parse(configuredPortal).getHost(), null);
        boolean pendingLogin =
                !prefs.loginState().isEmpty()
                        && System.currentTimeMillis() - prefs.loginStarted() < 10 * 60 * 1000L;
        if (pendingLogin) note("已打开登录页面。完成授权后，点击网页中的「返回 wanctl」。");
        footerAction(pendingLogin ? "重新打开登录" : "使用 GitHub 登录", true, this::beginLogin);
        footerAction(
                "其他登录方式",
                false,
                () -> {
                    String[] options =
                            HOSTED_PORTAL.equals(configuredPortal)
                                    ? new String[] {"输入授权码"}
                                    : new String[] {"使用门户登录", "输入授权码"};
                    new AlertDialog.Builder(this)
                            .setTitle("其他登录方式")
                            .setItems(
                                    options,
                                    (d, which) -> {
                                        if (options[which].equals("输入授权码")) promptCode();
                                        else openEnrollment(false);
                                    })
                            .show();
                });
    }

    private void beginLogin() {
        openEnrollment(true);
    }

    private void openEnrollment(boolean github) {
        String state = UUID.randomUUID().toString();
        prefs.setLoginState(state);
        String next = "/enroll?mobile_state=" + Uri.encode(state);
        String url = configuredPortal + (github ? "/auth/github?next=" + Uri.encode(next) : next);
        // Keep OAuth state cookies in the external user agent. The portal redirects
        // to GitHub; Android/browser verified-link handling may hand off to its app.
        openUrl(url);
        showLogin();
    }

    private void receiveLogin(Intent intent) {
        Uri u = intent.getData();
        if (u == null) return;
        intent.setData(null);
        if (!"wanctl".equals(u.getScheme()) || !"enroll".equals(u.getHost())) return;
        String state = u.getQueryParameter("state"), code = u.getQueryParameter("code");
        if (state == null
                || prefs.loginState().isEmpty()
                || !state.equals(prefs.loginState())
                || System.currentTimeMillis() - prefs.loginStarted() > 10 * 60 * 1000L) {
            error("登录已失效", "请从此设备重新发起登录。");
            return;
        }
        if (code == null || code.trim().isEmpty()) {
            error("授权未完成", "门户没有返回授权码，请重试。");
            return;
        }
        prefs.setLoginState("");
        submitEnroll(code);
    }

    private void promptCode() {
        EditText code = new EditText(this);
        code.setSingleLine(true);
        code.setHint("粘贴一次性授权码");
        new AlertDialog.Builder(this)
                .setTitle("输入授权码")
                .setMessage("仅用于尚未支持返回 App 的旧版门户。")
                .setView(code)
                .setNegativeButton("取消", null)
                .setPositiveButton("登录", (d, w) -> submitEnroll(code.getText().toString().trim()))
                .show();
    }

    private void submitEnroll(String code) {
        if (code.isEmpty() || busy) return;
        busy = true;
        screen("authorizing", "wanctl", null);
        mark(true);
        title("正在完成授权", "请稍等，正在连接你的空间。");
        io.execute(
                () -> {
                    Wanctl.Result result = Wanctl.run(this, 60, "login", "--code", code);
                    main.post(
                            () -> {
                                busy = false;
                                stopAnimation();
                                if (result.ok()) {
                                    prefs.setLoginState("");
                                    loggedIn = true;
                                    page = "";
                                    probe();
                                    route();
                                    if (prefs.enabled()) AgentService.restart(this);
                                } else {
                                    showLogin();
                                    error("登录未完成", result.message());
                                }
                            });
                });
    }

    private void showHome() {
        settingsFlow = false;
        permissionsFromSettings = false;
        screen("home", "wanctl", null);
        spring();
        connectionMark = new ConnectionMark(this);
        LinearLayout.LayoutParams logoSize = new LinearLayout.LayoutParams(dp(128), dp(128));
        logoSize.gravity = Gravity.CENTER_HORIZONTAL;
        body.addView(connectionMark, logoSize);
        gap(8);
        TextView device =
                text(prefs.deviceName().isEmpty() ? Build.MODEL : prefs.deviceName(), 14, MUTED);
        device.setGravity(Gravity.CENTER);
        body.addView(device);
        gap(12);
        status = text("", 32, INK);
        status.setGravity(Gravity.CENTER);
        body.addView(status);
        gap(12);
        homeCaption = text("", 16, MUTED);
        homeCaption.setGravity(Gravity.CENTER);
        body.addView(homeCaption);
        spring();
        power =
                footerAction(
                        "启用",
                        true,
                        () -> {
                            if (prefs.enabled()) {
                                prefs.setEnabled(false);
                                KeeperJob.cancel(this);
                                AgentService.stop(this);
                            } else {
                                prefs.setEnabled(true);
                                KeeperJob.schedule(this);
                                AgentService.start(this);
                            }
                            renderState();
                        });
        renderState();
    }

    private void renderState() {
        if (!page.equals("home") || status == null) return;
        AgentState s = AgentState.get();
        connectionMark.setState(s.phase(), prefs.enabled());
        String label, detail;
        switch (s.phase()) {
            case ONLINE:
                label = "已连接";
                detail = "现在可以从你的空间远程连接这台手机。";
                break;
            case STARTING:
            case RETRYING:
                label = "正在连接";
                detail = "正在连接服务，请稍等。";
                break;
            case ERROR:
                label = "连接未完成";
                detail = "轻点状态查看原因，或前往设置重新登录。";
                break;
            default:
                label = prefs.enabled() ? "正在连接" : "未启用";
                detail = prefs.enabled() ? "正在启动连接，请稍等。" : "启用后，可从你的空间远程连接这台手机。";
        }
        status.setText(label);
        status.setTextColor(INK);
        homeCaption.setText(detail);
        power.setText(prefs.enabled() ? "停用" : "启用");
        status.setOnClickListener(
                s.phase() == AgentState.Phase.ERROR ? v -> error("连接未完成", s.detail()) : null);
    }

    private void showSettings() {
        settingsFlow = true;
        permissionsFromSettings = false;
        screen("settings", "设置", this::showHome);
        LinearLayout device = group("设备");
        row(
                device,
                "设备名称",
                prefs.deviceName().isEmpty() ? Build.MODEL : prefs.deviceName(),
                () -> {
                    EditText name = new EditText(this);
                    name.setSingleLine(true);
                    name.setText(prefs.deviceName());
                    new AlertDialog.Builder(this)
                            .setTitle("设备名称")
                            .setView(name)
                            .setNegativeButton("取消", null)
                            .setPositiveButton(
                                    "保存",
                                    (d, w) -> {
                                        prefs.setDeviceName(name.getText().toString());
                                        restartIfRunning();
                                        showSettings();
                                    })
                            .show();
                });
        toggle(device, "开机自动启用", prefs.bootStart(), prefs::setBootStart);
        row(
                device,
                "通知与后台运行",
                notificationsGranted() && batteryGranted() ? "已允许" : "待配置",
                () -> {
                    permissionsFromSettings = true;
                    showPermissions();
                });
        LinearLayout connection = group("连接");
        row(connection, "服务", serviceName(), this::showInstances);
        row(connection, "重新登录", loggedIn ? "已登录" : "未登录", this::showLogin);
        row(connection, "高级设置", "", this::showAdvanced);
        LinearLayout about = group("关于");
        row(about, "查看日志", "", () -> startActivity(new Intent(this, LogActivity.class)));
        row(about, "连接详情", "", this::showDetails);
        row(
                about,
                "检查更新",
                BuildInfo.UPDATES_SUPPORTED ? "" : "预览版",
                () -> installer.checkAndInstall());
        note(BuildInfo.UPDATES_SUPPORTED ? "wanctl " + BuildInfo.VERSION : "wanctl Preview · 预览版");
    }

    private void showAdvanced() {
        screen("advanced", "高级设置", this::showSettings);
        LinearLayout trust = group("控制端配对");
        toggle(
                trust,
                "自动信任新控制端",
                prefs.autoTrust(),
                v -> {
                    prefs.setAutoTrust(v);
                    restartIfRunning();
                });
        note("开启后，同一空间的新控制端可以直接配对，无需在门户逐次确认。");
        LinearLayout commands = group("命令审批");
        toggle(
                commands,
                "自动放行所有命令",
                prefs.bypass(),
                v -> {
                    prefs.setBypass(v);
                    restartIfRunning();
                });
        note("开启后，已配对的控制端可执行任意命令、读写文件，无需逐条批准。请仅在信任这些控制端时开启。");
        LinearLayout elevation = group("系统控制");
        toggle(
                elevation,
                "提权通道",
                prefs.elevation(),
                v -> {
                    if (!v) {
                        prefs.setElevation(false);
                        restartIfRunning();
                    } else showElevationHelp();
                });
        row(elevation, "配置系统权限", "设置指引", this::showElevationHelp);
        note("截图、模拟点击等系统操作需要额外配置无线调试或 root。仅在使用这些功能时配置。");
    }

    private void showDetails() {
        screen("details", "连接详情", this::showSettings);
        for (String[] item :
                new String[][] {
                    {"设备 ID", deviceId},
                    {"门户", configuredPortal},
                    {"中继", configuredRelay},
                    {"设备指纹", fingerprint},
                    {"登录状态", credential}
                }) {
            LinearLayout group = group(item[0]);
            TextView value = text(item[1], 15, INK);
            value.setTextIsSelectable(true);
            value.setPadding(dp(16), dp(14), dp(16), dp(14));
            group.addView(value);
        }
    }

    private interface Change {
        void set(boolean v);
    }

    private void toggle(LinearLayout group, String label, boolean checked, Change change) {
        LinearLayout row = listRow(group);
        TextView name = text(label, 16, INK);
        row.addView(name, new LinearLayout.LayoutParams(0, -2, 1));
        Switch control = new Switch(this);
        control.setContentDescription(label);
        control.setShowText(false);
        control.setSwitchMinWidth(dp(48));
        GradientDrawable thumb = shape(Color.WHITE, 14);
        thumb.setSize(dp(26), dp(26));
        thumb.setStroke(dp(1), 0xFFE5E5EA);
        control.setThumbDrawable(
                new android.graphics.drawable.InsetDrawable(thumb, 0, dp(2), 0, dp(2)));
        android.graphics.drawable.StateListDrawable track =
                new android.graphics.drawable.StateListDrawable();
        GradientDrawable on = shape(BLUE, 16);
        on.setSize(dp(48), dp(30));
        GradientDrawable off = shape(0xFFD1D1D6, 16);
        off.setSize(dp(48), dp(30));
        track.addState(new int[] {android.R.attr.state_checked}, on);
        track.addState(new int[] {}, off);
        control.setTrackDrawable(track);
        control.setThumbTintList(null);
        control.setTrackTintList(null);
        control.setChecked(checked);
        control.setMinimumHeight(dp(48));
        row.setPadding(dp(16), dp(4), dp(16), dp(4));
        row.addView(control);
        control.setOnCheckedChangeListener((b, value) -> change.set(value));
        row.setOnClickListener(v -> control.setChecked(!control.isChecked()));
    }

    private void showElevationHelp() {
        new AlertDialog.Builder(this)
                .setTitle("配置提权通道")
                .setMessage(
                        "仅在需要控制系统、截图或模拟点击时开启。\n\n"
                                + "无线调试：在开发者选项中启用无线调试，再从门户终端完成 adb 配对。\n\n"
                                + "已 root 的手机可使用 root 通道。开启后仍需按系统提示授权。")
                .setNegativeButton("取消", (d, w) -> showAdvanced())
                .setNeutralButton(
                        "打开开发者选项",
                        (d, w) -> {
                            openSettings(
                                    new Intent(Settings.ACTION_APPLICATION_DEVELOPMENT_SETTINGS));
                            showAdvanced();
                        })
                .setPositiveButton(
                        "启用通道",
                        (d, w) -> {
                            prefs.setElevation(true);
                            restartIfRunning();
                            showAdvanced();
                        })
                .setOnCancelListener(d -> showAdvanced())
                .show();
    }

    private void restartIfRunning() {
        if (prefs.enabled()) AgentService.restart(this);
    }

    private void askBatteryExemption() {
        if (batteryGranted()) {
            showOEMBackgroundHelp();
            return;
        }
        openSettings(
                new Intent(
                        Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS,
                        Uri.parse("package:" + getPackageName())));
    }

    private void showOEMBackgroundHelp() {
        String vendor = Build.MANUFACTURER.toLowerCase(java.util.Locale.ROOT);
        String detail;
        if (vendor.contains("oppo") || vendor.contains("oneplus") || vendor.contains("realme"))
            detail = "在应用信息中打开「耗电管理」，允许后台活动；如有「自启动」选项，也请开启。";
        else if (vendor.contains("vivo")) detail = "在系统电池设置的「后台耗电管理」中找到 wanctl，选择「允许后台高耗电」，并允许自启动。";
        else if (vendor.contains("xiaomi") || vendor.contains("redmi"))
            detail = "在应用信息的「省电策略」中选择「无限制」，并允许自启动。";
        else if (vendor.contains("huawei") || vendor.contains("honor"))
            detail = "在电池设置的「应用启动管理」中找到 wanctl，关闭自动管理，允许自启动、关联启动和后台活动。";
        else detail = "在应用信息的电池设置中允许后台运行；如果系统提供自启动开关，也请开启。";
        new AlertDialog.Builder(this)
                .setTitle("允许后台运行")
                .setMessage(detail + "\n\n不同系统版本的名称可能略有不同。")
                .setNegativeButton("关闭", null)
                .setPositiveButton(
                        "打开应用设置",
                        (d, w) ->
                                openSettings(
                                        new Intent(
                                                Settings.ACTION_APPLICATION_DETAILS_SETTINGS,
                                                Uri.parse("package:" + getPackageName()))))
                .show();
    }

    private void openSettings(Intent intent) {
        try {
            startActivity(intent);
        } catch (ActivityNotFoundException e) {
            error("请手动打开系统设置", "在应用管理中找到 wanctl，配置对应权限。");
        }
    }

    private void openUrl(String url) {
        try {
            startActivity(
                    new Intent(Intent.ACTION_VIEW, Uri.parse(url))
                            .addCategory(Intent.CATEGORY_BROWSABLE));
        } catch (ActivityNotFoundException e) {
            error("未找到浏览器", "安装或启用浏览器后，再次点击登录。");
        }
    }

    private void error(String title, String message) {
        if (!isFinishing() && !isDestroyed())
            new AlertDialog.Builder(this)
                    .setTitle(title)
                    .setMessage(message)
                    .setPositiveButton("确定", null)
                    .show();
    }

    private static String line(String text, String key) {
        for (String l : text.split("\n")) {
            int i = l.indexOf(key);
            if (i >= 0) return l.substring(i + key.length()).trim();
        }
        return "";
    }

    private void probe() {
        if (probing || busy) return;
        probing = true;
        io.execute(
                () -> {
                    Wanctl.Result result = Wanctl.run(this, 10, "status");
                    Wanctl.Result id = Wanctl.run(this, 10, "id");
                    main.post(
                            () -> {
                                probing = false;
                                if (isDestroyed()) return;
                                if (!result.ok()) {
                                    error("无法读取设备配置", result.message());
                                    return;
                                }
                                configuredRelay = line(result.out, "relay:");
                                configuredPortal = line(result.out, "portal:");
                                if (configuredRelay.equals("未配置")) configuredRelay = "";
                                if (configuredPortal.equals("未配置")) configuredPortal = "";
                                credential = line(result.out, "凭证:");
                                loggedIn = credential.contains("已登录");
                                fingerprint = line(id.out, "fingerprint:");
                                deviceId = line(id.out, "device ID:");
                                boolean first = !ready;
                                ready = true;
                                if (first && !pendingPage.isEmpty()) {
                                    page = pendingPage;
                                    pendingPage = "";
                                }
                                if (loggedIn
                                        && !configuredRelay.isEmpty()
                                        && prefs.enabled()
                                        && !AgentService.isRunning()) {
                                    AgentService.start(this);
                                    KeeperJob.schedule(this);
                                }
                                if (first || page.equals("loading") || page.equals("authorizing"))
                                    route();
                                else renderState();
                            });
                });
    }
}
