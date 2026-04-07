# Swap Support for Matchlock VMs

## Overview

Matchlock VMs run with a fixed memory allocation that macOS holds as wired
(non-reclaimable) host memory. When guest workloads exceed available RAM,
the Linux OOM killer terminates processes. The `--swap-size` flag attaches a
dedicated swap disk so the guest can page cold memory to disk instead of
OOM-killing.

## Usage

```bash
# 4GB RAM + 2GB swap
matchlock run --memory 4096 --swap-size 2048 --image ubuntu:22.04 -- ...

# Disable encryption for maximum swap throughput (not recommended with secrets)
matchlock run --memory 4096 --swap-size 2048 --encrypt-swap=false --image ubuntu:22.04 -- ...
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--swap-size <MB>` | 0 (disabled) | Size of the swap disk in MB. Creates a sparse file on the host. |
| `--encrypt-swap` | true | Encrypt swap with dm-crypt using an ephemeral random key. Requires `CONFIG_DM_CRYPT` in the guest kernel. Falls back to unencrypted if unavailable. |

## How It Works

### Host side

1. A sparse file is created at `~/.matchlock/vms/<id>/swap.img` via
   `Truncate()` (instant, no I/O).
2. The file is attached as an additional virtio block device.
3. The device name (e.g., `vdd`) is passed to the guest via kernel cmdline:
   `matchlock.swap=vdd matchlock.encrypt_swap=1`.

### Guest side (guest-init)

When `matchlock.encrypt_swap=1`:

1. Open `/dev/mapper/control` and create a dm-crypt device (`swap-crypt`)
   using device-mapper ioctls.
2. Generate a random 256-bit key from `/dev/urandom`.
3. Load a crypt target: `aes-xts-plain64 <hex-key> 0 /dev/vdd 0`.
4. Activate the device, producing `/dev/dm-0`.
5. Write a minimal swap header to `/dev/dm-0`.
6. Call `swapon` on `/dev/dm-0`.

When `matchlock.encrypt_swap` is not set (or dm-crypt unavailable):

1. Write a swap header directly to `/dev/vdd`.
2. Call `swapon` on `/dev/vdd`.

The swap header is written in Go without `mkswap` — guest-init constructs the
`swap_header` struct and `SWAPSPACE2` magic directly, since `mkswap` is not
available in the minimal guest environment.

### Key management

The encryption key is generated from `/dev/urandom` at boot, passed to the
kernel's dm-crypt subsystem, and never written to disk. It exists only in
guest kernel memory. When the VM stops, the key is lost and the ciphertext
in `swap.img` becomes irrecoverable.

## Security Considerations

**With `--encrypt-swap` (default):** Secrets in guest memory that are paged
to swap are encrypted with AES-256-XTS before reaching the host filesystem.
The key is ephemeral — lost when the VM stops. Even if the host is
compromised while the VM is running, the key is in guest kernel memory, not
accessible from the host.

**With `--encrypt-swap=false`:** Swap data is written in plaintext to the
host filesystem. Any secrets in guest memory (API keys, tokens, credentials)
that get swapped out are recoverable from `swap.img`. Use this only when
swap performance is critical and no secrets are present.

**Cleanup:** The swap.img file is removed when the sandbox is cleaned up
(`matchlock kill` or `--rm`). If the matchlock process is killed
ungracefully, the file persists in `~/.matchlock/vms/<id>/swap.img`.

## Performance

Benchmarked on Apple M-series, 512MB RAM VM, 1GB swap, 800MB stress-ng
allocation (forces heavy swap thrashing):

| Configuration | bogo ops/s | Overhead |
|---|---|---|
| No swap (in-RAM only, self-throttled) | 149,105 | baseline |
| Unencrypted swap | 2,043 | ~73x slower than RAM |
| Encrypted swap (AES-CE hardware) | 680 | ~3x slower than unencrypted |
| Encrypted swap (AES generic, no CE) | 213 | ~10x slower than unencrypted |

**Notes:**
- The "no swap" baseline is not directly comparable — stress-ng throttled
  its allocation to fit in RAM, so it wasn't doing any I/O.
- The relevant comparison is encrypted vs unencrypted swap: **~3x overhead**
  with hardware AES, which is the expected cost of per-page encrypt/decrypt
  through dm-crypt.
- Real workloads rarely thrash swap continuously. Most memory access hits
  RAM; swap handles cold pages. The practical impact is much smaller than
  the worst-case benchmark.
- ARM64 hardware AES acceleration (`CONFIG_CRYPTO_AES_ARM64_CE`) is
  critical — without it, encryption is ~10x slower instead of ~3x.

## Kernel Configuration

The following kernel config options are required (added to both
`guest/kernel/arm64.config` and `guest/kernel/x86_64.config`):

```
# Device mapper and dm-crypt
CONFIG_MD=y
CONFIG_BLK_DEV_DM=y
CONFIG_DM_CRYPT=y

# Cryptographic algorithms
CONFIG_CRYPTO=y
CONFIG_CRYPTO_AES=y
CONFIG_CRYPTO_XTS=y

# ARM64 hardware AES acceleration (arm64.config only)
CONFIG_CRYPTO_AES_ARM64_CE=y
CONFIG_CRYPTO_AES_ARM64_CE_BLK=y
```

A kernel rebuild is required for encrypted swap to work. Without these
options, `--encrypt-swap` falls back to unencrypted swap with a warning.

## Implementation Details

### No external binaries

Guest-init uses no external tools (`mkswap`, `cryptsetup`, etc.). Everything
is done via syscalls and ioctls:

- **Swap header:** Written directly in Go — constructs the `swap_header`
  struct per `include/linux/swap.h`.
- **dm-crypt:** Set up via device-mapper ioctls (`DM_DEV_CREATE`,
  `DM_TABLE_LOAD`, `DM_DEV_SUSPEND`) against `/dev/mapper/control`.
- **Block device size:** Read via `BLKGETSIZE64` ioctl (since `Stat().Size()`
  returns 0 for block devices).
- **swapon:** Called via `SYS_SWAPON` syscall.

### Inspired by

- **[LinuxKit](https://github.com/linuxkit/linuxkit)** (`pkg/swap/swap.sh`) —
  the only other lightweight VM project with encrypted swap. Uses
  `cryptsetup open --type plain --key-file /dev/urandom`. We replicate this
  approach but use ioctls instead of shelling out to `cryptsetup`.
- **Linux `/etc/crypttab`** — the standard distro approach to encrypted swap
  partitions with ephemeral keys from `/dev/urandom`.
- **[Firecracker](https://github.com/firecracker-microvm/firecracker)** —
  default guest kernel configs include `CONFIG_SWAP=y` but no dm-crypt.
- **[Podman Machine](https://docs.podman.io/)** — uses zram (compressed RAM
  swap) instead of disk-backed swap. A complementary approach that avoids
  the encryption question entirely.

### Files changed

| File | Change |
|------|--------|
| `cmd/matchlock/cmd_run.go` | `--swap-size` and `--encrypt-swap` flags |
| `pkg/api/config.go` | `SwapSizeMB` and `EncryptSwap` on `Resources` |
| `pkg/vm/backend.go` | `SwapPath` and `EncryptSwap` on `VMConfig` |
| `pkg/sandbox/rootfs.go` | `createSwapImage()` — sparse file creation |
| `pkg/sandbox/sandbox_darwin.go` | Swap image creation, plumbing to VMConfig |
| `pkg/sandbox/sandbox_linux.go` | Same |
| `pkg/vm/darwin/backend.go` | Attach swap block device, kernel args |
| `pkg/vm/linux/backend.go` | Same for Firecracker |
| `cmd/guest-init/main.go` | Parse swap args, dm-crypt setup, swapon |
| `guest/kernel/arm64.config` | dm-crypt + AES-CE config options |
| `guest/kernel/x86_64.config` | dm-crypt config options |
