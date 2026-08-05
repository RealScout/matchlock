#!/bin/sh
# Register qemu-x86_64 as the binfmt_misc handler for x86_64 ELF binaries.
# Must run in the VM's init user namespace (e.g. a privileged docker
# container) — matchlock execs get their own userns, whose binfmt instance
# evaporates with the exec. The F flag pins the interpreter at registration
# so callers never need /usr/bin/qemu-x86_64-static in their own mount ns.
set -e
mount -t binfmt_misc binfmt_misc /proc/sys/fs/binfmt_misc 2>/dev/null || true
if [ -e /proc/sys/fs/binfmt_misc/qemu-x86_64 ]; then
    echo "already registered:"
    head -3 /proc/sys/fs/binfmt_misc/qemu-x86_64
    exit 0
fi
printf ':qemu-x86_64:M::\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00\x3e\x00:\xff\xff\xff\xff\xff\xfe\xfe\x00\xff\xff\xff\xff\xff\xff\xff\xff\xfe\xff\xff\xff:/usr/bin/qemu-x86_64-static:F\n' > /proc/sys/fs/binfmt_misc/register
echo "registered:"
head -3 /proc/sys/fs/binfmt_misc/qemu-x86_64
