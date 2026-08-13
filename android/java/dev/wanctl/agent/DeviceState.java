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

/** Collects Android framework state for the non-JVM wanctl child process. */
final class DeviceState {
    private static final String TAG = "wanctl";

    private final Context context;
    private boolean registered;

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
        if (!registered) {
            return;
        }
        context.unregisterReceiver(batteryReceiver);
        registered = false;
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
            atomicWrite(Wanctl.deviceStateFile(context), battery.toString() + "\n");
        } catch (JSONException | IOException e) {
            Log.w(TAG, "could not persist device state", e);
        }
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
