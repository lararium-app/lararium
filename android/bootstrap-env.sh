#!/bin/bash
# Bootstrap the Android build env on a CI box.
# One-time: run from the android/ directory.
set -euo pipefail
export JAVA_HOME=${JAVA_HOME:-/usr/lib/jvm/java-17-openjdk}
SDK=${ANDROID_HOME:-$HOME/android-sdk}

[ -x "$JAVA_HOME/bin/java" ] || { echo "JDK17 missing: sudo pacman -S jdk17-openjdk"; exit 1; }
[ -d "$SDK/cmdline-tools/latest" ] || {
  mkdir -p "$SDK/cmdline-tools"
  curl -sL -o /tmp/ct.zip https://dl.google.com/android/repository/commandlinetools-linux-11076708_latest.zip
  unzip -q /tmp/ct.zip -d "$SDK/cmdline-tools" && mv "$SDK/cmdline-tools/cmdline-tools" "$SDK/cmdline-tools/latest"
}
yes | "$SDK/cmdline-tools/latest/bin/sdkmanager" --licenses >/dev/null
"$SDK/cmdline-tools/latest/bin/sdkmanager" "platform-tools" "platforms;android-35" "build-tools;35.0.0" >/dev/null
echo "sdk.dir=$SDK" > local.properties
echo "ANDROID OK — build with: ~/gradle-8.11.1/bin/gradle assembleDebug testDebugUnitTest"
