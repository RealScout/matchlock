package vfs

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// utimens from the guest (tar -x, cp -p, rsync -a, touch -d) arrives as
// OpChtimes. It previously had no dispatch case, so timestamp updates were
// silently dropped and files kept "now" as their mtime.
func TestDispatchChtimesSetsModTime(t *testing.T) {
	dir := t.TempDir()
	s := NewVFSServer(NewRealFSProvider(dir))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f"), []byte("data"), 0644))

	want := time.Date(2020, 6, 15, 12, 0, 0, 0, time.UTC)
	mtime := want.UnixNano()
	resp := s.dispatch(&VFSRequest{Op: OpChtimes, Path: "f", MtimeNS: &mtime})
	require.Equal(t, int32(0), resp.Err)

	info, err := os.Stat(filepath.Join(dir, "f"))
	require.NoError(t, err)
	assert.True(t, info.ModTime().Equal(want), "mtime = %v, want %v", info.ModTime(), want)
}

func TestDispatchChtimesMtimeOnlyLeavesAtimeUsable(t *testing.T) {
	dir := t.TempDir()
	s := NewVFSServer(NewRealFSProvider(dir))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f"), []byte("data"), 0644))

	mtime := time.Date(2021, 1, 2, 3, 4, 5, 0, time.UTC).UnixNano()
	resp := s.dispatch(&VFSRequest{Op: OpChtimes, Path: "f", MtimeNS: &mtime})
	require.Equal(t, int32(0), resp.Err)
}

func TestMemoryProviderChtimes(t *testing.T) {
	p := NewMemoryProvider()
	require.NoError(t, p.WriteFile("/f", []byte("data"), 0644))

	want := time.Date(2020, 6, 15, 12, 0, 0, 0, time.UTC)
	require.NoError(t, p.Chtimes("/f", time.Time{}, want))

	info, err := p.Stat("/f")
	require.NoError(t, err)
	assert.True(t, info.ModTime().Equal(want))
}

func TestMemoryProviderChtimesMissingFile(t *testing.T) {
	p := NewMemoryProvider()
	err := p.Chtimes("/missing", time.Time{}, time.Now())
	assert.ErrorIs(t, err, syscall.ENOENT)
}

func TestReadonlyProviderChtimesRejected(t *testing.T) {
	p := NewReadonlyProvider(NewMemoryProvider())
	err := p.Chtimes("/f", time.Time{}, time.Now())
	assert.ErrorIs(t, err, syscall.EROFS)
}
