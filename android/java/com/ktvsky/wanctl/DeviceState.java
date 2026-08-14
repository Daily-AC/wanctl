package com.***REMOVED***.wanctl;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.content.IntentFilter;
import android.os.BatteryManager;
import android.os.Build;
import android.util.Log;

import org.json.JSONException;
import org.json.JSONObject;

import java.io.File;
import java.io.FileOutputStream;
import java.io.IOException;
import java.io.OutputStreamWriter;
import java.io.Writer;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.StandardCopyOption;
import java.time.Instant;
import java.util.Iterator;

/** Collects Android framework state for the non-JVM wanctl child process. */
final class DeviceState {
    private static final String TAG = "wanctl";

    private final Context context;
    private boolean registered;

    /**
     * The state file is one JSON document with two independent writers, so each
     * source keeps its last value here and the whole document is rewritten
     * whenever either changes. Writing only the part that changed would drop
     * the other one — they are minutes apart and neither can reconstruct it.
     */
    private JSONObject battery;
    private JSONObject adb;

    private AdbPortWatcher adbPorts;

    private final BroadcastReceiver batteryReceiver = new BroadcastReceiver() {
        @Override
        public void onReceive(Context context, Intent intent) {
            writeBattery(intent);
        }
    };

    DeviceState(Context context) {
        this.context = context;
    }

    /** Registers for changes and writes the current sticky state before returning. */
    void start() {
        refreshAdbPortWatch();
        if (registered) {
            return;
        }
        IntentFilter filter = new IntentFilter(Intent.ACTION_BATTERY_CHANGED);
        Intent current;
        if (Build.VERSION.SDK_INT >= 33) {
            current = context.registerReceiver(
                    batteryReceiver, filter, Context.RECEIVER_EXPORTED);
        } else {
            current = context.registerReceiver(batteryReceiver, filter);
        }
        registered = true;
        if (current != null) {
            writeBattery(current);
        }
    }

    void stop() {
        if (adbPorts != null) {
            adbPorts.stop();
            adbPorts = null;
        }
        if (!registered) {
            return;
        }
        context.unregisterReceiver(batteryReceiver);
        registered = false;
    }

    /**
     * Starts or stops the wireless-debugging port watch to match the app's
     * 提权通道 switch. Called on every service start, because flipping that
     * switch restarts the agent child rather than the service — see
     * MainActivity — so onCreate() alone would miss the change.
     *
     * <p>Nothing is watched while the switch is off. Discovery is cheap but it
     * is not free, and a device whose owner has not turned elevation on should
     * not be listening for the port that would enable it.
     */
    void refreshAdbPortWatch() {
        boolean want = new Prefs(context).elevation();
        if (want && adbPorts == null) {
            adbPorts = new AdbPortWatcher(context, this::writeADBPort);
            adbPorts.start();
        } else if (!want && adbPorts != null) {
            adbPorts.stop();
            adbPorts = null;
            writeADBPort(0);
        }
    }

    /** Records the port mDNS found, or clears it when the service goes away. */
    private void writeADBPort(int port) {
        try {
            JSONObject next = null;
            if (port > 0) {
                next = new JSONObject();
                next.put("port", port);
                next.put("updated_at", Instant.now().toString());
            }
            synchronized (this) {
                adb = next;
            }
            publish();
        } catch (JSONException | IOException e) {
            Log.w(TAG, "could not persist the adb port", e);
        }
    }

    private void writeBattery(Intent intent) {
        int rawLevel = intent.getIntExtra(BatteryManager.EXTRA_LEVEL, -1);
        int scale = intent.getIntExtra(BatteryManager.EXTRA_SCALE, -1);
        if (rawLevel < 0 || scale <= 0) {
            Log.w(TAG, "battery broadcast did not contain a valid level");
            return;
        }

        try {
            JSONObject battery = new JSONObject();
            battery.put("level", Math.round(rawLevel * 100.0f / scale));
            battery.put("status", statusName(
                    intent.getIntExtra(BatteryManager.EXTRA_STATUS, BatteryManager.BATTERY_STATUS_UNKNOWN)));
            battery.put("plugged", pluggedName(
                    intent.getIntExtra(BatteryManager.EXTRA_PLUGGED, 0)));
            battery.put("temperature_c",
                    intent.getIntExtra(BatteryManager.EXTRA_TEMPERATURE, 0) / 10.0);
            battery.put("health", healthName(
                    intent.getIntExtra(BatteryManager.EXTRA_HEALTH, BatteryManager.BATTERY_HEALTH_UNKNOWN)));
            battery.put("updated_at", Instant.now().toString());
            synchronized (this) {
                this.battery = battery;
            }
            publish();
        } catch (JSONException | IOException e) {
            Log.w(TAG, "could not persist device state", e);
        }
    }

    /**
     * Writes both sources as one document: the battery fields at the top level,
     * where v0.1.12's reader already looks for them, and the adb port beside
     * them under its own key. Each reader ignores what it does not know, in
     * both directions, so neither has to learn about the other.
     */
    private void publish() throws JSONException, IOException {
        JSONObject doc = new JSONObject();
        JSONObject battery;
        JSONObject adb;
        synchronized (this) {
            battery = this.battery;
            adb = this.adb;
        }
        if (battery != null) {
            for (Iterator<String> keys = battery.keys(); keys.hasNext(); ) {
                String key = keys.next();
                doc.put(key, battery.get(key));
            }
        }
        if (adb != null) {
            doc.put("adb", adb);
        }
        atomicWrite(Wanctl.deviceStateFile(context), doc.toString() + "\n");
    }

    private static void atomicWrite(File target, String value) throws IOException {
        File dir = target.getParentFile();
        if (!dir.isDirectory() && !dir.mkdirs()) {
            throw new IOException("could not create state directory: " + dir);
        }
        File temp = new File(dir, target.getName() + ".tmp");
        try (FileOutputStream out = new FileOutputStream(temp);
             Writer writer = new OutputStreamWriter(out, StandardCharsets.UTF_8)) {
            writer.write(value);
            writer.flush();
            out.getFD().sync();
        }
        try {
            Files.move(temp.toPath(), target.toPath(),
                    StandardCopyOption.ATOMIC_MOVE, StandardCopyOption.REPLACE_EXISTING);
        } catch (IOException e) {
            //noinspection ResultOfMethodCallIgnored
            temp.delete();
            throw e;
        }
    }

    private static String statusName(int status) {
        switch (status) {
            case BatteryManager.BATTERY_STATUS_CHARGING:
                return "charging";
            case BatteryManager.BATTERY_STATUS_DISCHARGING:
                return "discharging";
            case BatteryManager.BATTERY_STATUS_FULL:
                return "full";
            case BatteryManager.BATTERY_STATUS_NOT_CHARGING:
                return "not charging";
            default:
                return "unknown";
        }
    }

    private static String pluggedName(int plugged) {
        if ((plugged & BatteryManager.BATTERY_PLUGGED_AC) != 0) {
            return "ac";
        }
        if ((plugged & BatteryManager.BATTERY_PLUGGED_USB) != 0) {
            return "usb";
        }
        if ((plugged & BatteryManager.BATTERY_PLUGGED_WIRELESS) != 0) {
            return "wireless";
        }
        return plugged == 0 ? "none" : "unknown";
    }

    private static String healthName(int health) {
        switch (health) {
            case BatteryManager.BATTERY_HEALTH_GOOD:
                return "good";
            case BatteryManager.BATTERY_HEALTH_OVERHEAT:
                return "overheat";
            case BatteryManager.BATTERY_HEALTH_DEAD:
                return "dead";
            case BatteryManager.BATTERY_HEALTH_OVER_VOLTAGE:
                return "over voltage";
            case BatteryManager.BATTERY_HEALTH_UNSPECIFIED_FAILURE:
                return "unspecified failure";
            case BatteryManager.BATTERY_HEALTH_COLD:
                return "cold";
            default:
                return "unknown";
        }
    }
}
