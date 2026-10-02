package dev.wanctl.agent;

import android.app.KeyguardManager;
import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.content.Context;
import android.content.Intent;
import android.net.Uri;
import android.os.Build;
import android.os.Handler;
import android.os.Looper;
import android.os.PowerManager;
import android.service.notification.StatusBarNotification;

import org.json.JSONException;
import org.json.JSONObject;

import java.time.OffsetDateTime;
import java.time.format.DateTimeParseException;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * Turns the approval cards the portal pushes to this phone into notifications (ADR 0015).
 *
 * <p>A card carries the text of a command someone else wants to run, so nothing here shows it
 * while the keyguard is up. The public version alone does not guarantee that: it only replaces
 * the notification on a lock screen set to hide sensitive content, Android's default is to show
 * it, and the system resets a lock-screen visibility an app gives its own channel. So the content
 * follows the lock state instead — locked, a card is the one line 「有 1 个待审批请求」 with no
 * actions — and AgentService re-renders every card when the screen goes off and when the user
 * unlocks. The public version stays as the second line of defence.
 *
 * <p>The latest card per id is kept here, in memory, for the detail screen, and inside the
 * notification's own extras, so a process restart does not strand a notification whose card the
 * app no longer knows.
 */
final class ApprovalNotifier {
    static final String CHANNEL = "approval";
    /** The card id is the notification tag; 1 is the service's own notification. */
    private static final int ID = 2;
    private static final int KEEP = 32;
    private static final long DONE_LINGER_MS = 60_000;
    private static final String EXTRA_CARD = "dev.wanctl.agent.card";
    private static final String SCHEME = "wanctl-approval";

    static final String PENDING = "pending", EXPIRED = "expired", DONE = "done", TEST = "test";
    private static final String PAIR = "pair";

    private static final Handler main = new Handler(Looper.getMainLooper());
    private static final Map<String, Card> cards = recent();
    /** Decisions the owner made that no final card has answered yet; AgentService resends them. */
    private static final Map<String, String> verdicts = recent();
    /** When each of those decisions was made, so a screen can say it has not arrived yet. */
    private static final Map<String, Long> decidedAt = recent();
    /** How long 「提交中」 may last before the screen offers to send it again. */
    static final long SUBMIT_SLOW_MS = 10_000;
    private static final String LOCAL_TEST = "local-test";
    private static Runnable listener;

    private ApprovalNotifier() {
    }

    static final class Card {
        final String json, id, state, kind, device, peer, peerFp, cmd, path, cwd, result;
        final long created, expires;
        /** When this process got the card; a final card lingers from here, however often it is redrawn. */
        final long seen = System.currentTimeMillis();

        private Card(String json, JSONObject o) {
            this.json = json;
            id = text(o, "id");
            state = text(o, "state");
            kind = text(o, "kind");
            device = text(o, "device");
            peer = text(o, "peer");
            peerFp = text(o, "peer_fp");
            cmd = text(o, "cmd");
            path = text(o, "path");
            cwd = text(o, "cwd");
            result = text(o, "result");
            long t = time(text(o, "created"));
            created = t > 0 ? t : System.currentTimeMillis();
            expires = time(text(o, "expires"));
        }

        /** Null for anything that is not a card this app knows how to show. */
        static Card parse(String json) {
            Card k;
            try {
                k = new Card(json, new JSONObject(json));
            } catch (JSONException e) {
                return null;
            }
            boolean known = PENDING.equals(k.state) || EXPIRED.equals(k.state)
                    || DONE.equals(k.state) || TEST.equals(k.state);
            return k.id.isEmpty() || !known ? null : k;
        }

        boolean pairing() {
            return PAIR.equals(kind);
        }

        private static String text(JSONObject o, String key) {
            return o.isNull(key) ? "" : o.optString(key);
        }

        /** Go writes RFC 3339 with nanoseconds, and a zero time as year 1. */
        private static long time(String s) {
            if (s.isEmpty()) {
                return 0;
            }
            try {
                return OffsetDateTime.parse(s).toInstant().toEpochMilli();
            } catch (DateTimeParseException e) {
                return 0;
            }
        }
    }

    // ---------------------------------------------------------------- the store

    /** Handles one `wanctl-approval` line from the agent; false if it is not a card. */
    static boolean onLine(Context c, String json) {
        Card k = Card.parse(json);
        if (k == null) {
            return false;
        }
        boolean showing = active(c, k.id) != null;
        synchronized (ApprovalNotifier.class) {
            cards.put(k.id, k);
            if (DONE.equals(k.state)) {
                verdicts.remove(k.id);
            }
        }
        if (TEST.equals(k.state) && !k.id.startsWith(LOCAL_TEST)) {
            new Prefs(c).setApprovalPhone();
        }
        // A final state only updates a notification that is still there: one the
        // owner swiped away or ignored stays gone.
        if (!DONE.equals(k.state) || showing) {
            post(c, k, locked(c));
        }
        changed();
        return true;
    }

    /** The latest card for id, or null if neither this process nor the notification has it. */
    static Card card(Context c, String id) {
        synchronized (ApprovalNotifier.class) {
            Card k = cards.get(id);
            if (k != null) {
                return k;
            }
        }
        StatusBarNotification s = active(c, id);
        Card k = s == null ? null : Card.parse(s.getNotification().extras.getString(EXTRA_CARD, ""));
        if (k != null) {
            synchronized (ApprovalNotifier.class) {
                cards.put(id, k);
            }
        }
        return k;
    }

    static synchronized String verdict(String id) {
        return verdicts.get(id);
    }

    static synchronized Map<String, String> undecided() {
        return new LinkedHashMap<>(verdicts);
    }

    /** True once a decision has waited longer than SUBMIT_SLOW_MS for its final card. */
    static synchronized boolean slow(String id) {
        Long at = decidedAt.get(id);
        return verdicts.containsKey(id) && at != null
                && System.currentTimeMillis() - at > SUBMIT_SLOW_MS;
    }

    /** Restarts the 「提交中」 clock for a decision the owner chose to send again. */
    static synchronized void resent(String id) {
        if (verdicts.containsKey(id)) {
            decidedAt.put(id, System.currentTimeMillis());
        }
    }

    /**
     * What the home screen lists: requests still open, those the owner answered whose final card
     * has not come back yet, and expired ones whose notification is still up. Oldest first, the
     * order they are worked through. A pending card the owner swiped out of the shade stays here:
     * the shade is not the only way in any more. A final card leaves the list the moment it
     * arrives; its result stays in the notification and on the detail screen (v0.20.3 kept it here
     * for a minute and nothing redrew the screen when that minute was up).
     */
    static List<Card> open(Context c) {
        Map<String, Card> all = new LinkedHashMap<>();
        for (StatusBarNotification s : nm(c).getActiveNotifications()) {
            if (s.getId() == ID && s.getTag() != null) {
                Card k = card(c, s.getTag());
                if (k != null) {
                    all.put(k.id, k);
                }
            }
        }
        synchronized (ApprovalNotifier.class) {
            all.putAll(cards);
        }
        long now = System.currentTimeMillis();
        List<Card> out = new ArrayList<>();
        for (Card k : all.values()) {
            boolean showing = active(c, k.id) != null;
            boolean keep;
            if (DONE.equals(k.state) || TEST.equals(k.state)) {
                keep = false;
            } else if (verdict(k.id) != null) {
                keep = true;
            } else if (PENDING.equals(k.state)) {
                keep = k.expires == 0 || k.expires > now || showing;
            } else {
                keep = showing && !k.pairing();
            }
            if (keep) {
                out.add(k);
            }
        }
        out.sort((a, b) -> Long.compare(a.created, b.created));
        return out;
    }

    /** Posts a test card from the app itself, for the setup step that asks whether it rang. */
    static void test(Context c) {
        Card k = Card.parse("{\"id\":\"" + LOCAL_TEST + "\",\"state\":\"test\"}");
        post(c, k, false);
    }

    /** Records the owner's answer and shows 「正在提交…」 until the final card arrives. */
    static void decided(Context c, String id, String verdict) {
        Card k = card(c, id);
        synchronized (ApprovalNotifier.class) {
            verdicts.put(id, verdict);
            decidedAt.put(id, System.currentTimeMillis());
        }
        if (k != null && EXPIRED.equals(k.state) && "n".equals(verdict)) {
            // 忽略: refusing a lapsed request only clears it (ADR 0015, decision 2).
            nm(c).cancel(id, ID);
        } else if (k != null) {
            post(c, k, locked(c));
        }
        changed();
    }

    /**
     * One listener, the screen in front (home or detail); called on the main thread after any
     * change. Only one of them is resumed at a time.
     */
    static synchronized void listen(Runnable r) {
        listener = r;
    }

    static synchronized void unlisten(Runnable r) {
        if (listener == r) {
            listener = null;
        }
    }

    private static void changed() {
        Runnable r;
        synchronized (ApprovalNotifier.class) {
            r = listener;
        }
        if (r != null) {
            main.post(r);
        }
    }

    // ---------------------------------------------------------------- wording

    static String title(Card k) {
        String d = k.device.isEmpty() ? "设备" : k.device;
        switch (k.kind) {
            case PAIR:
                return "新控制端请求连接 " + d;
            case "exec":
                return d + " 请求执行命令";
            case "exec-elevated":
                return d + " 请求执行提权命令";
            case "read":
                return d + " 请求读取文件";
            case "write":
                return d + " 请求写入文件";
            case "logs":
                return d + " 请求读取日志";
            default:
                return d + " 请求审批";
        }
    }

    static String peerName(Card k) {
        return k.peer.isEmpty() ? "未命名控制端" : k.peer;
    }

    /** What the request is about in one line: the command, the path, or who is asking. */
    private static String subject(Card k) {
        if (k.pairing()) {
            return peerName(k);
        }
        if (!k.cmd.isEmpty()) {
            return k.cmd;
        }
        return k.path.isEmpty() ? "来自 " + peerName(k) : k.path;
    }

    /** The state line, or null for a pending card nobody has answered. */
    static String status(Card k) {
        if (DONE.equals(k.state)) {
            switch (k.result) {
                case "allowed":
                    if (k.pairing()) {
                        return "已信任";
                    }
                    return k.kind.startsWith("exec") ? "已允许，命令正在执行" : "已允许";
                case "denied":
                    return "已拒绝";
                case "granted":
                    return "已放行：30 分钟内同一条命令可执行一次";
                case "handled":
                    return "这条已在别处处理";
                default:
                    // gone: the wait ran out (or the device restarted) before the answer landed.
                    return "已过期";
            }
        }
        if (verdict(k.id) != null) {
            return "提交中…";
        }
        if (EXPIRED.equals(k.state)) {
            return k.pairing() ? "已过期，请让控制端重新连接" : "已过期，仍可批准：30 分钟内放行一次";
        }
        return null;
    }

    /** The allow and refuse labels, or null when there is nothing left to decide. */
    static String[] choices(Card k) {
        if (verdict(k.id) != null) {
            return null;
        }
        if (PENDING.equals(k.state)) {
            // Pairing trusts the key for good, so it is not "once"; the words are the portal's.
            return k.pairing() ? new String[] {"信任它", "拒绝"} : new String[] {"允许一次", "拒绝"};
        }
        if (EXPIRED.equals(k.state) && !k.pairing()) {
            return new String[] {"允许（30 分钟内一次）", "忽略"};
        }
        return null;
    }

    static String shortFingerprint(String fp) {
        return fp.length() <= 25 ? fp : fp.substring(0, 14) + "…" + fp.substring(fp.length() - 6);
    }

    // ---------------------------------------------------------------- notifications

    static void channel(Context c) {
        NotificationChannel ch = new NotificationChannel(
                CHANNEL, "待审批", NotificationManager.IMPORTANCE_HIGH);
        ch.setDescription("其他设备请求执行命令或连接时提醒你审批");
        ch.enableVibration(true);
        ch.setLockscreenVisibility(Notification.VISIBILITY_PRIVATE);
        nm(c).createNotificationChannel(ch);
    }

    /** Re-renders every approval notification for the new lock state, silently. */
    static void refresh(Context c, boolean locked) {
        for (StatusBarNotification s : nm(c).getActiveNotifications()) {
            if (s.getId() != ID || s.getTag() == null) {
                continue;
            }
            Card k = card(c, s.getTag());
            if (k != null && !TEST.equals(k.state)) {
                post(c, k, locked);
            }
        }
    }

    private static void post(Context c, Card k, boolean locked) {
        channel(c);
        boolean done = DONE.equals(k.state);
        Notification.Builder b = new Notification.Builder(c, CHANNEL)
                .setSmallIcon(R.drawable.ic_stat_agent)
                .setWhen(k.created)
                .setShowWhen(true)
                .setOnlyAlertOnce(true)
                .setVisibility(Notification.VISIBILITY_PRIVATE);
        if (TEST.equals(k.state)) {
            String title = "审批提醒测试";
            String text = "听到提示音、看到横幅了吗？点这里确认。没响的话，点进来按步骤打开。";
            Notification cover = new Notification.Builder(c, CHANNEL)
                    .setSmallIcon(R.drawable.ic_stat_agent)
                    .setContentTitle(title)
                    .setContentText(text)
                    .setStyle(new Notification.BigTextStyle().bigText(text))
                    .build();
            PendingIntent guide = PendingIntent.getActivity(c, ID,
                    new Intent(c, MainActivity.class).putExtra(MainActivity.EXTRA_PAGE, MainActivity.PAGE_ALERT_CHECK),
                    PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_UPDATE_CURRENT);
            b.setContentTitle(title)
                    .setContentText(text)
                    .setStyle(new Notification.BigTextStyle().bigText(text))
                    .setPublicVersion(cover)
                    .setContentIntent(guide)
                    .setAutoCancel(true);
            nm(c).notify(k.id, ID, b.build());
            return;
        }
        String line = done ? "1 个审批请求已处理" : "有 1 个待审批请求";
        Notification cover = new Notification.Builder(c, CHANNEL)
                .setSmallIcon(R.drawable.ic_stat_agent)
                .setWhen(k.created)
                .setShowWhen(true)
                .setContentTitle(line)
                .build();
        Uri card = Uri.fromParts(SCHEME, k.id, null);
        b.setPublicVersion(cover)
                .setContentIntent(PendingIntent.getActivity(c, 0,
                        new Intent(c, ApprovalActivity.class).setData(card),
                        PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_UPDATE_CURRENT));
        b.getExtras().putString(EXTRA_CARD, k.json);
        if (locked) {
            b.setContentTitle(line);
        } else {
            String status = status(k);
            StringBuilder body = new StringBuilder(subject(k));
            if (!k.pairing() && (!k.cmd.isEmpty() || !k.path.isEmpty())) {
                body.append("\n来自 ").append(peerName(k));
            }
            if (status != null) {
                body.append('\n').append(status);
            }
            b.setContentTitle(title(k))
                    .setContentText(status != null ? status : subject(k))
                    .setStyle(new Notification.BigTextStyle().bigText(body));
            String[] choices = choices(k);
            if (choices != null) {
                b.addAction(action(c, k.id, "y", choices[0]));
                b.addAction(action(c, k.id, "n", choices[1]));
            }
        }
        if (done) {
            // Every lock and unlock redraws the card; restarting the timer
            // each time would keep a final card up for as long as the phone
            // keeps being picked up.
            long left = DONE_LINGER_MS - (System.currentTimeMillis() - k.seen);
            if (left <= 0) {
                nm(c).cancel(k.id, ID);
                return;
            }
            b.setTimeoutAfter(left);
        }
        nm(c).notify(k.id, ID, b.build());
    }

    private static Notification.Action action(Context c, String id, String verdict, String label) {
        PendingIntent pi = PendingIntent.getBroadcast(c, 0,
                new Intent(c, ApprovalReceiver.class).setData(Uri.fromParts(SCHEME, id, verdict)),
                PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_UPDATE_CURRENT);
        Notification.Action.Builder a = new Notification.Action.Builder(null, label, pi);
        if (Build.VERSION.SDK_INT >= 31) {
            a.setAuthenticationRequired(true);
        }
        return a.build();
    }

    /** The detail screen's address for a card, as the notification and the home list open it. */
    static Uri uri(String id) {
        return Uri.fromParts(SCHEME, id, null);
    }

    /** The card id a notification action or the detail screen was opened for. */
    static String cardId(Uri u) {
        return u == null || !SCHEME.equals(u.getScheme()) ? "" : u.getSchemeSpecificPart();
    }

    static boolean locked(Context c) {
        return c.getSystemService(KeyguardManager.class).isKeyguardLocked()
                || !c.getSystemService(PowerManager.class).isInteractive();
    }

    private static StatusBarNotification active(Context c, String id) {
        for (StatusBarNotification s : nm(c).getActiveNotifications()) {
            if (s.getId() == ID && id.equals(s.getTag())) {
                return s;
            }
        }
        return null;
    }

    private static NotificationManager nm(Context c) {
        return c.getSystemService(NotificationManager.class);
    }

    private static <V> Map<String, V> recent() {
        return new LinkedHashMap<String, V>() {
            @Override
            protected boolean removeEldestEntry(Map.Entry<String, V> eldest) {
                return size() > KEEP;
            }
        };
    }
}
