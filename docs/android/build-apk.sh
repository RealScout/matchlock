#!/bin/sh
# Runs inside one matchlock exec. binfmt_misc instances are per-userns and
# every exec gets a fresh userns, so the qemu registration must happen in
# the same exec that runs the build — it cannot be done once VM-wide.
LOG=/workspace/app/tmp/apk-build.log
{
    set -ex
    mount -t binfmt_misc binfmt_misc /proc/sys/fs/binfmt_misc 2>/dev/null || true
    printf ':qemu-x86_64:M::\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00\x3e\x00:\xff\xff\xff\xff\xff\xfe\xfe\x00\xff\xff\xff\xff\xff\xff\xff\xff\xfe\xff\xff\xff:/usr/bin/qemu-x86_64-static:F\n' > /proc/sys/fs/binfmt_misc/register
    /opt/android-sdk/build-tools/36.0.0/aapt2 version
    export JAVA_HOME=/usr/lib/jvm/java-17-openjdk-arm64
    export ANDROID_HOME=/opt/android-sdk
    export ANDROID_SDK_ROOT=/opt/android-sdk
    export PATH="/opt/node20/bin:$JAVA_HOME/bin:$PATH"
    node --version
    cd /opt/work/RSHomebuyerApp/android
    ./gradlew assembleDebug
    cp app/build/outputs/apk/debug/app-debug.apk /workspace/app/tmp/RSHomebuyerApp-debug.apk
    echo BUILD_SCRIPT_DONE
} > "$LOG" 2>&1
