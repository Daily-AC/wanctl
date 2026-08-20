package dev.wanctl.agent;

import android.app.Activity;
import android.app.AlertDialog;
import android.app.PendingIntent;
import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.content.IntentFilter;
import android.content.pm.PackageInstaller;
import android.net.Uri;
import android.os.Build;
import android.os.Handler;
import android.os.Looper;
import android.provider.Settings;
import android.widget.Toast;

import java.io.File;
import java.io.FileInputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

/**
 * In-app update.
 *
 * <p>An APK cannot update itself the way `wanctl update` updates a binary: the
 * only directory this app may execute from is the one the package manager owns,
 * and it is read-only. So the unit of update is the APK, and the installer is
 * the system's.
 *
 * <p>The download and its signature check are done by the wanctl binary, not
 * here. It already fetches the release manifest, verifies the Ed25519 signature
 * against the public key compiled into it, and checks the artifact's SHA-256 —
 * reimplementing any of that in Java would add a second, weaker path to the
 * same trust decision. Java's job is to hand the verified file to
 * PackageInstaller. The APK's own signing key is then checked by Android on top
 * of that: an APK signed by anything other than the installed app's key is
 * refused, whatever the release manifest says.
 */
final class Installer {
    private static final String ACTION_STATUS = "dev.wanctl.agent.INSTALL_STATUS";

    private final Activity activity;
    private final ExecutorService io = Executors.newSingleThreadExecutor();
    private final Handler main = new Handler(Looper.getMainLooper());
    private BroadcastReceiver receiver;

    Installer(Activity activity) {
        this.activity = activity;
    }

    void close() {
        if (receiver != null) {
            try {
                activity.unregisterReceiver(receiver);
            } catch (IllegalArgumentException ignored) {
                // Never registered; nothing to undo.
            }
            receiver = null;
        }
        io.shutdownNow();
    }

    void checkAndInstall() {
        Toast.makeText(activity, R.string.update_checking, Toast.LENGTH_SHORT).show();
        File dir = new File(activity.getCacheDir(), "update");
        //noinspection ResultOfMethodCallIgnored
        dir.mkdirs();
        clear(dir);
        io.execute(() -> {
            // Generous: the relay verifies the artifact's hash before serving it,
            // and a phone on mobile data is not fast.
            Wanctl.Result r = Wanctl.run(activity, 300, "update", "--fetch-apk", dir.getAbsolutePath());
            main.post(() -> {
                if (!r.ok()) {
                    fail(activity.getString(R.string.update_failed), r.message());
                    return;
                }
                String path = r.out.trim();
                if (path.isEmpty()) {
                    Toast.makeText(activity, R.string.update_latest, Toast.LENGTH_LONG).show();
                    return;
                }
                install(new File(path));
            });
        });
    }

    private void install(File apk) {
        if (!activity.getPackageManager().canRequestPackageInstalls()) {
            new AlertDialog.Builder(activity)
                    .setTitle(R.string.btn_update)
                    .setMessage("系统需要先允许 wanctl 安装应用，去设置里打开「安装未知应用」后再点一次检查更新。")
                    .setNegativeButton(R.string.cancel, null)
                    .setPositiveButton(android.R.string.ok, (d, w) -> activity.startActivity(
                            new Intent(Settings.ACTION_MANAGE_UNKNOWN_APP_SOURCES,
                                    Uri.parse("package:" + activity.getPackageName()))))
                    .show();
            return;
        }
        registerReceiver();
        io.execute(() -> {
            PackageInstaller pi = activity.getPackageManager().getPackageInstaller();
            try {
                PackageInstaller.SessionParams params = new PackageInstaller.SessionParams(
                        PackageInstaller.SessionParams.MODE_FULL_INSTALL);
                params.setAppPackageName(activity.getPackageName());
                int sessionId = pi.createSession(params);
                try (PackageInstaller.Session session = pi.openSession(sessionId)) {
                    try (InputStream in = new FileInputStream(apk);
                         OutputStream out = session.openWrite("wanctl", 0, apk.length())) {
                        byte[] buf = new byte[64 * 1024];
                        int n;
                        while ((n = in.read(buf)) > 0) {
                            out.write(buf, 0, n);
                        }
                        session.fsync(out);
                    }
                    int flags = Build.VERSION.SDK_INT >= 31
                            ? PendingIntent.FLAG_MUTABLE : 0;
                    PendingIntent sender = PendingIntent.getBroadcast(activity, sessionId,
                            new Intent(ACTION_STATUS).setPackage(activity.getPackageName()), flags);
                    session.commit(sender.getIntentSender());
                }
            } catch (IOException e) {
                main.post(() -> fail(activity.getString(R.string.update_failed), String.valueOf(e.getMessage())));
            }
        });
    }

    private void registerReceiver() {
        if (receiver != null) {
            return;
        }
        receiver = new BroadcastReceiver() {
            @Override
            public void onReceive(Context context, Intent intent) {
                int st = intent.getIntExtra(PackageInstaller.EXTRA_STATUS,
                        PackageInstaller.STATUS_FAILURE);
                if (st == PackageInstaller.STATUS_PENDING_USER_ACTION) {
                    // The system's own confirm-install screen. Nothing installs
                    // without the user seeing this.
                    Intent confirm = Build.VERSION.SDK_INT >= 33
                            ? intent.getParcelableExtra(Intent.EXTRA_INTENT, Intent.class)
                            : intent.getParcelableExtra(Intent.EXTRA_INTENT);
                    if (confirm != null) {
                        confirm.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
                        activity.startActivity(confirm);
                    }
                    return;
                }
                if (st == PackageInstaller.STATUS_SUCCESS) {
                    // The process is about to be replaced; BootReceiver's
                    // MY_PACKAGE_REPLACED branch brings the agent back.
                    Toast.makeText(context, "✓ 已更新", Toast.LENGTH_LONG).show();
                    return;
                }
                String msg = intent.getStringExtra(PackageInstaller.EXTRA_STATUS_MESSAGE);
                fail(activity.getString(R.string.update_failed), msg == null ? ("status " + st) : msg);
            }
        };
        IntentFilter filter = new IntentFilter(ACTION_STATUS);
        if (Build.VERSION.SDK_INT >= 33) {
            activity.registerReceiver(receiver, filter, Context.RECEIVER_NOT_EXPORTED);
        } else {
            activity.registerReceiver(receiver, filter);
        }
    }

    private void fail(String title, String detail) {
        new AlertDialog.Builder(activity)
                .setTitle(title)
                .setMessage(detail)
                .setPositiveButton(android.R.string.ok, null)
                .show();
    }

    private static void clear(File dir) {
        File[] fs = dir.listFiles();
        if (fs == null) {
            return;
        }
        for (File f : fs) {
            //noinspection ResultOfMethodCallIgnored
            f.delete();
        }
    }
}
