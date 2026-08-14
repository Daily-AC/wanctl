package com.***REMOVED***.wanctl;

import android.content.Context;

import java.io.BufferedReader;
import java.io.File;
import java.io.IOException;
import java.io.InputStreamReader;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.concurrent.TimeUnit;

/**
 * Locates and invokes the wanctl binary that ships inside this APK.
 *
 * <p>The binary lives at {@code nativeLibraryDir/libwanctl.so}. That is the one
 * directory an ordinary Android app can both reach and execute from: the
 * package manager extracts {@code lib/<abi>/} out of the APK onto disk with the
 * {@code apk_data_file} SELinux label, which {@code untrusted_app} is permitted
 * to exec. Everything the app itself can write — {@code getFilesDir()},
 * {@code getCacheDir()}, {@code getCodeCacheDir()} — carries
 * {@code app_data_file} instead, and exec of those is denied outright. Copying
 * the binary somewhere "nicer" therefore breaks it; it runs from where the
 * installer put it or not at all.
 *
 * <p>None of the Termux-era argv workarounds apply here. Nothing preloads an
 * exec interceptor into this process, so {@code argv} arrives unmodified and
 * {@code os.Executable()} answers with the binary rather than the dynamic
 * linker.
 */
final class Wanctl {
    private Wanctl() {
    }

    static File binary(Context c) {
        return new File(c.getApplicationInfo().nativeLibraryDir, "libwanctl.so");
    }

    static File configDir(Context c) {
        return new File(c.getFilesDir(), "wanctl");
    }

    static File logFile(Context c) {
        return new File(c.getFilesDir(), "agent.log");
    }

    static File deviceStateFile(Context c) {
        return new File(new File(c.getFilesDir(), "state"), "device.json");
    }

    /**
     * Builds a command for the bundled binary with the environment wanctl needs.
     *
     * <p>{@code WANCTL_CONFIG_DIR} is set explicitly rather than left to
     * wanctl's own probing. Its Android fallback chain ends at
     * {@code /data/local/tmp/.wanctl}, which is shared with every other app and
     * with the adb shell user — correct for a binary someone pushed over adb,
     * wrong for an installed app that has a private directory of its own.
     *
     * <p>{@code WANCTL_ELEVATION} carries the app's elevation switch to the Go
     * side. It is passed as an environment variable rather than a flag because
     * the agent is a long-lived child: flipping the switch restarts the service
     * (see MainActivity), which is the point at which the new value takes
     * effect. Absent means off, so a build of the app that never learned about
     * this cannot accidentally enable it.
     */
    static ProcessBuilder command(Context c, String... args) {
        List<String> argv = new ArrayList<>();
        argv.add(binary(c).getAbsolutePath());
        for (String a : args) {
            argv.add(a);
        }
        ProcessBuilder pb = new ProcessBuilder(argv);
        Map<String, String> env = pb.environment();
        env.put("WANCTL_CONFIG_DIR", configDir(c).getAbsolutePath());
        env.put("WANCTL_DEVICE_STATE_FILE", deviceStateFile(c).getAbsolutePath());
        if (new Prefs(c).elevation()) {
            env.put("WANCTL_ELEVATION", "1");
        }
        env.put("HOME", c.getFilesDir().getAbsolutePath());
        env.put("TMPDIR", c.getCacheDir().getAbsolutePath());
        // wanctl resolves the session shell to /system/bin/sh on Android by
        // itself; PATH is set so anything the *session* runs behaves like a
        // normal adb shell rather than inheriting this app's empty environment.
        env.put("PATH", "/system/bin:/system/xbin:/vendor/bin");
        return pb;
    }

    static final class Result {
        final int code;
        final String out;
        final String err;

        Result(int code, String out, String err) {
            this.code = code;
            this.out = out;
            this.err = err;
        }

        boolean ok() {
            return code == 0;
        }

        /** The most informative message available, for showing a human. */
        String message() {
            String e = err.trim();
            if (!e.isEmpty()) {
                return e;
            }
            String o = out.trim();
            return o.isEmpty() ? ("exit " + code) : o;
        }
    }

    /** Runs a short-lived wanctl subcommand to completion. Never call on the main thread. */
    static Result run(Context c, int timeoutSeconds, String... args) {
        Process p = null;
        try {
            p = command(c, args).start();
            p.getOutputStream().close();
            final Process proc = p;
            final StringBuilder errBuf = new StringBuilder();
            Thread errPump = new Thread(() -> drain(proc.getErrorStream(), errBuf));
            errPump.setDaemon(true);
            errPump.start();

            StringBuilder outBuf = new StringBuilder();
            drain(p.getInputStream(), outBuf);

            if (!p.waitFor(timeoutSeconds, TimeUnit.SECONDS)) {
                p.destroyForcibly();
                return new Result(-1, outBuf.toString(), "timed out after " + timeoutSeconds + "s");
            }
            errPump.join(1000);
            return new Result(p.exitValue(), outBuf.toString(), errBuf.toString());
        } catch (IOException e) {
            return new Result(-1, "", describe(e));
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            return new Result(-1, "", "interrupted");
        } finally {
            if (p != null) {
                p.destroy();
            }
        }
    }

    /**
     * Turns an exec failure into something that names the actual cause. A bare
     * {@code IOException} here reads "error=13, Permission denied", which is the
     * exact symptom of the binary having been copied out of nativeLibraryDir —
     * worth saying out loud rather than making the next person rediscover it.
     */
    private static String describe(IOException e) {
        String m = String.valueOf(e.getMessage());
        if (m.contains("Permission denied")) {
            return m + "\n(binary must run from nativeLibraryDir — Android forbids exec of app_data_file)";
        }
        return m;
    }

    private static void drain(java.io.InputStream in, StringBuilder sink) {
        try (BufferedReader r = new BufferedReader(new InputStreamReader(in, StandardCharsets.UTF_8))) {
            String line;
            while ((line = r.readLine()) != null) {
                synchronized (sink) {
                    sink.append(line).append('\n');
                }
            }
        } catch (IOException ignored) {
            // Process died; whatever was read already is what we report.
        }
    }
}
