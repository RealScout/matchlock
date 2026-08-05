# Running Android (redroid) inside a matchlock sandbox VM

Proven end-to-end 2026-08-04: Android 15 arm64 boots inside an rsv2 sandbox VM,
the RSHomebuyerApp debug APK builds *inside the same VM*, and the app signs into
that instance's Rails — no emulator binary, no KVM, no host-side Android
tooling. This runbook records the full recipe and every trap we hit.

## Why redroid and not the emulator

Google ships **no linux-aarch64 Android emulator** in the SDK channel
(repository2-3.xml has linux_x64 / darwin_x64 / darwin_aarch64 / windows_x64
only). A CI build (`emulator-linux_aarch64_gfxstream`) exists on ci.android.com
but is unfetchable without authenticated `fetch_artifact`. Combined with no
`/dev/kvm` in the guest, emulator-based Android is a dead end. redroid runs
Android as a container on the guest kernel at native ARM speed.

## 1. Kernel (this PR)

redroid needs guest-kernel features the matchlock kernel didn't have. All are
`=y` (CONFIG_MODULES=n):

- `CONFIG_ANDROID_BINDER_IPC` / `CONFIG_ANDROID_BINDERFS` /
  `CONFIG_ANDROID_BINDER_DEVICES="binder,hwbinder,vndbinder"` — Android IPC.
  binderfs allows multiple Android instances per kernel. ashmem is gone since
  5.18; modern redroid needs binder only.
- `CONFIG_BINFMT_MISC` — lets qemu-user run the x86_64-only Android SDK
  tooling (aapt2, NDK clang) in the arm64 guest.
- `CONFIG_SECURITY` + `SECURITY_NETWORK` + `AUDIT` + `SECURITY_SELINUX` +
  `SELINUX_DEVELOP` — Android init expects /sys/fs/selinux; DEVELOP boots
  permissive. SECURITY_SELINUX's Kconfig deps drag the rest in.
- `CONFIG_MEMCG_V1` / `CPUSETS_V1` — Android's libprocessgroup sets task
  profiles against cgroup v1 (/dev/cpuctl, /dev/cpuset). Note 6.19 has **no**
  `CONFIG_CGROUP_V1`; that symbol was sitting in the config doing nothing.

`olddefconfig` drops unknown/unmet options **silently** — we found two phantom
symbols (`CONFIG_CGROUP_V1`, `CONFIG_SELINUX`) that read as authoritative but
did nothing. `guest/kernel/verify-config.sh` now fails the Docker kernel build
on any *new* drift; `known-drift.txt` holds the untriaged pre-existing drops.

Build: `KERNEL_VERSION=6.19.8-dmcrypt-vfs-binder2 KERNEL_SOURCE_VERSION=6.19.8
ARCH=arm64 ./scripts/build-kernel.sh` → lands in
`~/.cache/matchlock/kernels/<version>/kernel-arm64`.

## 2. Boot an instance on the new kernel (no fleet impact)

rsv2 pins the kernel in `qa/rsv2-cli/cmd/setup.go` (`sandboxKernelVersion`);
per-launch overrides avoid touching the pin:

```sh
RSV2_SANDBOX_KERNEL=$HOME/.cache/matchlock/kernels/6.19.8-dmcrypt-vfs-binder2/kernel-arm64 \
RSV2_SANDBOX_MEMORY_MB=9216 \
rsv2 sandbox 1 --detach
```

Restarting a VM destroys `/opt` and `/root` (ephemeral overlay). Salvage
first, over virtiofs, from a detached container so the exec channel stays free:

```sh
matchlock exec <vm> -- sh -c 'docker run -d --name salvage \
  -v /workspace/app/tmp:/out -v /opt:/s/opt:ro -v /root/.gradle:/s/root/.gradle:ro \
  alpine tar -C /s -cf /out/toolchain.tar opt/android-sdk opt/node20 opt/work root/.gradle'
```

**Per-VM CA trap:** every VM boot mints a fresh matchlock MITM CA. A salvaged
Java cacerts carries the old CA and Gradle fails with PKIX "signature check
failed" (while curl works — it uses the system store guest-init refreshes).
After any VM swap, re-import:

```sh
keytool -delete -cacerts -storepass changeit -alias matchlock
keytool -importcert -noprompt -cacerts -storepass changeit -alias matchlock \
  -file /etc/ssl/certs/matchlock-ca.crt
```

## 3. redroid

```sh
docker run -d --privileged --name redroid -v /data/redroid:/data -p 5555:5555 \
  redroid/redroid:15.0.0_64only-latest androidboot.redroid_gpu_mode=guest
```

- Boots to `sys.boot_completed=1` in ~2 min with ≥4 CPU / ≥6 GB (it failed
  mysteriously at 1 CPU / 512 MB — check resources before debugging Android).
- **Black screencap ≠ GPU bug.** redroid starts no Home app, so nothing draws.
  `am start -a android.intent.action.MAIN -c android.intent.category.HOME`
  starts the launcher. Read the GPU mode back as `ro.boot.redroid_gpu_mode`
  (NOT `redroid.gpu.mode`). Byte-size check: black 720x1280 PNG ≈ 6 KB, real
  content ≥ 50 KB.
- **redroid's netd programs no kernel routes** — policy-routing tables 1003/99
  are empty, so every app socket dies ENETUNREACH. Fix inside the container:

  ```sh
  ip rule add from all lookup main pref 20000
  ip route add default via 172.17.0.1 dev eth0 onlink   # only for egress
  ```

- No adb needed: `docker cp` + `docker exec redroid pm install`, `am start`,
  `input tap/text`, `screencap -p | base64`.
- No Google Play services (AOSP image): the app's map view is dead. Everything
  else works. A GApps/microG redroid variant exists if maps ever matter.

## 4. In-VM networking (the shield changes the rules)

- Containers **cannot** reach VM host-netns ports (shield INPUT chain) and
  **cannot** hairpin to published ports via the gateway. Same-bridge
  container-to-container traffic is the only reliable path.
- Put everything the app needs on redroid's bridge:
  `docker network connect bridge rsv2-qa-1-web-1`, run Metro as a bridge
  container. Talk container-IP to container-IP.
- Rails host authorization only admits `.rslocal.test` — point the name at the
  web container's bridge IP inside redroid (`/system/etc/hosts` works; bionic
  reads it) and give the app `http://www.rslocal.test:3000`.

## 5. Building the APK inside the VM

- **binfmt_misc is per-user-namespace, and every matchlock exec AND every
  docker container here gets its own instance.** A qemu registration made
  anywhere else is invisible to the build. Register inside the same exec that
  runs Gradle (F flag pins the interpreter at registration):

  ```sh
  mount -t binfmt_misc binfmt_misc /proc/sys/fs/binfmt_misc
  printf ':qemu-x86_64:M::\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00\x3e\x00:\xff\xff\xff\xff\xff\xfe\xfe\x00\xff\xff\xff\xff\xff\xff\xff\xff\xfe\xff\xff\xff:/usr/bin/qemu-x86_64-static:F\n' \
    > /proc/sys/fs/binfmt_misc/register
  ```

- Dynamic x86_64 binaries need amd64 multiarch libs:
  `dpkg --add-architecture amd64`, add an archive.ubuntu.com amd64 source
  (arm64 stays on ports.ubuntu.com), then
  `apt-get install libc6:amd64 libstdc++6:amd64 zlib1g:amd64`. dpkg's
  configure step fails without VM-wide binfmt — harmless; the unpacked files
  are what matters.
- AGP demands the aapt2 override be a file named exactly `aapt2`
  (`android.aapt2FromMavenOverride=/opt/aapt2-shim/aapt2`, a one-line
  qemu-exec wrapper).
- `reactNativeArchitectures=arm64-v8a` in gradle.properties — the default
  builds 4 ABIs; under emulation that's 4x the C++ for nothing.
- Shield allowlist needed: `dl.google.com maven.google.com
  repo.maven.apache.org plugins.gradle.org plugins-artifacts.gradle.org
  services.gradle.org archive.ubuntu.com ports.ubuntu.com`
  (`matchlock allow-list add <vm> ...`).
- Run the build as ONE long exec logging to virtiofs
  (`> /workspace/app/tmp/build.log`) and read the log from the host — execs
  serialize, and polling from a second exec collides with the build.
- Expo debug builds bundle no JS: run Metro
  (`npx expo start --offline --port 8081`) as a bridge-network container and
  aim the dev client with
  `am start -a android.intent.action.VIEW -d
  "rshomebuyerapp://expo-development-client/?url=http%3A%2F%2F<metro-ip>%3A8081"`.

## 6. Demo-data gotchas (rsv2 seed wizard)

- Seeded listings live in one MLS (RECSFAR here, all "san francisco") — a user
  whose saved searches target another MLS sees zero matches. jordan.reyes's QA
  searches target RECSFAR; harry.homebuyer's don't.
- Favorites are `Verdict` rows (`prose: 'saved'`); create them via Rails
  console to pre-populate the interested tab.
- The seed generator writes **relative** `/stub_photos/...` photo URLs — fine
  in a browser, opaque to React Native's `Image`. Absolutize in ES
  (`_update_by_query` prepending `http://www.rslocal.test:3000`) and photos
  render, served by the in-VM web container.

## Next direction: Rosetta instead of qemu

The one real tax left is qemu TCG emulation of the x86_64 NDK toolchain.
Virtualization.framework can inject Apple's Rosetta into a Linux guest as a
read-only directory share (`VZLinuxRosettaDirectoryShare`); registering
`/media/rosetta/rosetta` as the x86_64 binfmt handler gives near-native speed —
the mechanism Docker Desktop uses for fast amd64 containers. Security-neutral
relative to qemu: translation runs entirely in-guest, no host channel, one more
read-only virtiofs device. Requires host-side plumbing in the VZ config, which
we own in this fork.
