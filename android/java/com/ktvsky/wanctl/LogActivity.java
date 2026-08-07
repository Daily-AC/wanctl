package com.***REMOVED***.wanctl;

import android.app.Activity;
import android.os.Bundle;
import android.view.Menu;
import android.view.MenuItem;
import android.widget.ScrollView;
import android.widget.TextView;
import android.widget.Toast;

import java.io.File;
import java.io.IOException;
import java.io.RandomAccessFile;
import java.nio.charset.StandardCharsets;

/** The agent's own stdout, live and from disk. Read-only; the fix is never in here. */
public final class LogActivity extends Activity {
    private static final long TAIL_BYTES = 256 * 1024;

    private TextView view;
    private ScrollView scroll;
    private final Runnable onChange = this::refresh;

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        setContentView(R.layout.activity_log);
        view = findViewById(R.id.log);
        scroll = findViewById(R.id.scroll);
        if (getActionBar() != null) {
            getActionBar().setDisplayHomeAsUpEnabled(true);
        }
    }

    @Override
    protected void onResume() {
        super.onResume();
        AgentState.get().addListener(onChange);
        refresh();
    }

    @Override
    protected void onPause() {
        AgentState.get().removeListener(onChange);
        super.onPause();
    }

    @Override
    public boolean onCreateOptionsMenu(Menu menu) {
        menu.add(0, 1, 0, "复制全部").setShowAsAction(MenuItem.SHOW_AS_ACTION_NEVER);
        menu.add(0, 2, 0, "清空").setShowAsAction(MenuItem.SHOW_AS_ACTION_NEVER);
        return true;
    }

    @Override
    public boolean onOptionsItemSelected(MenuItem item) {
        switch (item.getItemId()) {
            case android.R.id.home:
                finish();
                return true;
            case 1:
                android.content.ClipboardManager cm = getSystemService(android.content.ClipboardManager.class);
                cm.setPrimaryClip(android.content.ClipData.newPlainText("wanctl log", view.getText()));
                Toast.makeText(this, "已复制", Toast.LENGTH_SHORT).show();
                return true;
            case 2:
                //noinspection ResultOfMethodCallIgnored
                Wanctl.logFile(this).delete();
                refresh();
                return true;
            default:
                return super.onOptionsItemSelected(item);
        }
    }

    private void refresh() {
        StringBuilder sb = new StringBuilder();
        String persisted = tail(Wanctl.logFile(this));
        if (!persisted.isEmpty()) {
            sb.append(persisted);
        }
        String live = AgentState.get().logText();
        // The in-memory ring is the authority for this run; the file may end
        // mid-line if the process was killed while writing.
        if (!live.isEmpty()) {
            if (sb.length() > 0) {
                sb.append("\n──── 本次运行 ────\n");
            }
            sb.append(live);
        }
        if (sb.length() == 0) {
            sb.append("(暂无日志)");
        }
        view.setText(sb.toString());
        scroll.post(() -> scroll.fullScroll(ScrollView.FOCUS_DOWN));
    }

    private static String tail(File f) {
        if (!f.exists()) {
            return "";
        }
        try (RandomAccessFile raf = new RandomAccessFile(f, "r")) {
            long len = raf.length();
            long from = Math.max(0, len - TAIL_BYTES);
            raf.seek(from);
            byte[] buf = new byte[(int) (len - from)];
            raf.readFully(buf);
            String s = new String(buf, StandardCharsets.UTF_8);
            // A truncated first line is noise, not history.
            int nl = from > 0 ? s.indexOf('\n') : -1;
            return nl >= 0 ? s.substring(nl + 1) : s;
        } catch (IOException e) {
            return "(读取日志失败: " + e.getMessage() + ")";
        }
    }
}
