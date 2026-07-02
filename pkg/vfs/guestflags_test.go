package vfs

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHostOpenFlagsTranslatesLinuxBits(t *testing.T) {
	assert.Equal(t, os.O_WRONLY, hostOpenFlags(0x1|linuxOAppend),
		"O_APPEND must be dropped, not aliased onto a host flag")
	assert.Equal(t, os.O_RDWR|os.O_CREATE|os.O_EXCL, hostOpenFlags(0x2|linuxOCreat|linuxOExcl))
	assert.Equal(t, os.O_WRONLY|os.O_TRUNC, hostOpenFlags(0x1|linuxOTrunc))
	assert.Equal(t, os.O_RDONLY, hostOpenFlags(0x8000|0x800),
		"Linux-only bits (O_LARGEFILE, O_NONBLOCK) must not leak through")
}
