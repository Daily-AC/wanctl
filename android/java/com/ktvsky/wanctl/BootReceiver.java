package com.***REMOVED***.wanctl;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;

/**
 * Brings the agent back after a reboot — the one thing the Termux route never
 * solved. It also handles MY_PACKAGE_REPLACED so an in-app update does not
 * silently leave the device offline until someone opens the app.
 *
 * <p>Two conditions gate it, and both are the user's: they must have enabled the
 * agent, and they must not have turned autostart off. Neither is inferred.
 */
public final class BootReceiver extends BroadcastReceiver {
    @Override
    public void onReceive(Context context, Intent intent) {
        String action = intent == null ? "" : String.valueOf(intent.getAction());
        if (!Intent.ACTION_BOOT_COMPLETED.equals(action)
                && !Intent.ACTION_MY_PACKAGE_REPLACED.equals(action)) {
            return;
        }
        Prefs prefs = new Prefs(context);
        if (!prefs.enabled()) {
            KeeperJob.cancel(context);
            return;
        }
        if (Intent.ACTION_BOOT_COMPLETED.equals(action) && !prefs.bootStart()) {
            return;
        }
        // Rescheduled here too: a persisted job should survive a reboot on its
        // own, but this receiver is the one place that runs after a reboot
        // regardless, and a duplicate schedule() is a no-op.
        KeeperJob.schedule(context);
        AgentService.start(context);
    }
}
