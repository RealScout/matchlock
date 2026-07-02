package vfs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Linux O_WRONLY as sent by the in-VM FUSE daemon. The remaining Linux flag
// bits used below are shared with the translator in guestflags.go —
// deliberately NOT syscall.O_*, which would be the host's values.
const linuxOWronly = 0x1

// Reproduces the reopen-append data-loss bug: each open/append-write/close
// cycle from the guest must preserve content written by earlier cycles.
// The guest kernel opens with O_WRONLY|O_APPEND and positions the write at
// the current EOF, so the host must not truncate on open.
func TestDispatchReopenAppendPreservesExistingContent(t *testing.T) {
	dir := t.TempDir()
	s := NewVFSServer(NewRealFSProvider(dir))

	lines := []string{"line1\n", "line2\n", "line3\n"}
	offset := int64(0)

	// Cycle 1: file does not exist yet, so the guest kernel issues FUSE_CREATE.
	create := s.dispatch(&VFSRequest{Op: OpCreate, Path: "f", Mode: 0644})
	require.Equal(t, int32(0), create.Err)
	w := s.dispatch(&VFSRequest{Op: OpWrite, Handle: create.Handle, Offset: offset, Data: []byte(lines[0])})
	require.Equal(t, int32(0), w.Err)
	offset += int64(len(lines[0]))
	s.dispatch(&VFSRequest{Op: OpRelease, Handle: create.Handle})

	// Cycles 2 and 3: the file exists, so the guest kernel issues FUSE_OPEN
	// with Linux O_WRONLY|O_APPEND and sends the write at the current EOF.
	for _, line := range lines[1:] {
		open := s.dispatch(&VFSRequest{Op: OpOpen, Path: "f", Flags: linuxOWronly | linuxOAppend})
		require.Equal(t, int32(0), open.Err)
		w := s.dispatch(&VFSRequest{Op: OpWrite, Handle: open.Handle, Offset: offset, Data: []byte(line)})
		require.Equal(t, int32(0), w.Err)
		offset += int64(len(line))
		s.dispatch(&VFSRequest{Op: OpRelease, Handle: open.Handle})
	}

	got, err := os.ReadFile(filepath.Join(dir, "f"))
	require.NoError(t, err)
	assert.NotContains(t, string(got), "\x00",
		"host file has NUL holes where earlier appends were truncated away")
	assert.Equal(t, "line1\nline2\nline3\n", string(got))
}
