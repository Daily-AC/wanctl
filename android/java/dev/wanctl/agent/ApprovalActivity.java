package dev.wanctl.agent;

import android.app.Activity;
import android.content.Intent;
import android.content.res.ColorStateList;
import android.graphics.Color;
import android.graphics.Typeface;
import android.graphics.drawable.GradientDrawable;
import android.graphics.drawable.RippleDrawable;
import android.os.Build;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.view.Gravity;
import android.view.View;
import android.widget.Button;
import android.widget.LinearLayout;
import android.widget.ScrollView;
import android.widget.TextView;

/**
 * One approval card in full: who asks, on which device, and the whole command (ADR 0015).
 *
 * <p>Deliberately not allowed over the keyguard: tapping the notification on a locked phone makes
 * Android ask for the unlock first, and that is the only way the command reaches the screen. Not
 * exported either; only the notification opens it. Same look as MainActivity.
 */
public final class ApprovalActivity extends Activity {
    private static final int INK = Color.rgb(29, 29, 31), MUTED = Color.rgb(105, 105, 110);
    private static final int BLUE = Color.rgb(0, 102, 204), CANVAS = Color.rgb(250, 250, 252);
    private final Runnable onChange = this::render;
    private final Handler main = new Handler(Looper.getMainLooper());
    private String id = "";
    private TextView subtitle;
    private boolean shownSlow;
    /** Moves the countdown, and redraws once when a submission turns slow; nothing else. */
    private final Runnable tick = this::tick;
    private LinearLayout body, footer;

    @Override
    protected void onCreate(Bundle state) {
        super.onCreate(state);
        if (getActionBar() != null) getActionBar().hide();
        id = ApprovalNotifier.cardId(getIntent().getData());
        frame();
    }

    @Override
    protected void onNewIntent(Intent intent) {
        super.onNewIntent(intent);
        setIntent(intent);
        id = ApprovalNotifier.cardId(intent.getData());
    }

    @Override
    protected void onResume() {
        super.onResume();
        ApprovalNotifier.listen(onChange);
        render();
    }

    @Override
    protected void onPause() {
        ApprovalNotifier.unlisten(onChange);
        main.removeCallbacksAndMessages(null);
        super.onPause();
    }

    private void render() {
        main.removeCallbacksAndMessages(null);
        body.removeAllViews();
        footer.removeAllViews();
        footer.setVisibility(View.GONE);
        ApprovalNotifier.Card k = id.isEmpty() ? null : ApprovalNotifier.card(this, id);
        if (k == null || ApprovalNotifier.TEST.equals(k.state)) {
            heading("找不到这条请求", "它可能已处理或已失效。");
            return;
        }
        String left = remaining(k);
        subtitle = heading(ApprovalNotifier.title(k), ago(k.created) + (left.isEmpty() ? "" : " · " + left));
        shownSlow = ApprovalNotifier.slow(id);
        if (k.pairing()) {
            body.addView(text("这个控制端第一次连接。信任之后它就能控制这台设备。", 15, MUTED));
            gap(8);
        }
        if (!k.cmd.isEmpty()) block("命令", k.cmd);
        if (!k.path.isEmpty()) block("文件", k.path);
        if (!k.cwd.isEmpty()) block("工作目录", k.cwd);
        LinearLayout from = group("来源");
        row(from, "设备", k.device);
        row(from, "控制端", ApprovalNotifier.peerName(k));
        if (!k.peerFp.isEmpty()) row(from, "指纹", ApprovalNotifier.shortFingerprint(k.peerFp));
        String[] choices = ApprovalNotifier.choices(k);
        String status = ApprovalNotifier.status(k);
        if (choices != null) {
            if (status != null) outcome(status, false);
            footerButton(choices[0], true, () -> decide(k, "y"));
            footerButton(choices[1], false, () -> decide(k, "n"));
        } else if (ApprovalNotifier.verdict(id) != null) {
            submitting();
        } else if (status != null) {
            outcome(status, true);
            footerButton("完成", false, this::finish);
        }
        // The countdown and the 「提交中」 deadline both move with the clock.
        if (!left.isEmpty() || ApprovalNotifier.verdict(id) != null && !shownSlow)
            main.postDelayed(tick, 1000);
    }

    private void tick() {
        ApprovalNotifier.Card k = ApprovalNotifier.card(this, id);
        if (k == null) return;
        if (ApprovalNotifier.verdict(id) != null && ApprovalNotifier.slow(id) && !shownSlow) {
            render();
            return;
        }
        String left = remaining(k);
        subtitle.setText(ago(k.created) + (left.isEmpty() ? "" : " · " + left));
        if (!left.isEmpty() || ApprovalNotifier.verdict(id) != null && !shownSlow)
            main.postDelayed(tick, 1000);
    }

    /**
     * The answer is on its way. Until the final card comes back the button stays where the owner
     * tapped, saying so; past SUBMIT_SLOW_MS the screen says it has not arrived and offers to
     * send it again, instead of a spinner that never ends.
     */
    private void submitting() {
        if (!ApprovalNotifier.slow(id)) {
            Button b = footerButton("提交中…", true, () -> {});
            b.setEnabled(false);
            b.setAlpha(.6f);
            return;
        }
        Prefs prefs = new Prefs(this);
        if (!prefs.enabled()) {
            outcome("还没送达：wanctl 已停用", false);
            footer.addView(note("启用后会把这个决定送出去。"));
            footerButton("启用并重新提交", true, () -> {
                prefs.setEnabled(true);
                KeeperJob.schedule(this);
                AgentService.retry(this, id);
                render();
            });
            return;
        }
        outcome("还没送达", false);
        footer.addView(note("手机可能没联网，或 wanctl 没连上服务。联网后会自动重发，也可以现在再发一次。"));
        footerButton("重新提交", true, () -> {
            AgentService.retry(this, id);
            render();
        });
    }

    /** The result in words the owner cannot miss, above the buttons. */
    private void outcome(String text, boolean done) {
        footer.setVisibility(View.VISIBLE);
        TextView t = text(text, done ? 20 : 17, INK);
        t.setTypeface(Typeface.create("sans-serif-medium", Typeface.NORMAL));
        t.setGravity(Gravity.CENTER);
        t.setPadding(0, dp(4), 0, dp(12));
        footer.addView(t);
    }

    private TextView note(String text) {
        TextView t = text(text, 14, MUTED);
        t.setGravity(Gravity.CENTER);
        t.setPadding(0, 0, 0, dp(8));
        return t;
    }

    /** 「还剩 2:35」 for a card still waiting, empty otherwise. */
    static String remaining(ApprovalNotifier.Card k) {
        if (!ApprovalNotifier.PENDING.equals(k.state) || k.expires == 0) return "";
        long s = (k.expires - System.currentTimeMillis()) / 1000;
        if (s <= 0) return "";
        return String.format(java.util.Locale.ROOT, "还剩 %d:%02d", s / 60, s % 60);
    }

    /** Answers only what is on screen: the card may have moved on while it was open. */
    private void decide(ApprovalNotifier.Card shown, String verdict) {
        ApprovalNotifier.Card now = ApprovalNotifier.card(this, id);
        if (now == null || !now.state.equals(shown.state) || ApprovalNotifier.choices(now) == null) {
            render();
            return;
        }
        boolean ignore = ApprovalNotifier.EXPIRED.equals(now.state) && "n".equals(verdict);
        AgentService.decide(this, id, verdict);
        if (ignore) finish();
        else render();
    }

    private static String ago(long at) {
        long minutes = (System.currentTimeMillis() - at) / 60_000;
        if (minutes < 1) return "刚刚";
        if (minutes < 60) return minutes + " 分钟前";
        return minutes / 60 + " 小时前";
    }

    // ---------------------------------------------------------------- layout, as in MainActivity

    private void frame() {
        LinearLayout outer = new LinearLayout(this);
        outer.setOrientation(LinearLayout.VERTICAL);
        outer.setBackgroundColor(CANVAS);
        if (Build.VERSION.SDK_INT >= 30) {
            outer.setOnApplyWindowInsetsListener(
                    (v, insets) -> {
                        android.graphics.Insets safe =
                                insets.getInsets(
                                        android.view.WindowInsets.Type.systemBars()
                                                | android.view.WindowInsets.Type.displayCutout());
                        v.setPadding(safe.left, safe.top, safe.right, safe.bottom);
                        return insets;
                    });
        } else {
            outer.setFitsSystemWindows(true);
        }
        LinearLayout header = new LinearLayout(this);
        header.setGravity(Gravity.CENTER_VERTICAL);
        header.setPadding(dp(16), dp(4), dp(16), dp(4));
        Button back = button("‹", false, this::finish);
        back.setTextSize(30);
        back.setTextColor(INK);
        back.setPadding(0, 0, 0, 0);
        back.setContentDescription("返回");
        header.addView(back, new LinearLayout.LayoutParams(dp(48), dp(48)));
        TextView label = text("审批", 18, INK);
        label.setTypeface(Typeface.create("sans-serif-medium", Typeface.NORMAL));
        label.setGravity(Gravity.CENTER_VERTICAL);
        header.addView(label, new LinearLayout.LayoutParams(0, dp(52), 1));
        outer.addView(header);
        ScrollView scroll = new ScrollView(this);
        scroll.setFillViewport(true);
        body = new LinearLayout(this);
        body.setOrientation(LinearLayout.VERTICAL);
        body.setPadding(dp(24), dp(12), dp(24), dp(24));
        scroll.addView(body, new ScrollView.LayoutParams(-1, -2));
        outer.addView(scroll, new LinearLayout.LayoutParams(-1, 0, 1));
        footer = new LinearLayout(this);
        footer.setOrientation(LinearLayout.VERTICAL);
        footer.setPadding(dp(24), dp(8), dp(24), dp(20));
        outer.addView(footer, new LinearLayout.LayoutParams(-1, -2));
        setContentView(outer);
        outer.requestApplyInsets();
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

    private TextView text(String value, int size, int color) {
        TextView t = new TextView(this);
        t.setText(value);
        t.setTextSize(size);
        t.setTextColor(color);
        t.setLineSpacing(dp(3), 1.12f);
        return t;
    }

    private void gap(int size) {
        body.addView(new View(this), new LinearLayout.LayoutParams(1, dp(size)));
    }

    private TextView heading(String title, String detail) {
        TextView h = text(title, 24, INK);
        h.setTypeface(Typeface.create("sans-serif-medium", Typeface.NORMAL));
        body.addView(h);
        gap(8);
        TextView d = text(detail, 15, MUTED);
        body.addView(d);
        gap(16);
        return d;
    }

    private LinearLayout group(String label) {
        TextView heading = text(label, 13, MUTED);
        heading.setPadding(dp(4), dp(16), 0, dp(8));
        body.addView(heading);
        LinearLayout group = new LinearLayout(this);
        group.setOrientation(LinearLayout.VERTICAL);
        group.setBackground(shape(Color.WHITE, 16));
        body.addView(group, new LinearLayout.LayoutParams(-1, -2));
        return group;
    }

    /** The command, path or directory in full: monospace, wrapped, selectable. */
    private void block(String label, String value) {
        TextView t = text(value, 14, INK);
        t.setTypeface(Typeface.MONOSPACE);
        t.setTextIsSelectable(true);
        t.setPadding(dp(16), dp(14), dp(16), dp(14));
        group(label).addView(t);
    }

    private void row(LinearLayout group, String label, String value) {
        if (group.getChildCount() > 0) {
            View line = new View(this);
            line.setBackgroundColor(0xFFF0F0F3);
            LinearLayout.LayoutParams p = new LinearLayout.LayoutParams(-1, dp(1));
            p.leftMargin = dp(16);
            p.rightMargin = dp(16);
            group.addView(line, p);
        }
        LinearLayout row = new LinearLayout(this);
        row.setGravity(Gravity.CENTER_VERTICAL);
        row.setPadding(dp(16), dp(12), dp(16), dp(12));
        row.setMinimumHeight(dp(52));
        row.addView(text(label, 16, INK));
        TextView v = text(value, 14, MUTED);
        v.setGravity(Gravity.END);
        v.setTextIsSelectable(true);
        v.setPadding(dp(16), 0, 0, 0);
        row.addView(v, new LinearLayout.LayoutParams(0, -2, 1));
        group.addView(row, new LinearLayout.LayoutParams(-1, -2));
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
        b.setOnClickListener(v -> action.run());
        return b;
    }

    private Button footerButton(String label, boolean primary, Runnable action) {
        footer.setVisibility(View.VISIBLE);
        LinearLayout.LayoutParams p = new LinearLayout.LayoutParams(-1, -2);
        p.topMargin = dp(4);
        Button b = button(label, primary, action);
        footer.addView(b, p);
        return b;
    }
}
