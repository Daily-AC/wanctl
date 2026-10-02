#!/usr/bin/env python3
"""Home-screen approval test for the Android app, on an emulator, over the real chain.

Setup (once): run phonestack (see main.go); on the emulator sign the app in to
http://10.0.2.2:18995 / :18996 with an enroll code and enable it; make it the
approval phone (POST /api/approval-phone); give a controller config dir the
controller token from info.json, `wanctl trust server` the target, and trust
the controller once when the pairing card comes up. After the phone's agent
restarts, wait until the stack logs "approval phone … online": a request made
before that has no approver and is refused.

    WANCTL_CONFIG_DIR=<controller dir> tools/phonestack/home_test.py <wanctl binary>

1. One request: the home screen is its card; 允许一次 on the card; within
   8 seconds the card is gone and the command ran. v0.20.3 kept an answered
   card on the home screen until something else redrew it.
2. Two requests: the card is the older one and says 还有 1 条; both are
   answered from the home card.
"""
import os, re, subprocess, sys, time

WANCTL = sys.argv[1] if len(sys.argv) > 1 else "wanctl"
DEVICE = os.environ.get("WANCTL_TEST_DEVICE", "emulator-5554")
if not DEVICE.startswith("emulator-"):
    sys.exit("Use an emulator, not a personal device.")


def adb(*args):
    return subprocess.run(["adb", "-s", DEVICE, *args], capture_output=True, text=True).stdout


def screen():
    # A screen whose countdown ticks may never go idle, and then uiautomator
    # writes nothing: never read a dump left over from an earlier call.
    for _ in range(5):
        adb("shell", "rm", "-f", "/sdcard/home-test.xml")
        if "dumped" in adb("shell", "uiautomator", "dump", "/sdcard/home-test.xml"):
            break
    else:
        sys.exit("FAIL: uiautomator could not dump the screen")
    xml = adb("shell", "cat", "/sdcard/home-test.xml")
    return [(t.replace("&amp;", "&"), int(x1), int(y1), int(x2), int(y2)) for t, x1, y1, x2, y2 in
            re.findall(r'text="([^"]*)"[^>]*?bounds="\[(\d+),(\d+)\]\[(\d+),(\d+)\]"', xml)]


def until(check, seconds, what):
    end = time.time() + seconds
    while time.time() < end:
        texts = screen()
        if check([t for t, *_ in texts]):
            return texts
        time.sleep(0.3)
    sys.exit(f"FAIL: {what} within {seconds:g}s; screen: {[t for t, *_ in screen() if t]}")


def tap(texts, label):
    for t, x1, y1, x2, y2 in texts:
        if t == label:
            adb("shell", "input", "tap", str((x1 + x2) // 2), str((y1 + y2) // 2))
            return
    sys.exit(f"FAIL: no {label!r} on screen")


def request(cmd):
    return subprocess.Popen([WANCTL, "exec", "--target", "phonestack-target", "--", cmd],
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)


def answered(proc, want):
    out, _ = proc.communicate(timeout=30)
    if proc.returncode != 0 or want not in out:
        sys.exit(f"FAIL: exec exit {proc.returncode}: {out.strip()}")


adb("shell", "am", "start", "-n", "dev.wanctl.agent/.MainActivity")
until(lambda t: "允许一次" not in t, 10, "a home screen with nothing waiting")

tag = f"home-test-{int(time.time())}"
one = request(f"echo {tag}-1")
texts = until(lambda t: f"echo {tag}-1" in t and "允许一次" in t, 20, "the request as the home card")
tap(texts, "允许一次")
start = time.time()
until(lambda t: f"echo {tag}-1" not in t, 8, "the answered card leaving the home screen")
print(f"ok 1: answered card gone after {time.time() - start:.1f}s")
answered(one, f"{tag}-1")

first = request(f"echo {tag}-2")
until(lambda t: f"echo {tag}-2" in t, 20, "the first of two requests")
second = request(f"echo {tag}-3")
texts = until(lambda t: f"echo {tag}-2" in t and "还有 1 条" in t, 20, "还有 1 条 under the older card")
tap(texts, "允许一次")
texts = until(lambda t: f"echo {tag}-3" in t and "允许一次" in t and "还有 1 条" not in t, 5,
              "the next card after the first answer")
tap(texts, "允许一次")
until(lambda t: f"echo {tag}-3" not in t and "允许一次" not in t, 5, "the home screen emptying")
answered(first, f"{tag}-2")
answered(second, f"{tag}-3")
print("ok 2: two requests, 还有 1 条, both answered from the home card")
print("PASS")
