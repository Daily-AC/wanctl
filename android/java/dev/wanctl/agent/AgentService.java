package com.***REMOVED***.wanctl;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Context;
import android.content.Intent;
import android.content.pm.ServiceInfo;
import android.os.Build;
import android.os.IBinder;
import android.os.PowerManager;
import android.util.Log;

import java.io.BufferedReader;
import java.io.File;
import java.io.FileWriter;
import java.io.IOException;
import java.io.InputStreamReader;
import java.io.PrintWriter;
import java.nio.charset.StandardCharsets;
import java.text.SimpleDateFormat;
import java.util.ArrayList;
import java.util.Date;
import java.util.List;
import java.util.Locale;

/**
 * Runs and supervises the wanctl agent as a child process.
 *
 * <p>This is what the Termux route never had: Android gives an unprivileged
 * process no service manager to install itself into, so `wanctl service install`
 * refuses on Android and `wanctl start` produces a detached process that dies at
 * the next reboot. A foreground service is the platform's own answer — it
 * survives the activity going away, it is visible to the user in the shade
 * (which is the deal: the OS keeps it alive, the user always knows), and paired
 * with BOOT_COMPLETED it comes back by itself.
 */
public final class AgentService extends Service {
    private static final String TAG = "wanctl";
    private static final String CHANNEL = "agent";
    private static final int NOTIFICATION_ID = 1;
    private static final long MAX_LOG_BYTES = 512 * 1024;

    /** Restarting faster than this after a clean start means something is wrong, not flapping. */
    private static final long STABLE_RUN_MS = 60_000;
    private static final long BACKOFF_MIN_MS = 2_000;
    private static final long BACKOFF_MAX_MS = 60_000;

    public static final String ACTION_STOP = "com.***REMOVED***.wanctl.STOP";
    public static final String ACTION_RESTART = "com.***REMOVED***.wanctl.RESTART";

    private Thread supervisor;
    private volatile Process child;
    private volatile boolean stopping;
    private volatile boolean restarting;
    private PowerManager.WakeLock wakeLock;

    /**
     * Whether this service is alive, for the reconcilers to check. A static is
     * honest here rather than sloppy: it lives in the same process as the
     * service, so a process death resets it to false — which is exactly the
     * answer the reconciler needs in that case.
     */
    private static volatile boolean running;

    static boolean isRunning() {
        return running;
    }

    static void start(Context c) {
        Intent i = new Intent(c, AgentService.class);
        c.startForegroundService(i);
    }

    static void stop(Context c) {
        c.stopService(new Intent(c, AgentService.class));
    }

    /**
     * Applies changed settings without a stop/start pair.
     *
     * <p>stopService() is asynchronous, so stopping and immediately starting
     * races onDestroy() against the next onStartCommand(): the supervisor
     * thread may still be alive when the new command arrives, which the
     * `supervisor == null` guard reads as "already running" and the new
     * settings are never picked up. Killing the child instead is synchronous
     * from the caller's point of view, and runOnce() re-reads Prefs on every
     * iteration, so the respawn carries the new flags.
     */
    static void restart(Context c) {
        c.startForegroundService(new Intent(c, AgentService.class).setAction(ACTION_RESTART));
    }

    @Override
    public void onCreate() {
        super.onCreate();
        running = true;
        NotificationManager nm = getSystemService(NotificationManager.class);
        NotificationChannel ch = new NotificationChannel(
                CHANNEL, getString(R.string.channel_name), NotificationManager.IMPORTANCE_LOW);
        ch.setDescription(getString(R.string.channel_desc));
        ch.setShowBadge(false);
        nm.createNotificationChannel(ch);
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        String action = intent == null ? null : intent.getAction();
        if (ACTION_STOP.equals(action)) {
            new Prefs(this).setEnabled(false);
            stopSelf();
            return START_NOT_STICKY;
        }
        // Always first: the system kills a foreground service that has not
        // called this within a few seconds of being started, whatever else it
        // was asked to do.
        startForegroundCompat(notification(getString(R.string.state_retrying), ""));
        if (ACTION_RESTART.equals(action) && supervisor != null) {
            restarting = true;
            Process p = child;
            if (p != null) {
                p.destroy();
            }
            supervisor.interrupt();
            return START_STICKY;
        }
        if (supervisor == null) {
            stopping = false;
            acquireWakeLock();
            supervisor = new Thread(this::supervise, "wanctl-supervisor");
            supervisor.start();
        }
        // START_STICKY so a low-memory kill is followed by a restart; the agent
        // being reachable is the entire product.
        return START_STICKY;
    }

    @Override
    public void onDestroy() {
        running = false;
        stopping = true;
        Process p = child;
        if (p != null) {
            p.destroy(); // SIGTERM: the agent deregisters from the relay on the way out
        }
        Thread t = supervisor;
        if (t != null) {
            t.interrupt();
        }
        supervisor = null;
        releaseWakeLock();
        AgentState.get().setPhase(AgentState.Phase.STOPPED, "");
        super.onDestroy();
    }

    @Override
    public IBinder onBind(Intent intent) {
        return null;
    }

    // ---------------------------------------------------------------- supervision

    private void supervise() {
        AgentState state = AgentState.get();
        long backoff = BACKOFF_MIN_MS;
        while (!stopping) {
            long startedAt = System.currentTimeMillis();
            state.setPhase(AgentState.Phase.STARTING, "");
            updateNotification(getString(R.string.state_retrying), "");
            int code;
            String fatal = null;
            try {
                code = runOnce();
            } catch (IOException e) {
                // Stopping or restarting tears the pipe out from under the
                // reader, so this is the normal exit from both of those as well
                // as a real failure. Reporting "无法启动 agent" for either is a
                // lie in the log the user reads when something actually breaks.
                if (!stopping && !restarting) {
                    append("! 无法启动 agent: " + e.getMessage());
                    fatal = e.getMessage();
                }
                code = -1;
            } catch (InterruptedException e) {
                Thread.interrupted(); // clear the flag; a restart interrupt is not a stop
                if (stopping) {
                    break;
                }
                code = -1;
            }
            if (stopping) {
                break;
            }
            if (restarting) {
                // A settings change, not a failure: respawn at once and with a
                // clean backoff rather than making the user wait out a timer
                // that a crash loop earned.
                restarting = false;
                backoff = BACKOFF_MIN_MS;
                append("· 按新设置重启 agent");
                continue;
            }
            if (fatalReason != null) {
                fatal = fatalReason;
            }
            if (fatal != null) {
                // A rejected token is not a transient failure and retrying it
                // just burns battery while hiding the real problem from the
                // person who could fix it in ten seconds.
                append("✗ 已停止重试: " + fatal);
                state.setPhase(AgentState.Phase.ERROR, fatal);
                updateNotification(getString(R.string.state_stopped), fatal);
                return;
            }
            if (System.currentTimeMillis() - startedAt > STABLE_RUN_MS) {
                backoff = BACKOFF_MIN_MS;
            }
            append("· agent 退出 (code " + code + ")，" + (backoff / 1000) + "s 后重启");
            state.setPhase(AgentState.Phase.RETRYING, (backoff / 1000) + "s 后重试");
            updateNotification(getString(R.string.state_retrying), (backoff / 1000) + "s 后重试");
            try {
                Thread.sleep(backoff);
            } catch (InterruptedException e) {
                Thread.interrupted();
                if (stopping) {
                    break;
                }
                // Interrupted mid-backoff by a restart request: fall through
                // and respawn now.
            }
            backoff = Math.min(backoff * 2, BACKOFF_MAX_MS);
        }
        AgentState.get().setPhase(AgentState.Phase.STOPPED, "");
    }

    private volatile String fatalReason;

    private int runOnce() throws IOException, InterruptedException {
        fatalReason = null;
        Prefs prefs = new Prefs(this);
        List<String> args = new ArrayList<>();
        args.add("agent");
        String name = prefs.deviceName();
        if (!name.isEmpty()) {
            args.add("--name");
            args.add(name);
        }
        if (prefs.autoTrust()) {
            args.add("--yes");
        }
        // Passed explicitly in both directions: an empty --mode means "keep
        // whatever was persisted", so leaving it out after the user turns
        // bypass back off would silently keep the device wide open.
        args.add("--mode");
        args.add(prefs.bypass() ? "bypass" : "normal");
        ProcessBuilder pb = Wanctl.command(this, args.toArray(new String[0]));
        pb.redirectErrorStream(true);
        // Without this the agent inherits the service's working directory, "/",
        // which is read-only — so a relative path in an exec session or a
        // `wanctl push` with a bare filename fails for a reason nobody would
        // guess. The app's own files directory is the one place it can write.
        pb.directory(Wanctl.configDir(this).getParentFile());
        append("$ wanctl " + String.join(" ", args));
        Process p = pb.start();
        child = p;
        p.getOutputStream().close();
        try (BufferedReader r = new BufferedReader(
                new InputStreamReader(p.getInputStream(), StandardCharsets.UTF_8))) {
            String line;
            while ((line = r.readLine()) != null) {
                consume(line);
            }
        }
        int code = p.waitFor();
        child = null;
        return code;
    }

    /**
     * Reads meaning out of the agent's own output. See AgentState's note on
     * report vs measurement.
     *
     * <p>The three fatal markers below are a text coupling to the Go side, which
     * is worth naming: they are matched on substrings of messages wanctl prints,
     * and nothing in Java can stop those messages being reworded. What keeps it
     * honest is a test on the other side of the fence — TestAgentErrorsTheAppKeysOn
     * in the wanctl package asserts these substrings still appear — so a rewrite
     * fails CI rather than quietly turning a fatal error back into an infinite
     * retry loop.
     */
    private void consume(String line) {
        append(line);
        String t = line.trim();
        if (t.contains("online via ")) {
            String relay = t.substring(t.indexOf("online via ") + "online via ".length()).trim();
            AgentState.get().setOnline(relay, null);
            updateNotification(getString(R.string.state_running), relay);
        } else if (t.startsWith("fingerprint:")) {
            AgentState.get().setFingerprint(t.substring("fingerprint:".length()).trim());
        } else if (t.contains("--token")) {
            // No credential at all. Retrying cannot produce one, and a service
            // that respawns every two seconds forever is a battery drain that
            // hides a problem the user could fix in ten seconds.
            fatalReason = getString(R.string.err_not_logged_in);
        } else if (t.contains("rejected token")) {
            fatalReason = getString(R.string.err_token_rejected);
        } else if (t.contains("registered this device name")) {
            fatalReason = getString(R.string.err_name_taken);
        }
    }

    private void append(String line) {
        String stamped = new SimpleDateFormat("MM-dd HH:mm:ss", Locale.US).format(new Date()) + "  " + line;
        AgentState.get().append(stamped);
        Log.i(TAG, line);
        persist(stamped);
    }

    /**
     * Keeps a copy on disk, because the interesting log lines are the ones from
     * before the user opened the app — a night of reconnects, or the reason the
     * agent stopped while nobody was looking.
     */
    private void persist(String line) {
        File f = Wanctl.logFile(this);
        try {
            if (f.length() > MAX_LOG_BYTES) {
                File old = new File(f.getParentFile(), f.getName() + ".1");
                //noinspection ResultOfMethodCallIgnored
                old.delete();
                //noinspection ResultOfMethodCallIgnored
                f.renameTo(old);
            }
            try (PrintWriter w = new PrintWriter(new FileWriter(f, true))) {
                w.println(line);
            }
        } catch (IOException ignored) {
            // Logging must never be the thing that takes the agent down.
        }
    }

    // ---------------------------------------------------------------- platform glue

    private void startForegroundCompat(Notification n) {
        if (Build.VERSION.SDK_INT >= 34) {
            startForeground(NOTIFICATION_ID, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE);
        } else {
            startForeground(NOTIFICATION_ID, n);
        }
    }

    private void updateNotification(String title, String text) {
        NotificationManager nm = getSystemService(NotificationManager.class);
        nm.notify(NOTIFICATION_ID, notification(title, text));
    }

    private Notification notification(String title, String text) {
        PendingIntent open = PendingIntent.getActivity(this, 0,
                new Intent(this, MainActivity.class),
                PendingIntent.FLAG_IMMUTABLE);
        PendingIntent stop = PendingIntent.getService(this, 1,
                new Intent(this, AgentService.class).setAction(ACTION_STOP),
                PendingIntent.FLAG_IMMUTABLE);
        return new Notification.Builder(this, CHANNEL)
                .setSmallIcon(R.drawable.ic_stat_agent)
                .setContentTitle(title)
                .setContentText(text)
                .setContentIntent(open)
                .setOngoing(true)
                .setShowWhen(false)
                .addAction(new Notification.Action.Builder(null, "停止", stop).build())
                .build();
    }

    /**
     * A partial wake lock is what `termux-wake-lock` did for the Termux route.
     * A foreground service keeps the process from being killed; it does not keep
     * the CPU from suspending, and a suspended CPU is an agent that answers
     * nothing until someone touches the screen.
     */
    private void acquireWakeLock() {
        PowerManager pm = getSystemService(PowerManager.class);
        wakeLock = pm.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "wanctl:agent");
        wakeLock.setReferenceCounted(false);
        wakeLock.acquire();
    }

    private void releaseWakeLock() {
        if (wakeLock != null && wakeLock.isHeld()) {
            wakeLock.release();
        }
        wakeLock = null;
    }
}
