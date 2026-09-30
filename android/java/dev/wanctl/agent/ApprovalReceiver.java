package dev.wanctl.agent;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;

/**
 * The 「允许」 and 「拒绝」 buttons on an approval notification. Not exported: only this app's own
 * PendingIntents reach it, and on API 31+ the system asks for the unlock before it fires.
 */
public final class ApprovalReceiver extends BroadcastReceiver {
    @Override
    public void onReceive(Context context, Intent intent) {
        AgentService.decide(context, ApprovalNotifier.cardId(intent.getData()),
                intent.getData() == null ? "" : intent.getData().getFragment());
    }
}
