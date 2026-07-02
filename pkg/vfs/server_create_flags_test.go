package vfs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FUSE_CREATE carries the guest's open flags, but they were discarded and
// every create ran as O_CREAT|O_TRUNC. If a file appears host-side between
// the guest's lookup and its create, an exclusive create (the standard
// lockfile idiom) must fail with EEXIST instead of truncating the host file.
func TestDispatchCreateExclDoesNotClobberExistingFile(t *testing.T) {
	dir := t.TempDir()
	s := NewVFSServer(NewRealFSProvider(dir))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f"), []byte("host data"), 0644))

	resp := s.dispatch(&VFSRequest{
		Op:    OpCreate,
		Path:  "f",
		Mode:  0644,
		Flags: linuxOWronly | linuxOCreat | linuxOExcl,
	})
	assert.Equal(t, -int32(syscall.EEXIST), resp.Err)

	got, err := os.ReadFile(filepath.Join(dir, "f"))
	require.NoError(t, err)
	assert.Equal(t, "host data", string(got), "exclusive create must not clobber the existing file")
}

func TestDispatchCreateWithoutTruncPreservesExistingContent(t *testing.T) {
	dir := t.TempDir()
	s := NewVFSServer(NewRealFSProvider(dir))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f"), []byte("host data"), 0644))

	resp := s.dispatch(&VFSRequest{
		Op:    OpCreate,
		Path:  "f",
		Mode:  0644,
		Flags: linuxOWronly | linuxOCreat,
	})
	require.Equal(t, int32(0), resp.Err)
	defer s.dispatch(&VFSRequest{Op: OpRelease, Handle: resp.Handle})

	got, err := os.ReadFile(filepath.Join(dir, "f"))
	require.NoError(t, err)
	assert.Equal(t, "host data", string(got))
}

func TestDispatchCreateNewFileWithFlags(t *testing.T) {
	dir := t.TempDir()
	s := NewVFSServer(NewRealFSProvider(dir))

	resp := s.dispatch(&VFSRequest{
		Op:    OpCreate,
		Path:  "new.txt",
		Mode:  0644,
		Flags: linuxOWronly | linuxOCreat | linuxOExcl,
	})
	require.Equal(t, int32(0), resp.Err)
	require.NotNil(t, resp.Stat)
	defer s.dispatch(&VFSRequest{Op: OpRelease, Handle: resp.Handle})

	_, err := os.Stat(filepath.Join(dir, "new.txt"))
	assert.NoError(t, err)
}

// Legacy guests omit flags entirely; the old create-and-truncate behavior
// must be preserved for them.
func TestDispatchCreateNoFlagsKeepsLegacyTruncate(t *testing.T) {
	dir := t.TempDir()
	s := NewVFSServer(NewRealFSProvider(dir))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f"), []byte("old"), 0644))

	resp := s.dispatch(&VFSRequest{Op: OpCreate, Path: "f", Mode: 0644})
	require.Equal(t, int32(0), resp.Err)
	defer s.dispatch(&VFSRequest{Op: OpRelease, Handle: resp.Handle})

	got, err := os.ReadFile(filepath.Join(dir, "f"))
	require.NoError(t, err)
	assert.Empty(t, string(got))
}

func TestMemoryProviderOpenExclRejectsExistingFile(t *testing.T) {
	p := NewMemoryProvider()
	require.NoError(t, p.WriteFile("/f", []byte("data"), 0644))

	_, err := p.Open("/f", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	assert.ErrorIs(t, err, syscall.EEXIST)
}
