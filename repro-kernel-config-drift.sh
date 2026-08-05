#!/usr/bin/env bash
# Reproduction: kernel 6.19.8 olddefconfig silently drops netfilter options
# required for Docker bridge/NAT networking.
#
# This runs inside Docker — no host toolchain needed.
# Requires: Docker with BuildKit.
#
# Expected result:
#   6.1.137: all 6 iptables options preserved after olddefconfig
#   6.19.8:  all 6 iptables options silently dropped after olddefconfig
#
# See: https://github.com/jingkaihe/matchlock/issues/XXX

set -euo pipefail

CONFIG_DIR="$(cd "$(dirname "$0")/guest/kernel" && pwd)"
ARM64_CONFIG="$CONFIG_DIR/arm64.config"

if [ ! -f "$ARM64_CONFIG" ]; then
  echo "ERROR: arm64.config not found at $ARM64_CONFIG" >&2
  exit 1
fi

# Explicitly set in arm64.config, and all `default n` with nothing selecting
# them — so olddefconfig will drop them silently rather than erroring.
# Netfilter: Docker iptables-legacy NAT/filter/mangle support.
# Binder: containerised Android (redroid) in the guest.
# BINFMT_MISC: qemu-user registration for foreign-arch binaries.
# CONFIG_ANDROID_BINDER_DEVICES is omitted deliberately — it is a string
# option, not =y, and its upstream default is already the value we want.
CRITICAL_OPTIONS=(
  CONFIG_IP_NF_FILTER
  CONFIG_IP_NF_MANGLE
  CONFIG_IP_NF_NAT
  CONFIG_IP6_NF_FILTER
  CONFIG_IP6_NF_MANGLE
  CONFIG_IP6_NF_NAT
  CONFIG_BINFMT_MISC
  CONFIG_ANDROID_BINDER_IPC
  CONFIG_ANDROID_BINDERFS
)

# Derived so the grep below cannot drift from the list above.
CRITICAL_REGEX=$(IFS='|'; echo "${CRITICAL_OPTIONS[*]}")

check_kernel() {
  local version="$1"
  local source_version="$version"

  # kernel.org strips trailing .0 from URLs
  case "$source_version" in
    *.*.0) source_version="${source_version%.0}" ;;
  esac

  echo ""
  echo "=== Kernel $version ==="

  local result
  result=$(docker run --rm \
    -v "$ARM64_CONFIG:/config/arm64.config:ro" \
    ubuntu:22.04 bash -c "
      apt-get update -qq >/dev/null 2>&1
      apt-get install -y -qq --no-install-recommends \
        build-essential bc flex bison libelf-dev libssl-dev wget ca-certificates \
        gcc-aarch64-linux-gnu >/dev/null 2>&1

      wget -qO /tmp/linux.tar.xz \
        https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-${source_version}.tar.xz
      mkdir /tmp/linux-src
      tar xf /tmp/linux.tar.xz -C /tmp/linux-src --strip-components=1

      cp /config/arm64.config /tmp/linux-src/.config
      cd /tmp/linux-src
      make ARCH=arm64 CROSS_COMPILE=aarch64-linux-gnu- olddefconfig >/dev/null 2>&1

      grep -E '^(${CRITICAL_REGEX})=' .config || true
    " 2>&1)

  echo "$result"

  local dropped=0
  for opt in "${CRITICAL_OPTIONS[@]}"; do
    if ! echo "$result" | grep -q "${opt}=y"; then
      echo "  DROPPED: ${opt}"
      dropped=$((dropped + 1))
    fi
  done

  if [ $dropped -eq 0 ]; then
    echo "  All ${#CRITICAL_OPTIONS[@]} critical options preserved."
  else
    echo "  $dropped/${#CRITICAL_OPTIONS[@]} critical options silently dropped."
  fi
}

echo "Reproduction: arm64.config default-n options dropped by olddefconfig on 6.19.8"
echo "Config: $ARM64_CONFIG"

check_kernel "6.1.137"
check_kernel "6.19.8"

echo ""
echo "If 6.19.8 shows dropped options, the bug is confirmed."
echo "Docker fails with: iptables ... can't initialize iptables table 'nat'"
