#!/usr/bin/env bash
# Run a packaged binary in a real, non-debuggable application sandbox.
# First start a disposable relay: WANCTL_TOKENS=sandbox-token:sandbox go run . relay --addr 127.0.0.1:18740
# Then: scripts/android-id-smoke.sh /absolute/path/to/the-emulator-ABI.apk
set -euo pipefail
APK=${1:?pass an APK matching the emulator ABI}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
OUT=$(mktemp -d)
SDK=${ANDROID_HOME:-${ANDROID_SDK_ROOT:-"$HOME/Library/Android/sdk"}}
BT="$SDK/build-tools/$(ls "$SDK/build-tools" | sort -V | tail -1)"
PLATFORM="$SDK/platforms/$(ls "$SDK/platforms" | sort -V | tail -1)/android.jar"
DEVICE=${WANCTL_TEST_DEVICE:-emulator-5554}
case "$DEVICE" in emulator-*) ;; *) echo 'Use an emulator, not a personal device.' >&2; exit 1;; esac
if [ -z "${JAVA_HOME:-}" ] && [ -d '/Applications/Android Studio.app/Contents/jbr/Contents/Home' ]; then
  JAVA_HOME='/Applications/Android Studio.app/Contents/jbr/Contents/Home'
  export JAVA_HOME
fi
if [ -n "${JAVA_HOME:-}" ]; then export PATH="$JAVA_HOME/bin:$PATH"; fi
ADB="$SDK/platform-tools/adb"
ABI=$("$ADB" -s "$DEVICE" shell getprop ro.product.cpu.abi | tr -d '\r')
cleanup() {
  "$ADB" -s "$DEVICE" uninstall dev.wanctl.idsandbox >/dev/null 2>&1 || true
  python3 -c 'import shutil,sys;shutil.rmtree(sys.argv[1])' "$OUT"
}
trap cleanup EXIT
mkdir -p "$OUT/classes" "$OUT/dex" "$OUT/gen" "$OUT/lib/$ABI"
unzip -p "$APK" lib/$ABI/libwanctl.so > "$OUT/lib/$ABI/libwanctl.so"
cat > "$OUT/AndroidManifest.xml" <<'EOF'
<manifest xmlns:android="http://schemas.android.com/apk/res/android" package="dev.wanctl.idsandbox" android:versionCode="1" android:versionName="test">
 <uses-sdk android:minSdkVersion="29" android:targetSdkVersion="35"/>
 <uses-permission android:name="android.permission.INTERNET"/>
 <application android:label="wanctl ID sandbox test" android:debuggable="false" android:extractNativeLibs="true" android:theme="@android:style/Theme.Material.Light.NoActionBar">
  <activity android:name=".Probe" android:exported="true"/>
 </application>
</manifest>
EOF
cp "$ROOT/tools/android-sandbox/Probe.java" "$OUT/Probe.java"

"$BT/aapt2" link -I "$PLATFORM" --manifest "$OUT/AndroidManifest.xml" --java "$OUT/gen" -o "$OUT/base.apk"
javac -Xlint:-options -source 17 -target 17 -encoding UTF-8 -classpath "$PLATFORM" -d "$OUT/classes" "$OUT/Probe.java"
find "$OUT/classes" -name '*.class' > "$OUT/classes.list"
"$BT/d8" --release --lib "$PLATFORM" --min-api 29 --output "$OUT/dex" @"$OUT/classes.list"
cp "$OUT/base.apk" "$OUT/unsigned.apk"
(cd "$OUT/dex" && zip -q "$OUT/unsigned.apk" classes.dex)
(cd "$OUT" && zip -q "$OUT/unsigned.apk" lib/$ABI/libwanctl.so)
"$BT/zipalign" -f 4 "$OUT/unsigned.apk" "$OUT/aligned.apk"
"$BT/apksigner" sign --ks "$HOME/.android/debug.keystore" --ks-pass pass:android --out "$OUT/test.apk" "$OUT/aligned.apk"
"$ADB" -s "$DEVICE" install -r "$OUT/test.apk"
"$ADB" -s "$DEVICE" shell am force-stop dev.wanctl.idsandbox
"$ADB" -s "$DEVICE" shell am start -n dev.wanctl.idsandbox/.Probe

PID=$("$ADB" -s "$DEVICE" shell pidof dev.wanctl.idsandbox | tr -d '\r')
for attempt in $(seq 1 45); do
  logs=$("$ADB" -s "$DEVICE" logcat -d --pid="$PID" -s WANCTL_ID_SANDBOX:I '*:S')
  if [[ "$logs" == *'FAIL '* ]]; then printf '%s\n' "$logs"; exit 1; fi
  if [[ "$logs" == *'PASS app_sandbox_agent_online=true'* ]]; then printf '%s\n' "$logs"; exit 0; fi
  sleep 1
done
printf '%s\n' "$logs"
echo 'Sandbox test timed out' >&2
exit 1
