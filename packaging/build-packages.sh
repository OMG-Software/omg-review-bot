#!/usr/bin/env bash
# Builds the Linux packages and binary tarballs from prebuilt binaries.
#
#   packaging/build-packages.sh VERSION RELEASE BIN_DIR OUT_DIR
#
# BIN_DIR holds prbot-linux-amd64 and prbot-linux-arm64. RELEASE is the package
# release number; the release workflow passes its run number, so each rolling
# build upgrades the one before. Needs nfpm on PATH (or NFPM=/path/to/nfpm);
# to build the version the release workflow uses:
#   go -C packaging/tools build -o "$PWD/nfpm" github.com/goreleaser/nfpm/v2/cmd/nfpm
#
# File names leave out the version so the rolling "latest" release keeps the
# same download URLs; the version is inside each package.
set -euo pipefail

if [[ $# -ne 4 ]]; then
    echo "usage: $0 VERSION RELEASE BIN_DIR OUT_DIR" >&2
    exit 2
fi
version=$1 release=$2
bin_dir=$(realpath "$3")
mkdir -p "$4"
out_dir=$(realpath "$4")
nfpm=${NFPM:-nfpm}

cd "$(dirname "$0")/.."

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# The CA bundle package gives the bot the trust store for GitHub's TLS
# certificates; openSUSE ships the bundle itself in ca-certificates-mozilla.
# target  packager  release suffix  CA bundle package
targets=(
    "rhel10   rpm       .el10  ca-certificates"
    "fedora44 rpm       .fc44  ca-certificates"
    "fedora45 rpm       .fc45  ca-certificates"
    "opensuse rpm       -      ca-certificates-mozilla"
    "debian   deb       -      ca-certificates"
    "ubuntu   deb       -      ca-certificates"
    "arch     archlinux -      ca-certificates"
)

# Architecture names as each package format spells them.
arch_name() {
    case "$1:$2" in
        deb:*) echo "$2" ;;
        *:amd64) echo x86_64 ;;
        *:arm64) echo aarch64 ;;
    esac
}

extension() {
    case "$1" in
        rpm) echo rpm ;;
        deb) echo deb ;;
        archlinux) echo pkg.tar.zst ;;
    esac
}

for goarch in amd64 arm64; do
    binary="$bin_dir/prbot-linux-$goarch"
    if [[ ! -x $binary ]]; then
        echo "missing or not executable: $binary" >&2
        exit 1
    fi

    for t in "${targets[@]}"; do
        read -r target packager suffix ca_package <<<"$t"
        [[ $suffix == - ]] && suffix=
        config="$work/nfpm-$target-$goarch.yaml"
        sed -e "s|@PRBOT_BINARY@|$binary|" -e "s|@CA_PACKAGE@|$ca_package|" packaging/nfpm.yaml >"$config"
        file="prbot-$target-$(arch_name "$packager" "$goarch").$(extension "$packager")"
        echo "building $file"
        PKG_ARCH=$goarch PKG_VERSION=$version PKG_RELEASE="$release$suffix" \
            "$nfpm" package --config "$config" --packager "$packager" --target "$out_dir/$file"
    done

    # Plain binary for other distributions and manual installs under /opt.
    dir="$work/prbot-$version-linux-$goarch"
    mkdir -p "$dir/deploy"
    install -m 0755 "$binary" "$dir/prbot"
    install -m 0644 README.md LICENSE "$dir/"
    install -m 0644 deploy/prbot.service "$dir/deploy/"
    tar -C "$work" --owner=0 --group=0 -czf "$out_dir/prbot-linux-$goarch.tar.gz" "$(basename "$dir")"
done

(cd "$out_dir" && sha256sum -- *.rpm *.deb *.pkg.tar.zst *.tar.gz >SHA256SUMS)
echo "packages in $out_dir:"
ls -l "$out_dir"
