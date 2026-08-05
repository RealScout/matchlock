#!/bin/sh
# Fail the kernel build when olddefconfig silently drops a requested option.
#
# olddefconfig resolves unmet dependencies by dropping the option rather than
# erroring, so a config can build "successfully" without features the guest
# needs — this bit us with netfilter (Docker NAT) on 6.19.8. Comparing the
# requested config against the resolved .config is the only way to catch it.
#
# Usage: verify-config.sh <requested.config> <resolved .config> [known-drift.txt]
#
# known-drift.txt lists options already being dropped before this check
# existed. They are reported but do not fail the build, so pre-existing gaps
# stay visible without blocking every kernel build on a backlog.

set -eu

requested="$1"
resolved="$2"
drift="${3:-}"

known() {
    [ -n "$drift" ] && [ -f "$drift" ] || return 1
    grep -v '^#' "$drift" | grep -qxF "$1"
}

missing=""
tolerated=""
while IFS= read -r line; do
    grep -qxF "$line" "$resolved" && continue
    if known "${line%=y}"; then
        tolerated="${tolerated}  ${line}
"
    else
        missing="${missing}${line}
"
    fi
done <<EOF
$(grep -E '^CONFIG_[A-Za-z0-9_]+=y$' "$requested")
EOF

if [ -n "$tolerated" ]; then
    echo "verify-config: known pre-existing drift (see $drift):"
    printf '%s' "$tolerated"
fi

if [ -n "$missing" ]; then
    echo "ERROR: olddefconfig dropped options requested in $requested:" >&2
    printf '%s' "$missing" >&2
    echo "Check each option's Kconfig dependencies against this kernel version." >&2
    exit 1
fi

echo "verify-config: all requested =y options survived olddefconfig."
