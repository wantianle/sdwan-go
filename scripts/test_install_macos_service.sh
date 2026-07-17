#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck disable=SC1091 # Path is resolved dynamically from this test's directory.
source "$script_dir/install.sh"

plist=$(mktemp)
trap 'rm -f "$plist"' EXIT
write_macos_plist "$plist"

program_arguments=$(awk '
    /<key>ProgramArguments<\/key>/ { in_arguments=1; next }
    in_arguments && /<\/array>/ { exit }
    in_arguments && /<string>/ { gsub(/^[[:space:]]*<string>|<\/string>[[:space:]]*$/, ""); print }
' "$plist")
expected_arguments=$'/usr/local/bin/sdwan\n-f\n/etc/sdwan/iwan.conf'
if [[ "$program_arguments" != "$expected_arguments" ]]; then
    printf 'unexpected ProgramArguments:\n%s\n' "$program_arguments" >&2
    exit 1
fi

require() {
    grep -Fq "$1" "$plist" || {
        printf 'missing plist content: %s\n' "$1" >&2
        exit 1
    }
}

require '<key>KeepAlive</key>'
require '<key>StandardErrorPath</key>'
require '<string>/var/log/sdwan.log</string>'

if grep -Fq '<key>StandardOutPath</key>' "$plist"; then
    printf 'StandardOutPath must not duplicate the stderr log\n' >&2
    exit 1
fi

if grep -Fq '<key>ThrottleInterval</key>' "$plist"; then
    printf 'ThrottleInterval must not be present for foreground service mode\n' >&2
    exit 1
fi
