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
    private String id = "";
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
        ApprovalNotifier.listen(null);
        super.onPause();
    }

    private void render() {
        body.removeAllViews();
        footer.removeAllViews();
        footer.setVisibility(View.GONE);
        ApprovalNotifier.Card k = id.isEmpty() ? null : ApprovalNotifier.card(this, id);
        if (k == null || ApprovalNotifier.TEST.equals(k.state)) {
            heading("找不到这条请求", "它可能已处理或已失效。");
            return;
        }
        String status = ApprovalNotifier.status(k);
        heading(ApprovalNotifier.title(k), ago(k.created) + (status == null ? "" : " · " + status));
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
        if (choices != null) {
            footerButton(choices[0], true, () -> decide(k, "y"));
            footerButton(choices[1], false, () -> decide(k, "n"));
        }
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

    private void heading(String title, String detail) {
        TextView h = text(title, 24, INK);
        h.setTypeface(Typeface.create("sans-serif-medium", Typeface.NORMAL));
        body.addView(h);
        gap(8);
        body.addView(text(detail, 15, MUTED));
        gap(16);
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

    private void footerButton(String label, boolean primary, Runnable action) {
        footer.setVisibility(View.VISIBLE);
        LinearLayout.LayoutParams p = new LinearLayout.LayoutParams(-1, -2);
        p.topMargin = dp(4);
        footer.addView(button(label, primary, action), p);
    }
}
