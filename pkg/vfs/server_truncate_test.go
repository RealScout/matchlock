package vfs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ftruncate(2) from the guest arrives as OpTruncate with the target size in
// Offset. It must work for any size, not just zero — shrinking to a nonzero
// length previously reported success while changing nothing.
func TestDispatchTruncateShrinksToNonZeroSize(t *testing.T) {
	dir := t.TempDir()
	s := NewVFSServer(NewRealFSProvider(dir))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f"), []byte("line1\nline2\n"), 0644))

	resp := s.dispatch(&VFSRequest{Op: OpTruncate, Path: "f", Offset: 6})
	require.Equal(t, int32(0), resp.Err)

	got, err := os.ReadFile(filepath.Join(dir, "f"))
	require.NoError(t, err)
	assert.Equal(t, "line1\n", string(got))
}

func TestDispatchTruncateExtendsFile(t *testing.T) {
	dir := t.TempDir()
	s := NewVFSServer(NewRealFSProvider(dir))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f"), []byte("abc"), 0644))

	resp := s.dispatch(&VFSRequest{Op: OpTruncate, Path: "f", Offset: 10})
	require.Equal(t, int32(0), resp.Err)

	info, err := os.Stat(filepath.Join(dir, "f"))
	require.NoError(t, err)
	assert.Equal(t, int64(10), info.Size())
}

func TestDispatchTruncateViaOpenHandle(t *testing.T) {
	dir := t.TempDir()
	s := NewVFSServer(NewRealFSProvider(dir))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f"), []byte("line1\nline2\n"), 0644))

	open := s.dispatch(&VFSRequest{Op: OpOpen, Path: "f", Flags: 0x2}) // Linux O_RDWR
	require.Equal(t, int32(0), open.Err)
	defer s.dispatch(&VFSRequest{Op: OpRelease, Handle: open.Handle})

	resp := s.dispatch(&VFSRequest{Op: OpTruncate, Handle: open.Handle, Offset: 6})
	require.Equal(t, int32(0), resp.Err)

	got, err := os.ReadFile(filepath.Join(dir, "f"))
	require.NoError(t, err)
	assert.Equal(t, "line1\n", string(got))
}
