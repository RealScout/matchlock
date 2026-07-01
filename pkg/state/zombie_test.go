package state

import (
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// spawnZombie starts a child that exits immediately and is deliberately NOT
// reaped, leaving a real zombie for the duration of the test. Returns the
// zombie's PID and a reap func (registered via t.Cleanup as well).
func spawnZombie(t *testing.T) (int, func()) {
	t.Helper()
	cmd := exec.Command("true")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	// EOF on the pipe means the child closed its stdout, i.e. it has exited.
	// Until Wait() is called it stays a zombie.
	_, _ = io.ReadAll(stdout)
	reap := func() { _ = cmd.Wait() }
	t.Cleanup(reap)
	return cmd.Process.Pid, reap
}

func TestIsProcessZombie_RealZombie(t *testing.T) {
	pid, reap := spawnZombie(t)

	// Brief poll: the exit-to-zombie transition can lag the pipe EOF by a tick.
	deadline := time.Now().Add(5 * time.Second)
	for !IsProcessZombie(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	assert.True(t, IsProcessZombie(pid), "unreaped exited child should read as zombie")

	// The whole point of the fix: a zombie must NOT count as running,
	// even though Signal(0) still succeeds for it.
	mgr := NewManagerWithDir(t.TempDir())
	assert.False(t, mgr.isProcessRunning(pid))

	// After reaping, the PID is gone entirely.
	reap()
	assert.False(t, IsProcessZombie(pid))
	assert.False(t, mgr.isProcessRunning(pid))
}

func TestIsProcessZombie_LiveSelfIsNotZombie(t *testing.T) {
	// The test process itself is alive and running, never a zombie.
	assert.False(t, IsProcessZombie(os.Getpid()))
}

func TestIsProcessZombie_InvalidPIDs(t *testing.T) {
	assert.False(t, IsProcessZombie(0))
	assert.False(t, IsProcessZombie(-1))
	// A PID that (almost certainly) doesn't exist: ps/proc lookup fails, and a
	// failed lookup must not be reported as a zombie.
	assert.False(t, IsProcessZombie(999999))
}

func TestIsProcessRunning_LiveSelf(t *testing.T) {
	mgr := NewManagerWithDir(t.TempDir())
	// Our own live PID passes both the Signal(0) check and the zombie reject.
	assert.True(t, mgr.isProcessRunning(os.Getpid()))
	assert.False(t, mgr.isProcessRunning(0))
}
