package dev.wanctl.agent;

import android.content.Context;
import android.net.nsd.NsdManager;
import android.net.nsd.NsdServiceInfo;
import android.net.wifi.WifiManager;
import android.os.Handler;
import android.os.Looper;
import android.util.Log;

import java.net.InetAddress;
import java.net.NetworkInterface;
import java.net.SocketException;
import java.util.ArrayDeque;
import java.util.Deque;
import java.util.Enumeration;

/**
 * Finds the port this device's own adbd is listening on for wireless debugging.
 *
 * <p>Android picks a fresh port every time wireless debugging is enabled and
 * publishes it only over mDNS — there is no system property to read, and the
 * adb elevation channel cannot connect without it. (Measured on a PGBM10 on
 * 2026-08-14: the port moved from 37819 to 41031 across one re-pair, and
 * {@code getprop | grep -i adb} showed nothing carrying it.)
 *
 * <p>This lives in Java rather than in the Go child because mDNS on Android is
 * {@link NsdManager}, a framework service, and because the multicast hardware
 * filter is controlled through {@link WifiManager.MulticastLock}.
 */
final class AdbPortWatcher {
    private static final String TAG = "wanctl";

    /**
     * What adbd advertises its TLS connect listener as (AOSP {@code adb_mdns.h}).
     * The pairing listener is a different service, {@code _adb-tls-pairing._tcp},
     * and connecting to it would fail in a confusing way.
     */
    private static final String SERVICE_TYPE = "_adb-tls-connect._tcp.";

    /** How long to wait before starting discovery over after a failure. */
    private static final long RETRY_MS = 30_000L;

    /** Receives the discovered port, or 0 when the service goes away. */
    interface Sink {
        void onAdbPort(int port);
    }

    private final Context context;
    private final Sink sink;
    private final Handler handler = new Handler(Looper.getMainLooper());
    private final Deque<NsdServiceInfo> pending = new ArrayDeque<>();

    private NsdManager nsd;
    private WifiManager.MulticastLock multicastLock;
    private NsdManager.DiscoveryListener discovery;
    private boolean wanted;
    private boolean resolving;
    /** The service whose port was last published, so a loss can be matched. */
    private String acceptedService;

    AdbPortWatcher(Context context, Sink sink) {
        this.context = context.getApplicationContext();
        this.sink = sink;
    }

    synchronized void start() {
        wanted = true;
        if (discovery != null) {
            return;
        }
        nsd = context.getSystemService(NsdManager.class);
        if (nsd == null) {
            Log.w(TAG, "no NsdManager on this device; the adb channel will need WANCTL_ADB_PORT");
            return;
        }
        acquireMulticastLock();
        discovery = newDiscoveryListener();
        try {
            nsd.discoverServices(SERVICE_TYPE, NsdManager.PROTOCOL_DNS_SD, discovery);
        } catch (IllegalArgumentException e) {
            Log.w(TAG, "could not start mDNS discovery for " + SERVICE_TYPE, e);
            discovery = null;
            releaseMulticastLock();
            scheduleRetry();
        }
    }

    synchronized void stop() {
        wanted = false;
        handler.removeCallbacksAndMessages(null);
        if (discovery != null && nsd != null) {
            try {
                nsd.stopServiceDiscovery(discovery);
            } catch (IllegalArgumentException e) {
                // Discovery was already stopped by the framework; nothing to undo.
                Log.d(TAG, "mDNS discovery was already stopped", e);
            }
        }
        discovery = null;
        pending.clear();
        resolving = false;
        acceptedService = null;
        releaseMulticastLock();
    }

    private NsdManager.DiscoveryListener newDiscoveryListener() {
        return new NsdManager.DiscoveryListener() {
            @Override
            public void onDiscoveryStarted(String serviceType) {
                Log.i(TAG, "watching mDNS for " + serviceType);
            }

            @Override
            public void onServiceFound(NsdServiceInfo info) {
                enqueue(info);
            }

            @Override
            public void onServiceLost(NsdServiceInfo info) {
                forget(info);
            }

            @Override
            public void onDiscoveryStopped(String serviceType) {
            }

            @Override
            public void onStartDiscoveryFailed(String serviceType, int errorCode) {
                Log.w(TAG, "mDNS discovery failed to start (" + errorCode + ")");
                restartLater();
            }

            @Override
            public void onStopDiscoveryFailed(String serviceType, int errorCode) {
                Log.w(TAG, "mDNS discovery failed to stop (" + errorCode + ")");
            }
        };
    }

    private synchronized void enqueue(NsdServiceInfo info) {
        pending.addLast(info);
        pump();
    }

    /**
     * Resolves one service at a time. NsdManager rejects a second concurrent
     * resolve with FAILURE_ALREADY_ACTIVE, and a phone on a Wi-Fi network with
     * other developers on it will see several of these services at once.
     */
    private synchronized void pump() {
        if (resolving || pending.isEmpty() || nsd == null) {
            return;
        }
        NsdServiceInfo next = pending.pollFirst();
        resolving = true;
        nsd.resolveService(next, new NsdManager.ResolveListener() {
            @Override
            public void onServiceResolved(NsdServiceInfo resolved) {
                accept(resolved);
                resolveDone();
            }

            @Override
            public void onResolveFailed(NsdServiceInfo info, int errorCode) {
                Log.w(TAG, "could not resolve " + info.getServiceName() + " (" + errorCode + ")");
                resolveDone();
            }
        });
    }

    private synchronized void resolveDone() {
        resolving = false;
        pump();
    }

    private void accept(NsdServiceInfo info) {
        int port = info.getPort();
        InetAddress host = info.getHost();
        if (port <= 0 || port > 65535) {
            return;
        }
        // Every phone on this Wi-Fi with wireless debugging on advertises the
        // same service type. Ours is the one whose address this device holds;
        // anything else belongs to someone else's phone, and dialling it would
        // be both wrong and rude.
        if (!isOwnAddress(host)) {
            Log.d(TAG, "ignoring " + info.getServiceName() + " at " + host + ": not this device");
            return;
        }
        synchronized (this) {
            acceptedService = info.getServiceName();
        }
        Log.i(TAG, "this device's adbd is on port " + port);
        sink.onAdbPort(port);
    }

    private void forget(NsdServiceInfo info) {
        boolean ours;
        synchronized (this) {
            ours = info.getServiceName() != null && info.getServiceName().equals(acceptedService);
            if (ours) {
                acceptedService = null;
            }
        }
        if (ours) {
            // Wireless debugging was turned off, or Android rotated the port.
            // Publishing 0 is not the same as leaving the old value: something
            // else may be listening there by now.
            Log.i(TAG, "this device's adbd stopped advertising; clearing the port");
            sink.onAdbPort(0);
        }
    }

    private static boolean isOwnAddress(InetAddress addr) {
        if (addr == null) {
            return false;
        }
        if (addr.isLoopbackAddress()) {
            return true;
        }
        try {
            for (Enumeration<NetworkInterface> ifs = NetworkInterface.getNetworkInterfaces();
                 ifs.hasMoreElements(); ) {
                for (Enumeration<InetAddress> addrs = ifs.nextElement().getInetAddresses();
                     addrs.hasMoreElements(); ) {
                    if (addrs.nextElement().equals(addr)) {
                        return true;
                    }
                }
            }
        } catch (SocketException e) {
            Log.w(TAG, "could not enumerate this device's addresses", e);
        }
        return false;
    }

    private synchronized void restartLater() {
        if (discovery != null && nsd != null) {
            try {
                nsd.stopServiceDiscovery(discovery);
            } catch (IllegalArgumentException ignored) {
                // Already stopped; the retry starts a fresh listener anyway.
            }
        }
        discovery = null;
        releaseMulticastLock();
        scheduleRetry();
    }

    private void scheduleRetry() {
        handler.postDelayed(() -> {
            synchronized (AdbPortWatcher.this) {
                if (!wanted) {
                    return;
                }
            }
            start();
        }, RETRY_MS);
    }

    /**
     * Wi-Fi hardware filters multicast traffic away while the device is idle,
     * which is exactly when nothing is holding a lock. This is a device-wide
     * filter rather than a per-app one, so holding the lock is what keeps mDNS
     * announcements arriving at all — NsdManager itself listens in the system
     * process and does not take one on our behalf.
     */
    private void acquireMulticastLock() {
        if (multicastLock != null) {
            return;
        }
        WifiManager wifi = context.getSystemService(WifiManager.class);
        if (wifi == null) {
            return;
        }
        multicastLock = wifi.createMulticastLock("wanctl-adb-mdns");
        multicastLock.setReferenceCounted(false);
        try {
            multicastLock.acquire();
        } catch (SecurityException e) {
            Log.w(TAG, "could not hold a multicast lock; mDNS may be filtered", e);
            multicastLock = null;
        }
    }

    private void releaseMulticastLock() {
        if (multicastLock == null) {
            return;
        }
        if (multicastLock.isHeld()) {
            multicastLock.release();
        }
        multicastLock = null;
    }
}
