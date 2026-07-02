package vfs

import "os"

// The VFS wire protocol carries open(2) flags exactly as the guest kernel
// produced them. The guest is always Linux, so these are Linux ABI values.
// The host may not be Linux (macOS in particular), and the two ABIs assign
// different meanings to the same bits — e.g. Linux O_APPEND (0x400) is
// Darwin O_TRUNC — so raw guest bits must never reach a host open(2).
const (
	linuxOAccMode = 0x3
	linuxOCreat   = 0x40
	linuxOExcl    = 0x80
	linuxOTrunc   = 0x200
	linuxOAppend  = 0x400
)

// hostOpenFlags translates guest (Linux) open flags into the host's os.O_*
// values. Only flags meaningful to the VFS server survive:
//
//   - The access mode (O_RDONLY/O_WRONLY/O_RDWR) shares values on all
//     supported hosts and passes through.
//   - O_CREAT, O_EXCL and O_TRUNC are remapped to the host's bits.
//   - O_APPEND is dropped: every guest write arrives as an explicitly
//     positioned WriteAt, so append positioning is already handled by the
//     guest kernel, and a host-side O_APPEND would make WriteAt fail.
//   - All other bits (O_LARGEFILE, O_NOATIME, O_NONBLOCK, ...) are dropped
//     rather than left to alias onto unrelated host flags.
func hostOpenFlags(guestFlags uint32) int {
	flags := int(guestFlags & linuxOAccMode)
	if guestFlags&linuxOCreat != 0 {
		flags |= os.O_CREATE
	}
	if guestFlags&linuxOExcl != 0 {
		flags |= os.O_EXCL
	}
	if guestFlags&linuxOTrunc != 0 {
		flags |= os.O_TRUNC
	}
	return flags
}
