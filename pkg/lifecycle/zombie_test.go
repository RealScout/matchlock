package lifecycle

import (
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessRunning_RejectsZombie(t *testing.T) {
	// Start a child that exits immediately and don't reap it — a real zombie.
	cmd := exec.Command("true")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	_, _ = io.ReadAll(stdout) // EOF = child exited; unreaped until Wait()
	t.Cleanup(func() { _ = cmd.Wait() })

	pid := cmd.Process.Pid
	deadline := time.Now().Add(5 * time.Second)
	for processRunning(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	assert.False(t, processRunning(pid), "zombie supervisor must not count as running")
	assert.True(t, processRunning(os.Getpid()), "live process still counts as running")
}
