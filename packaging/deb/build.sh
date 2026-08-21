#!/bin/sh
# Build the Omahab .deb packages with plain dpkg-deb (no debhelper).
#
# Usage: build.sh [--dry-run] [--require-client] <arch> <out-dir>
#
#   --dry-run        stage and print the package trees without invoking
#                    dpkg-deb (also implied when dpkg-deb is unavailable)
#   --require-client fail instead of warn when the omahab-clientd binary is
#                    not staged yet
#   <arch>           amd64 or arm64
#   <out-dir>        directory that receives
#                    omahab_<version>_<arch>.deb and, when the companion
#                    binary is staged, omahab-client_<version>_<arch>.deb
#
# Inputs, all relative to the repository root:
#
#   packaging/omahab.files      explicit `source -> dest-dir/` mappings
#                               (no globs; directory sources copy contents
#                               recursively). Doc files should map into
#                               usr/share/doc/omahab/.
#   packaging/systemd/omahab-clientd.service
#                               user unit; always installed into the client
#                               package at usr/lib/systemd/user/
#   packaging/deb/changelog     version source of truth (first stanza)
#   packaging/deb/omahab.control, omahab-client.control
#                               control templates with @VERSION@/@ARCH@
#   packaging/deb/omahab.postinst, omahab.prerm, omahab.postrm
#                               maintainer scripts for the server package
#
# Package split rule: mappings whose source basename begins with
# `omahab-clientd` belong to the omahab-client package; everything else to
# the omahab (server) package. The client package is built only when an
# omahab-clientd binary is staged (a user unit without its daemon would be
# a broken package). Files staged under etc/ are registered as conffiles.
set -eu

usage() {
    echo "usage: build.sh [--dry-run] [--require-client] <arch> <out-dir>" >&2
    echo "  <arch>      amd64 or arm64" >&2
    echo "  <out-dir>   receives omahab_<version>_<arch>.deb and, when the" >&2
    echo "              companion binary is staged, omahab-client_<version>_<arch>.deb" >&2
    exit 2
}

die() {
    echo "packaging/deb/build.sh: error: $*" >&2
    exit 1
}

dry_run=0
require_client=0
while [ $# -gt 0 ]; do
    case $1 in
        --dry-run) dry_run=1 ;;
        --require-client) require_client=1 ;;
        -*) usage ;;
        *) break ;;
    esac
    shift
done
[ $# -eq 2 ] || usage

arch=$1
outdir=$2
case $arch in
    amd64 | arm64) ;;
    *) die "unsupported architecture '$arch' (expected amd64 or arm64)" ;;
esac

for tool in sed find sort xargs md5sum gzip chmod cp mkdir; do
    command -v "$tool" >/dev/null 2>&1 ||
        die "required tool '$tool' not found"
done

root=$(cd "$(dirname "$0")/../.." && pwd)
files_list=$root/packaging/omahab.files
changelog=$root/packaging/deb/changelog
clientd_unit=$root/packaging/systemd/omahab-clientd.service
[ -f "$files_list" ] || die "missing $files_list (release/packaging peer owns it)"
[ -f "$changelog" ] || die "missing $changelog"
[ -f "$clientd_unit" ] || die "missing $clientd_unit"

version=$(sed -n 's/^omahab (\([^)]*\)).*$/\1/p' "$changelog" | sed -n '1p')
[ -n "$version" ] || die "cannot parse a version from $changelog"

mkdir -p "$outdir" || die "cannot create output directory $outdir"
work=$(mktemp -d "${TMPDIR:-/tmp}/omahab-deb.XXXXXXXX") ||
    die "mktemp failed"
trap 'rm -rf -- "$work"' EXIT

server_data=$work/omahab/data
client_data=$work/omahab-client/data
mkdir -p "$server_data" "$client_data"

stage() {
    # stage <source> <dest-dir-within-data-root> <data-root>
    src=$1
    dst=$2
    data=$3
    case $dst in
        /*) die "destination '$dst' must be relative to the package root" ;;
    esac
    case $dst in
        */) ;;
        *) dst=$dst/ ;;
    esac
    case $src in
        *\** | *\?* | *\[*) die "glob mapping not supported; list files explicitly: $src" ;;
    esac
    [ -e "$src" ] || die "mapping source does not exist: $src"
    mkdir -p "$data/$dst"
    if [ -d "$src" ]; then
        cp -Rp "$src/." "$data/$dst"
    else
        cp -Rp "$src" "$data/$dst"
    fi
}

client_has_binary=0
while IFS= read -r line || [ -n "$line" ]; do
    case $line in
        '' | '#'*) continue ;;
    esac
    case $line in
        *'->'*) ;;
        *) die "malformed omahab.files line (expected 'source -> dest-dir/'): $line" ;;
    esac
    entry=${line%%->*}
    rest=${line#*->}
    src=$(printf '%s' "$entry" | sed "s/@ARCH@/$arch/g; s/[[:space:]]*$//; s/^[[:space:]]*//; s/\\/$//")
    dst=$(printf '%s' "$rest" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')
    [ -n "$src" ] && [ -n "$dst" ] || die "malformed omahab.files line: $line"
    src=$root/$src
    base=$(basename "$src")
    case $base in
        omahab-clientd*)
            stage "$src" "$dst" "$client_data"
            case $base in
                omahab-clientd) client_has_binary=1 ;;
            esac
            ;;
        *)
            stage "$src" "$dst" "$server_data"
            ;;
    esac
done <"$files_list"

# The companion user unit always belongs to the client package.
stage "$clientd_unit" usr/lib/systemd/user "$client_data"

# Doc trees: compressed changelog per package.
for pkg_data in "$server_data" "$client_data"; do
    pkg=$(basename "$(dirname "$pkg_data")")
    mkdir -p "$pkg_data/usr/share/doc/$pkg"
    gzip -n -9 -c "$changelog" >"$pkg_data/usr/share/doc/$pkg/changelog.gz"
done

finalize() {
    # finalize <pkg> <data-root> <control-template>
    pkg=$1
    data=$2
    template=$3
    [ -d "$data/usr" ] || die "package $pkg staged no content"
    debian=$work/$pkg/DEBIAN
    mkdir -p "$debian"
    sed -e "s/@VERSION@/$version/g" -e "s/@ARCH@/$arch/g" "$template" >"$debian/control"
    case $pkg in
        omahab)
            for script in postinst prerm postrm; do
                cp "$root/packaging/deb/omahab.$script" "$debian/$script"
                chmod 0755 "$debian/$script"
            done
            ;;
    esac
    # Everything under etc/ is a conffile.
    conffiles=$(cd "$data" && find etc -type f 2>/dev/null | LC_ALL=C sort | sed 's|^|/|')
    if [ -n "$conffiles" ]; then
        printf '%s\n' "$conffiles" >"$debian/conffiles"
    fi
    (cd "$data" && find . -type f -print | LC_ALL=C sort | sed 's|^\./||' | xargs -r md5sum) \
        >"$debian/md5sums"
    chmod 0755 "$debian"
}

build_client=1
if [ "$client_has_binary" -ne 1 ]; then
    if [ "$require_client" -eq 1 ]; then
        die "omahab-clientd binary is not staged (see packaging/omahab.files); refusing to build omahab-client"
    fi
    echo "packaging/deb/build.sh: warning: omahab-clientd binary not staged; skipping omahab-client package" >&2
    build_client=0
fi

show_tree() {
    echo "--- $1"
    (cd "$work/$1" && find . | LC_ALL=C sort | sed 's|^\./||')
}

finalize omahab "$server_data" "$root/packaging/deb/omahab.control"

deb_name=omahab_${version}_${arch}.deb
if [ "$dry_run" -eq 1 ] || ! command -v dpkg-deb >/dev/null 2>&1; then
    echo "packaging/deb/build.sh: dry run for $deb_name" >&2
    show_tree omahab
    if [ "$build_client" -eq 1 ]; then
        finalize omahab-client "$client_data" "$root/packaging/deb/omahab-client.control"
        show_tree omahab-client
        echo "packaging/deb/build.sh: dry run for omahab-client_${version}_${arch}.deb (not built)" >&2
    fi
    exit 0
fi

dpkg-deb --root-owner-group --build "$work/omahab" "$outdir/$deb_name" ||
    die "dpkg-deb failed for $deb_name"
echo "$outdir/$deb_name"

if [ "$build_client" -eq 1 ]; then
    finalize omahab-client "$client_data" "$root/packaging/deb/omahab-client.control"
    client_deb=omahab-client_${version}_${arch}.deb
    dpkg-deb --root-owner-group --build "$work/omahab-client" "$outdir/$client_deb" ||
        die "dpkg-deb failed for $client_deb"
    echo "$outdir/$client_deb"
fi
