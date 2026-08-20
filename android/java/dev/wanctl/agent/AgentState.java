package dev.wanctl.agent;

import android.os.Handler;
import android.os.Looper;

import java.util.ArrayDeque;
import java.util.ArrayList;
import java.util.List;

/**
 * What the service knows about the agent child process, and what the UI reads.
 *
 * <p>Process liveness is first-hand: the service owns the child, so it does not
 * have to infer anything. Whether the agent is actually <em>registered with the
 * relay</em> is not something the parent can observe directly, so it is read off
 * the agent's own stdout — that is a report, not a measurement, and the UI
 * labels the two separately rather than collapsing them into one "online".
 */
final class AgentState {
    enum Phase {
        /** No child process, because the user has not enabled the agent. */
        STOPPED,
        /** Child process started, nothing reported yet. */
        STARTING,
        /** The agent printed its "online via …" line. */
        ONLINE,
        /** The child exited and the service is waiting out a backoff. */
        RETRYING,
        /** The child exited for a reason retrying cannot fix (e.g. a rejected token). */
        ERROR,
    }

    private static final AgentState INSTANCE = new AgentState();
    private static final int LOG_LINES = 800;

    static AgentState get() {
        return INSTANCE;
    }

    private final Handler main = new Handler(Looper.getMainLooper());
    private final List<Runnable> listeners = new ArrayList<>();
    private final ArrayDeque<String> log = new ArrayDeque<>();

    private Phase phase = Phase.STOPPED;
    private String relay = "";
    private String fingerprint = "";
    private String detail = "";

    private AgentState() {
    }

    synchronized Phase phase() {
        return phase;
    }

    synchronized String relay() {
        return relay;
    }

    synchronized String fingerprint() {
        return fingerprint;
    }

    /** A human-readable elaboration on the phase: the backoff, or why it stopped. */
    synchronized String detail() {
        return detail;
    }

    synchronized void setPhase(Phase p, String detail) {
        this.phase = p;
        this.detail = detail == null ? "" : detail;
        if (p == Phase.STOPPED || p == Phase.ERROR) {
            relay = "";
        }
        notifyListeners();
    }

    synchronized void setOnline(String relay, String fingerprint) {
        this.phase = Phase.ONLINE;
        this.detail = "";
        this.relay = relay;
        if (fingerprint != null && !fingerprint.isEmpty()) {
            this.fingerprint = fingerprint;
        }
        notifyListeners();
    }

    synchronized void setFingerprint(String fp) {
        if (fp != null && !fp.isEmpty() && !fp.equals(fingerprint)) {
            fingerprint = fp;
            notifyListeners();
        }
    }

    synchronized void append(String line) {
        log.addLast(line);
        while (log.size() > LOG_LINES) {
            log.removeFirst();
        }
        notifyListeners();
    }

    synchronized String logText() {
        return String.join("\n", log);
    }

    void addListener(Runnable r) {
        synchronized (this) {
            listeners.add(r);
        }
    }

    void removeListener(Runnable r) {
        synchronized (this) {
            listeners.remove(r);
        }
    }

    private void notifyListeners() {
        List<Runnable> snapshot = new ArrayList<>(listeners);
        for (Runnable r : snapshot) {
            main.post(r);
        }
    }
}
