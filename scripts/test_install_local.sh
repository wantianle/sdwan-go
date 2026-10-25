#!/usr/bin/env bash
# Tests for the local-artifact install path of scripts/install.sh.
#
# Runs without root and without network: it sources install.sh (the main() guard
# keeps the installer from executing), points INSTALL_DIR at a temp directory and
# drives install_from_local() in a subshell per case.
#
# Usage: bash scripts/test_install_local.sh
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=install.sh
# shellcheck disable=SC1091
source "$SCRIPT_DIR/install.sh"

WORK=$(mktemp -d "${TMPDIR:-/tmp}/sdwan-local-install.XXXXXX")
trap 'rm -rf "$WORK"' EXIT

OUT=""
STATUS=0

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

ok() {
    echo "ok - $*"
}

hash_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

# write_manifest <dir> <name-prefix> <name>...
# Emits GNU text-mode records ("hash" + two spaces + name), the format the
# release pipeline produces with `sha256sum dist/*`.
write_manifest() {
    local dir="$1" prefix="$2"
    shift 2
    : > "$dir/SHA256SUMS"
    local name
    for name in "$@"; do
        printf '%s  %s%s\n' "$(hash_of "$dir/$name")" "$prefix" "$name" >> "$dir/SHA256SUMS"
    done
}

# run_local <target-dir> <local-dir> <os> <arch> <binary>
# Sets OUT (combined stdout/stderr) and STATUS from install_from_local().
# The function is executed in a command-substitution subshell so its `exit`s and
# its reliance on installer globals cannot leak into this script.
# run_local <target-dir> <local-dir> <os> <arch> <binary> [keep]
# Sets OUT (combined stdout/stderr) and STATUS from install_from_local().
# With keep=1 the target directory is preserved (to prove a failed install does
# not disturb an already installed binary). The function is executed in a
# command-substitution subshell so its `exit`s and its reliance on installer
# globals cannot leak into this script.
run_local() {
    local target="$1" local_dir="$2" os="$3" arch="$4" binary="$5" keep="${6:-0}"
    if [[ "$keep" != "1" ]]; then
        rm -rf "$target"
    fi
    mkdir -p "$target"
    set +e
    OUT=$(LOCAL_DIR="$local_dir" INSTALL_DIR="$target" OS="$os" ARCH="$arch" BINARY="$binary" \
        install_from_local 2>&1)
    STATUS=$?
    set -e
}

# ── fixtures ────────────────────────────────────────────────
DIST="$WORK/dist"
mkdir -p "$DIST"
printf 'linux-amd64 payload\n' > "$DIST/sdwan-linux-amd64"
printf 'macos-amd64 payload\n' > "$DIST/sdwan-macos-amd64"
chmod 755 "$DIST/sdwan-linux-amd64" "$DIST/sdwan-macos-amd64"

LINUX_HASH=$(hash_of "$DIST/sdwan-linux-amd64")

# ── (a) linux/amd64 installs from a local directory with a manifest ──
write_manifest "$DIST" "" "sdwan-linux-amd64" "sdwan-macos-amd64"
run_local "$WORK/install-linux" "$DIST" linux amd64 sdwan-linux-amd64
[[ $STATUS -eq 0 ]] || fail "(a) linux/amd64 local install failed: $OUT"
[[ -f "$WORK/install-linux/sdwan" ]] || fail "(a) no binary was installed"
cmp -s "$DIST/sdwan-linux-amd64" "$WORK/install-linux/sdwan" ||
    fail "(a) installed content is not sdwan-linux-amd64"
[[ -x "$WORK/install-linux/sdwan" ]] || fail "(a) installed binary is not executable"
[[ "$OUT" == *"校验通过"* ]] || fail "(a) manifest verification was not reported"
[[ "$OUT" == *"[local]"* && "$OUT" == *"→ $WORK/install-linux/sdwan"* ]] ||
    fail "(a) missing [local] line with source and target: $OUT"
[[ "$OUT" != *"跳过完整性校验"* ]] || fail "(a) verification was skipped despite a usable manifest"
ok "(a) linux/amd64 artifact selected and manifest-verified"

# ── (a') manifest written with the release pipeline's "dist/" prefix ──
write_manifest "$DIST" "dist/" "sdwan-linux-amd64" "sdwan-macos-amd64"
run_local "$WORK/install-linux-distprefix" "$DIST" linux amd64 sdwan-linux-amd64
[[ $STATUS -eq 0 ]] || fail "(a') dist/-prefixed manifest was not accepted: $OUT"
[[ "$OUT" == *"校验通过"* ]] || fail "(a') dist/-prefixed manifest was not verified"
ok "(a') dist/-prefixed manifest records are honored"

# ── (a'') darwin falls back to the dist/ "macos" spelling ──
# No manifest here: the darwin hash command (shasum) must not make the selection
# test host-dependent. Hash verification is covered by the linux cases.
DIST_MACOS="$WORK/dist-macos"
mkdir -p "$DIST_MACOS"
printf 'macos-amd64 payload\n' > "$DIST_MACOS/sdwan-macos-amd64"
run_local "$WORK/install-darwin" "$DIST_MACOS" darwin amd64 sdwan-darwin-amd64
[[ $STATUS -eq 0 ]] || fail "(a'') darwin macos-spelling fallback failed: $OUT"
[[ -f "$WORK/install-darwin/sdwan" ]] || fail "(a'') darwin fallback installed nothing"
cmp -s "$DIST_MACOS/sdwan-macos-amd64" "$WORK/install-darwin/sdwan" ||
    fail "(a'') darwin fallback did not install sdwan-macos-amd64"
ok "(a'') darwin resolves sdwan-macos-\$ARCH"

# ── (b) hash mismatch aborts and installs nothing ──
DIST_BAD="$WORK/dist-bad"
mkdir -p "$DIST_BAD"
printf 'linux-amd64 payload\n' > "$DIST_BAD/sdwan-linux-amd64"
ZEROS=$(printf '%064d' 0)
printf '%s  %s\n' "$ZEROS" "sdwan-linux-amd64" > "$DIST_BAD/SHA256SUMS"
run_local "$WORK/install-bad" "$DIST_BAD" linux amd64 sdwan-linux-amd64
[[ $STATUS -ne 0 ]] || fail "(b) hash mismatch did not abort (status $STATUS): $OUT"
[[ "$OUT" == *"校验和不匹配"* ]] || fail "(b) mismatch was not reported: $OUT"
[[ "$OUT" == *"$ZEROS"* ]] || fail "(b) expected hash was not printed: $OUT"
[[ "$OUT" == *"$(hash_of "$DIST_BAD/sdwan-linux-amd64")"* ]] ||
    fail "(b) actual hash was not printed: $OUT"
[[ ! -e "$WORK/install-bad/sdwan" ]] || fail "(b) a mismatching artifact was installed"
compgen -G "$WORK/install-bad/.sdwan.*" >/dev/null && fail "(b) temp file left behind"
ok "(b) hash mismatch aborts with both hashes"

# assert_not_installed <target-dir> <label>: a failed run must leave no binary
# and no temp file in the install directory.
assert_not_installed() {
    local target="$1" label="$2"
    [[ ! -e "$target/sdwan" ]] || fail "$label: a binary was installed despite the failure"
    if compgen -G "$target/.sdwan.*" >/dev/null; then
        fail "$label: a temp file was left behind"
    fi
}

# ── (b2) manifest exists but has no record for the selected artifact ──
DIST_NOREC="$WORK/dist-norec"
mkdir -p "$DIST_NOREC"
printf 'linux-amd64 payload\n' > "$DIST_NOREC/sdwan-linux-amd64"
printf '%s  %s\n' "$(hash_of "$DIST_NOREC/sdwan-linux-amd64")" "sdwan-macos-amd64" > "$DIST_NOREC/SHA256SUMS"
run_local "$WORK/install-norec" "$DIST_NOREC" linux amd64 sdwan-linux-amd64
[[ $STATUS -ne 0 ]] || fail "(b2) a manifest without the selected artifact must abort (status $STATUS): $OUT"
[[ "$OUT" == *"找不到 sdwan-linux-amd64 的校验和记录"* ]] ||
    fail "(b2) missing-record error does not name the artifact: $OUT"
assert_not_installed "$WORK/install-norec" "(b2)"
ok "(b2) manifest without the selected artifact aborts"

# ── (b3) duplicate records for the selected artifact ──
DIST_DUP="$WORK/dist-dup"
mkdir -p "$DIST_DUP"
printf 'linux-amd64 payload\n' > "$DIST_DUP/sdwan-linux-amd64"
DUP_HASH=$(hash_of "$DIST_DUP/sdwan-linux-amd64")
printf '%s  %s\n%s *%s\n' "$DUP_HASH" "sdwan-linux-amd64" "$DUP_HASH" "sdwan-linux-amd64" > "$DIST_DUP/SHA256SUMS"
run_local "$WORK/install-dup" "$DIST_DUP" linux amd64 sdwan-linux-amd64
[[ $STATUS -ne 0 ]] || fail "(b3) duplicate records must abort (status $STATUS): $OUT"
[[ "$OUT" == *"2 条记录"* ]] || fail "(b3) duplicate-record error is not explicit: $OUT"
assert_not_installed "$WORK/install-dup" "(b3)"
ok "(b3) duplicate records abort"

# ── (b4) malformed hash record for the selected artifact ──
DIST_MALFORMED="$WORK/dist-malformed"
mkdir -p "$DIST_MALFORMED"
printf 'linux-amd64 payload\n' > "$DIST_MALFORMED/sdwan-linux-amd64"
printf '%s  %s\n' "not-a-sha256" "sdwan-linux-amd64" > "$DIST_MALFORMED/SHA256SUMS"
run_local "$WORK/install-malformed" "$DIST_MALFORMED" linux amd64 sdwan-linux-amd64
[[ $STATUS -ne 0 ]] || fail "(b4) a malformed hash record must abort (status $STATUS): $OUT"
[[ "$OUT" == *"校验和格式无效"* && "$OUT" == *"sdwan-linux-amd64"* ]] ||
    fail "(b4) malformed-record error is not explicit: $OUT"
assert_not_installed "$WORK/install-malformed" "(b4)"
ok "(b4) malformed hash record aborts"

# ── (b5) malformed record appended after a usable one is still fatal ──
DIST_MALFORMED2="$WORK/dist-malformed2"
mkdir -p "$DIST_MALFORMED2"
printf 'linux-amd64 payload\n' > "$DIST_MALFORMED2/sdwan-linux-amd64"
printf '%s  %s\n%s  %s\n' "$(hash_of "$DIST_MALFORMED2/sdwan-linux-amd64")" "sdwan-linux-amd64" "zz" "sdwan-linux-amd64" > "$DIST_MALFORMED2/SHA256SUMS"
run_local "$WORK/install-malformed2" "$DIST_MALFORMED2" linux amd64 sdwan-linux-amd64
[[ $STATUS -ne 0 ]] || fail "(b5) a malformed second record must abort (status $STATUS): $OUT"
assert_not_installed "$WORK/install-malformed2" "(b5)"
ok "(b5) a malformed record cannot hide behind a valid one"

# ── (b8) a trailing extra token makes the record malformed (strict whole-record) ──
DIST_EXTRA="$WORK/dist-extra"
mkdir -p "$DIST_EXTRA"
printf 'linux-amd64 payload\n' > "$DIST_EXTRA/sdwan-linux-amd64"
printf '%s  %s extra-token\n' "$(hash_of "$DIST_EXTRA/sdwan-linux-amd64")" "sdwan-linux-amd64" > "$DIST_EXTRA/SHA256SUMS"
run_local "$WORK/install-extra" "$DIST_EXTRA" linux amd64 sdwan-linux-amd64
[[ $STATUS -ne 0 ]] || fail "(b8) a record with a trailing token must abort (status $STATUS): $OUT"
[[ "$OUT" == *"校验和格式无效"* && "$OUT" == *"sdwan-linux-amd64"* ]] ||
    fail "(b8) trailing-token error does not name the artifact: $OUT"
assert_not_installed "$WORK/install-extra" "(b8)"
ok "(b8) a trailing extra token is rejected"

# ── (b9) a record with reversed fields is malformed ──
DIST_ORDER="$WORK/dist-order"
mkdir -p "$DIST_ORDER"
printf 'linux-amd64 payload\n' > "$DIST_ORDER/sdwan-linux-amd64"
printf '%s  %s\n' "sdwan-linux-amd64" "$(hash_of "$DIST_ORDER/sdwan-linux-amd64")" > "$DIST_ORDER/SHA256SUMS"
run_local "$WORK/install-order" "$DIST_ORDER" linux amd64 sdwan-linux-amd64
[[ $STATUS -ne 0 ]] || fail "(b9) a reversed record must abort (status $STATUS): $OUT"
assert_not_installed "$WORK/install-order" "(b9)"
ok "(b9) a reversed record is rejected"

# ── (a2) a CRLF manifest with one correct record verifies and installs ──
DIST_CRLF="$WORK/dist-crlf"
mkdir -p "$DIST_CRLF"
printf 'linux-amd64 payload\n' > "$DIST_CRLF/sdwan-linux-amd64"
CRLF_HASH=$(hash_of "$DIST_CRLF/sdwan-linux-amd64")
printf '%s  %s\r\n' "$CRLF_HASH" "sdwan-linux-amd64" > "$DIST_CRLF/SHA256SUMS"
run_local "$WORK/install-crlf" "$DIST_CRLF" linux amd64 sdwan-linux-amd64
[[ $STATUS -eq 0 ]] || fail "(a2) a CRLF manifest must verify and install: $OUT"
[[ "$OUT" == *"校验通过"* ]] || fail "(a2) CRLF record was not verified: $OUT"
cmp -s "$DIST_CRLF/sdwan-linux-amd64" "$WORK/install-crlf/sdwan" ||
    fail "(a2) CRLF install produced the wrong content"
ok "(a2) a CRLF manifest record verifies and installs"

# ── (b10) a CRLF duplicate of the selected artifact must still be a duplicate ──
DIST_CRLF_DUP="$WORK/dist-crlf-dup"
mkdir -p "$DIST_CRLF_DUP"
printf 'linux-amd64 payload\n' > "$DIST_CRLF_DUP/sdwan-linux-amd64"
CRLF_DUP_HASH=$(hash_of "$DIST_CRLF_DUP/sdwan-linux-amd64")
printf '%s  %s\r\n%s *%s\r\n' \
    "$CRLF_DUP_HASH" "sdwan-linux-amd64" "$CRLF_DUP_HASH" "sdwan-linux-amd64" > "$DIST_CRLF_DUP/SHA256SUMS"
run_local "$WORK/install-crlf-dup" "$DIST_CRLF_DUP" linux amd64 sdwan-linux-amd64
[[ $STATUS -ne 0 ]] || fail "(b10) a CRLF duplicate must abort (status $STATUS): $OUT"
[[ "$OUT" == *"2 条记录"* ]] || fail "(b10) CRLF duplicate was not counted: $OUT"
assert_not_installed "$WORK/install-crlf-dup" "(b10)"
ok "(b10) a CRLF duplicate is still a duplicate"

# ── (a3) all four accepted name forms verify against the same artifact ──
DIST_FORMS="$WORK/dist-forms"
mkdir -p "$DIST_FORMS"
printf 'linux-amd64 payload\n' > "$DIST_FORMS/sdwan-linux-amd64"
FORMS_HASH=$(hash_of "$DIST_FORMS/sdwan-linux-amd64")
for FORM in "sdwan-linux-amd64" "*sdwan-linux-amd64" "dist/sdwan-linux-amd64" "*dist/sdwan-linux-amd64"; do
    printf '%s  %s\n' "$FORMS_HASH" "$FORM" > "$DIST_FORMS/SHA256SUMS"
    run_local "$WORK/install-form" "$DIST_FORMS" linux amd64 sdwan-linux-amd64
    [[ $STATUS -eq 0 ]] || fail "(a3) name form '$FORM' was rejected: $OUT"
    [[ "$OUT" == *"校验通过"* ]] || fail "(a3) name form '$FORM' was not verified: $OUT"
    cmp -s "$DIST_FORMS/sdwan-linux-amd64" "$WORK/install-form/sdwan" ||
        fail "(a3) name form '$FORM' installed the wrong content"
done
ok "(a3) all four accepted name forms verify"

# ── (b6) a failed local install leaves the existing binary untouched ──
PRE="$WORK/install-preexisting"
mkdir -p "$PRE"
printf 'old installed binary\n' > "$PRE/sdwan"
printf 'old installed binary\n' > "$WORK/old-sdwan.expected"
run_local "$PRE" "$DIST_MALFORMED" linux amd64 sdwan-linux-amd64 1
[[ $STATUS -ne 0 ]] || fail "(b6) malformed manifest must abort over an existing install: $OUT"
cmp -s "$WORK/old-sdwan.expected" "$PRE/sdwan" ||
    fail "(b6) a failed local install modified the existing binary"
ok "(b6) a failed local install leaves the existing binary untouched"

# ── (b7) a hash mismatch also leaves the existing binary untouched ──
PRE2="$WORK/install-preexisting-mismatch"
mkdir -p "$PRE2"
printf 'old installed binary\n' > "$PRE2/sdwan"
run_local "$PRE2" "$DIST_BAD" linux amd64 sdwan-linux-amd64 1
[[ $STATUS -ne 0 ]] || fail "(b7) hash mismatch must abort over an existing install: $OUT"
cmp -s "$WORK/old-sdwan.expected" "$PRE2/sdwan" ||
    fail "(b7) a hash mismatch overwrote the existing binary"
if compgen -G "$PRE2/.sdwan.*" >/dev/null; then
    fail "(b7) a temp file was left behind"
fi
ok "(b7) a hash mismatch leaves the existing binary untouched"

# ── (c) missing artifact aborts and names the expected files ──
DIST_EMPTY="$WORK/dist-empty"
mkdir -p "$DIST_EMPTY"
printf 'not an artifact\n' > "$DIST_EMPTY/unrelated.txt"
run_local "$WORK/install-missing" "$DIST_EMPTY" linux amd64 sdwan-linux-amd64
[[ $STATUS -ne 0 ]] || fail "(c) missing artifact did not abort (status $STATUS): $OUT"
[[ "$OUT" == *"sdwan-linux-amd64"* ]] || fail "(c) expected artifact name not listed: $OUT"
[[ ! -e "$WORK/install-missing/sdwan" ]] || fail "(c) missing artifact still installed something"
run_local "$WORK/install-missing-darwin" "$DIST_EMPTY" darwin amd64 sdwan-darwin-amd64
[[ $STATUS -ne 0 ]] || fail "(c) missing darwin artifact did not abort"
[[ "$OUT" == *"sdwan-macos-amd64"* ]] || fail "(c) darwin fallback name not listed: $OUT"
ok "(c) missing artifact aborts and lists expected names"

# ── (c2) symlinked local source is refused ──
DIST_LINK="$WORK/dist-link"
mkdir -p "$DIST_LINK"
printf 'linux-amd64 payload\n' > "$DIST_LINK/real-payload"
ln -s "$DIST_LINK/real-payload" "$DIST_LINK/sdwan-linux-amd64"
run_local "$WORK/install-link" "$DIST_LINK" linux amd64 sdwan-linux-amd64
[[ $STATUS -ne 0 ]] || fail "(c2) a symlinked source must be refused (status $STATUS): $OUT"
[[ "$OUT" == *"符号链接"* ]] || fail "(c2) symlink refusal is not explicit: $OUT"
assert_not_installed "$WORK/install-link" "(c2)"
ok "(c2) symlinked source is refused"

# ── (c3) a symlink cannot shadow the preferred candidate name: the real
# dist-named artifact still installs (darwin: sdwan-darwin-* symlinked) ──
DIST_LINK2="$WORK/dist-link2"
mkdir -p "$DIST_LINK2"
printf 'macos-amd64 payload\n' > "$DIST_LINK2/sdwan-macos-amd64"
ln -s "$DIST_LINK2/sdwan-macos-amd64" "$DIST_LINK2/sdwan-darwin-amd64"
run_local "$WORK/install-link2" "$DIST_LINK2" darwin amd64 sdwan-darwin-amd64
[[ $STATUS -eq 0 ]] || fail "(c3) the real fallback artifact must still install: $OUT"
cmp -s "$DIST_LINK2/sdwan-macos-amd64" "$WORK/install-link2/sdwan" ||
    fail "(c3) the wrong artifact was installed"
ok "(c3) a real artifact still installs when a symlink shadows the preferred name"

# ── (d) no manifest: install proceeds with exactly one notice ──
DIST_PLAIN="$WORK/dist-plain"
mkdir -p "$DIST_PLAIN"
printf 'linux-amd64 payload\n' > "$DIST_PLAIN/sdwan-linux-amd64"
run_local "$WORK/install-nomanifest" "$DIST_PLAIN" linux amd64 sdwan-linux-amd64
[[ $STATUS -eq 0 ]] || fail "(d) manifest-less local install failed: $OUT"
cmp -s "$DIST_PLAIN/sdwan-linux-amd64" "$WORK/install-nomanifest/sdwan" ||
    fail "(d) manifest-less install did not copy the artifact"
NOTICES=$(printf '%s\n' "$OUT" | grep -c '跳过完整性校验' || true)
[[ "$NOTICES" -eq 1 ]] || fail "(d) expected exactly one skip notice, got $NOTICES: $OUT"
[[ "$OUT" != *"📥 下载"* ]] || fail "(d) local install attempted a network download: $OUT"
ok "(d) manifest-less local install copies the artifact and prints one notice"

# ── environment override + sourceability invariants ──
[[ "$INSTALL_DIR" == "/usr/local/bin" ]] ||
    fail "INSTALL_DIR must still default to /usr/local/bin (got $INSTALL_DIR)"
[[ "$LINUX_HASH" =~ ^[0-9a-f]{64}$ ]] || fail "fixture hash helper is broken"
ok "INSTALL_DIR default preserved and install.sh is sourceable"

echo "All local-install tests passed."
