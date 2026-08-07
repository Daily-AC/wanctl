package com.***REMOVED***.wanctl;

import android.app.job.JobInfo;
import android.app.job.JobParameters;
import android.app.job.JobScheduler;
import android.app.job.JobService;
import android.content.ComponentName;
import android.content.Context;
import android.util.Log;

/**
 * A periodic reconciler: if the user wants the agent running and it is not, start it.
 *
 * <p>BOOT_COMPLETED is not reliable enough on its own. Measured on a vivo
 * PA2353 (Android 13) on 2026-08-07, across two reboots of the same build with
 * identical settings: the second delivered the broadcast and the agent came
 * back in 1 second; the first was dropped outright —
 *
 * <pre>am_broadcast_discard_app: [0,…,BOOT_COMPLETED,187,ResolveInfo{com.***REMOVED***.wanctl/.BootReceiver}]</pre>
 *
 * — while the same broadcast reached other apps in the same second. The system
 * discards a receiver whose process it could not start, and a just-booted
 * tablet was running a load average of 30. A device that silently fails to come
 * back after a reboot is the exact failure the Termux route had; leaving it on
 * one best-effort broadcast would be reproducing that with extra steps.
 *
 * <p>So the broadcast is the fast path and this is the floor: a persisted
 * periodic job survives reboots on its own, and 15 minutes is the shortest
 * period JobScheduler will accept. Worst case the agent is late, not absent.
 *
 * <p>Starting a foreground service from here is allowed for the same reason it
 * is allowed from BOOT_COMPLETED: the battery-optimization exemption. The
 * framework says so in as many words — the successful start logs as
 * {@code SYSTEM_ALLOW_LISTED}. Without that exemption this job will be
 * throttled and its foreground-service start refused, which is why the app
 * pushes the user towards granting it.
 */
public final class KeeperJob extends JobService {
    private static final int JOB_ID = 0x77616E; // "wan"
    private static final long PERIOD_MS = 15 * 60 * 1000L;

    static void schedule(Context c) {
        JobScheduler js = c.getSystemService(JobScheduler.class);
        if (js == null) {
            return;
        }
        JobInfo job = new JobInfo.Builder(JOB_ID, new ComponentName(c, KeeperJob.class))
                .setPeriodic(PERIOD_MS)
                .setPersisted(true)
                .setRequiredNetworkType(JobInfo.NETWORK_TYPE_ANY)
                .build();
        // Rescheduling an identical periodic job resets its period, so this is
        // only called on app start and on boot, not on every settings change.
        js.schedule(job);
    }

    static void cancel(Context c) {
        JobScheduler js = c.getSystemService(JobScheduler.class);
        if (js != null) {
            js.cancel(JOB_ID);
        }
    }

    @Override
    public boolean onStartJob(JobParameters params) {
        if (new Prefs(this).enabled() && !AgentService.isRunning()) {
            Log.i("wanctl", "keeper: agent should be running but is not; starting it");
            AgentService.start(this);
        }
        return false; // all done, synchronously
    }

    @Override
    public boolean onStopJob(JobParameters params) {
        return false; // nothing in flight to reschedule
    }
}
