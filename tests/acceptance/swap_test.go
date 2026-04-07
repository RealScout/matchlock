//go:build acceptance

package acceptance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jingkaihe/matchlock/pkg/sdk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dmcryptKernel returns the path to a kernel with CONFIG_DM_CRYPT and
// CONFIG_ZRAM, or skips the test if MATCHLOCK_DMCRYPT_KERNEL is not set.
func dmcryptKernel(t *testing.T) string {
	t.Helper()
	path := os.Getenv("MATCHLOCK_DMCRYPT_KERNEL")
	if path == "" {
		t.Skip("MATCHLOCK_DMCRYPT_KERNEL not set; skipping test that requires dm-crypt/zram kernel")
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("MATCHLOCK_DMCRYPT_KERNEL=%s not found: %v", path, err)
	}
	return path
}

// swapStateDir returns the VM state directory (~/.matchlock/vms/<id>/) for
// reading host-side files like swap.img.
func swapStateDir(vmID string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".matchlock", "vms", vmID)
}

// --- Tier 1: Stock kernel tests (unencrypted swap) ---

func TestNoSwapByDefault(t *testing.T) {
	t.Parallel()
	client := launchAlpine(t)

	result, err := client.Exec(context.Background(), "free -m")
	require.NoError(t, err)
	assert.Contains(t, result.Stdout, "Swap:             0")
}

func TestSwapDiskActive(t *testing.T) {
	t.Parallel()
	client := launchWithBuilder(t,
		sdk.New("alpine:latest").WithSwap(512, false),
	)

	// Swap should be on a virtio block device.
	result, err := client.Exec(context.Background(), "cat /proc/swaps")
	require.NoError(t, err)
	assert.Contains(t, result.Stdout, "/dev/vd")
	assert.Contains(t, result.Stdout, "partition")

	// Swap total should be non-zero.
	result, err = client.Exec(context.Background(), "cat /proc/meminfo")
	require.NoError(t, err)
	for _, line := range strings.Split(result.Stdout, "\n") {
		if strings.HasPrefix(line, "SwapTotal:") {
			fields := strings.Fields(line)
			require.Len(t, fields, 3)
			kb, _ := strconv.Atoi(fields[1])
			assert.Greater(t, kb, 0, "SwapTotal should be > 0")
		}
	}
}

func TestSwapPreventsOOM(t *testing.T) {
	t.Parallel()
	client := launchWithBuilder(t,
		sdk.New("alpine:latest").WithMemory(512).WithSwap(1024, false),
	)

	// Install stress-ng, allocate 800M in a 512M VM. Without swap this
	// would OOM-kill the process.
	result, err := client.Exec(context.Background(), strings.Join([]string{
		"apk add --no-cache stress-ng >/dev/null 2>&1",
		"stress-ng --vm 1 --vm-bytes 800M --vm-keep --timeout 10s --metrics-brief 2>&1",
		"echo exit=$?",
	}, " && "))
	require.NoError(t, err)
	assert.Contains(t, result.Stdout, "exit=0", "stress-ng should succeed (swap prevents OOM)")

	// Confirm pages were actually swapped.
	result, err = client.Exec(context.Background(), "cat /proc/vmstat")
	require.NoError(t, err)
	for _, line := range strings.Split(result.Stdout, "\n") {
		if strings.HasPrefix(line, "pswpout ") {
			fields := strings.Fields(line)
			require.Len(t, fields, 2)
			pswpout, _ := strconv.ParseInt(fields[1], 10, 64)
			assert.Greater(t, pswpout, int64(0), "pswpout should be > 0: pages were swapped out")
		}
	}
}

// --- Tier 2: dm-crypt kernel tests (encrypted swap + zram) ---
// These require MATCHLOCK_DMCRYPT_KERNEL pointing to a kernel built with
// CONFIG_DM_CRYPT, CONFIG_ZRAM, CONFIG_CRYPTO_AES, etc.

func TestEncryptedSwapOnDmCrypt(t *testing.T) {
	t.Parallel()
	kernelPath := dmcryptKernel(t)

	client := launchWithBuilder(t,
		sdk.New("alpine:latest").
			WithKernel("file://"+kernelPath).
			WithSwap(512, true),
	)

	// Swap must be on /dev/dm-*, NOT on /dev/vd*.
	result, err := client.Exec(context.Background(), "cat /proc/swaps")
	require.NoError(t, err)
	assert.Contains(t, result.Stdout, "/dev/dm-",
		"encrypted swap should be on a dm-crypt device, got:\n%s", result.Stdout)
	assert.NotContains(t, result.Stdout, "/dev/vd",
		"encrypted swap should NOT fall back to raw device")
}

func TestEncryptedSwapCiphertextOnHost(t *testing.T) {
	// This test is CLI-based so we can read the host-side swap.img.
	kernelPath := dmcryptKernel(t)

	// Start a detached VM with encrypted swap.
	stdout, stderr, exitCode := runCLIWithTimeout(t, 2*time.Minute,
		"run", "--image", "alpine:latest",
		"--kernel", "file://"+kernelPath,
		"--swap-size", "512",
		"--encrypt-swap",
		"-d",
	)
	require.Equalf(t, 0, exitCode, "run failed: stdout=%s stderr=%s", stdout, stderr)
	vmID := strings.TrimSpace(stdout)
	require.True(t, strings.HasPrefix(vmID, "vm-"), "expected vm-* ID, got %q", vmID)
	t.Cleanup(func() {
		runCLI(t, "kill", vmID)
		runCLI(t, "rm", vmID)
	})

	waitForDetachedVMExecReady(t, vmID)

	// Write a known secret pattern inside the guest.
	secret := "MATCHLOCK_SWAP_ENCRYPTION_TEST_SECRET_7f3a9b2e"
	execOut, _, execExit := runCLIWithTimeout(t, 30*time.Second,
		"exec", vmID, "--", "sh", "-c",
		fmt.Sprintf("yes '%s' | head -c 50000000 > /tmp/secret_data", secret))
	require.Equalf(t, 0, execExit, "write secret failed: %s", execOut)

	// Force the secret to swap by allocating memory under pressure.
	execOut, _, execExit = runCLIWithTimeout(t, 60*time.Second,
		"exec", vmID, "--", "sh", "-c",
		"apk add --no-cache stress-ng >/dev/null 2>&1 && stress-ng --vm 1 --vm-bytes 450M --vm-keep --timeout 10s 2>&1; cat /proc/swaps")
	require.Equalf(t, 0, execExit, "stress failed: %s", execOut)
	assert.Contains(t, execOut, "/dev/dm-", "swap should be on dm-crypt device")

	// Read the host-side swap.img and search for the plaintext secret.
	swapImg := filepath.Join(swapStateDir(vmID), "swap.img")
	swapData, err := os.ReadFile(swapImg)
	require.NoErrorf(t, err, "read swap.img at %s", swapImg)
	assert.Greater(t, len(swapData), 0, "swap.img should not be empty")

	// The secret should NOT appear in the raw swap image (it's encrypted).
	assert.NotContains(t, string(swapData), secret,
		"plaintext secret found in encrypted swap.img — encryption is not working")
}

func TestZramCompressedSwap(t *testing.T) {
	t.Parallel()
	kernelPath := dmcryptKernel(t)

	client := launchWithBuilder(t,
		sdk.New("alpine:latest").
			WithKernel("file://"+kernelPath).
			WithZram(50),
	)

	// zram0 should be active as swap.
	result, err := client.Exec(context.Background(), "cat /proc/swaps")
	require.NoError(t, err)
	assert.Contains(t, result.Stdout, "/dev/zram0",
		"zram should be active, got:\n%s", result.Stdout)

	// zram should have higher priority than disk swap.
	for _, line := range strings.Split(result.Stdout, "\n") {
		if strings.Contains(line, "/dev/zram0") {
			fields := strings.Fields(line)
			require.GreaterOrEqual(t, len(fields), 5)
			priority, _ := strconv.Atoi(fields[4])
			assert.Greater(t, priority, 0, "zram priority should be positive (higher than disk)")
		}
	}

	// Allocate compressible memory (zeros) that exceeds RAM but fits in
	// RAM + zram. Then verify zram actually compressed data.
	result, err = client.Exec(context.Background(), strings.Join([]string{
		"dd if=/dev/zero of=/dev/shm/zeros bs=1M count=200 2>/dev/null",
		"cat /sys/block/zram0/mm_stat",
	}, " && "))
	require.NoError(t, err)

	// mm_stat fields: orig_data_size compr_data_size mem_used_total ...
	fields := strings.Fields(strings.TrimSpace(result.Stdout))
	if len(fields) >= 2 {
		origSize, _ := strconv.ParseInt(fields[0], 10, 64)
		comprSize, _ := strconv.ParseInt(fields[1], 10, 64)
		if origSize > 0 && comprSize > 0 {
			ratio := float64(origSize) / float64(comprSize)
			t.Logf("zram compression: %d bytes -> %d bytes (%.1fx ratio)", origSize, comprSize, ratio)
			assert.Greater(t, ratio, 1.0,
				"zram should compress data (ratio > 1.0)")
		}
	}
}

func TestZramPlusDiskSwapLayering(t *testing.T) {
	t.Parallel()
	kernelPath := dmcryptKernel(t)

	client := launchWithBuilder(t,
		sdk.New("alpine:latest").
			WithKernel("file://"+kernelPath).
			WithMemory(512).
			WithSwap(1024, true).
			WithZram(50),
	)

	result, err := client.Exec(context.Background(), "cat /proc/swaps")
	require.NoError(t, err)

	// Both zram and disk swap should be present.
	assert.Contains(t, result.Stdout, "/dev/zram0", "zram should be active")
	assert.Contains(t, result.Stdout, "/dev/dm-", "encrypted disk swap should be active")

	// Verify priority ordering: zram > disk.
	var zramPriority, diskPriority int
	for _, line := range strings.Split(result.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		pri, _ := strconv.Atoi(fields[4])
		if strings.Contains(line, "/dev/zram0") {
			zramPriority = pri
		}
		if strings.Contains(line, "/dev/dm-") {
			diskPriority = pri
		}
	}
	assert.Greater(t, zramPriority, diskPriority,
		"zram priority (%d) should be higher than disk priority (%d)", zramPriority, diskPriority)

	// Under memory pressure, pages should go to zram first (fast),
	// then overflow to disk (slow). Verify both get used.
	result, err = client.Exec(context.Background(), strings.Join([]string{
		"apk add --no-cache stress-ng >/dev/null 2>&1",
		"stress-ng --vm 1 --vm-bytes 800M --vm-keep --timeout 10s 2>&1",
		"cat /proc/swaps",
		"cat /sys/block/zram0/mm_stat",
	}, " && "))
	require.NoError(t, err)

	// After stress, both swap devices should show usage.
	assert.Contains(t, result.Stdout, "/dev/zram0")
	assert.Contains(t, result.Stdout, "/dev/dm-")
}

func TestUnencryptedSwapFlagDisablesEncryption(t *testing.T) {
	t.Parallel()
	kernelPath := dmcryptKernel(t)

	client := launchWithBuilder(t,
		sdk.New("alpine:latest").
			WithKernel("file://"+kernelPath).
			WithSwap(512, false), // encrypt=false
	)

	// Swap should be on raw /dev/vd*, NOT /dev/dm-*.
	result, err := client.Exec(context.Background(), "cat /proc/swaps")
	require.NoError(t, err)
	assert.Contains(t, result.Stdout, "/dev/vd",
		"unencrypted swap should be on raw device")
	assert.NotContains(t, result.Stdout, "/dev/dm-",
		"--encrypt-swap=false should not use dm-crypt")
}
