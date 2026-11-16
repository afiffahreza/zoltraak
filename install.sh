#!/bin/sh
set -eu

fail() {
    printf 'zoltraak: %s\n' "$*" >&2
    exit 1
}

main() {
    for cmd in curl uname mktemp tar awk install mkdir; do
        command -v "$cmd" >/dev/null 2>&1 || fail "required command not found: $cmd"
    done
    if command -v sha256sum >/dev/null 2>&1; then
        checksum=sha256sum
    elif command -v shasum >/dev/null 2>&1; then
        checksum=shasum
    else
        fail 'sha256sum or shasum is required'
    fi

    case "$(uname -s)" in
        Linux) os=linux ;;
        Darwin) os=darwin ;;
        *) fail 'supported operating systems: Linux and macOS' ;;
    esac
    case "$(uname -m)" in
        x86_64|amd64) arch=amd64 ;;
        aarch64|arm64) arch=arm64 ;;
        *) fail 'supported architectures: amd64 and arm64' ;;
    esac

    releases=https://github.com/afiffahreza/zoltraak/releases
    version=${ZOLTRAAK_VERSION:-latest}
    install_dir=${INSTALL_DIR:-"$HOME/.local/bin"}
    if [ "$version" = latest ]; then
        release_url=$(curl --proto '=https' --tlsv1.2 -fsSLI -o /dev/null \
            -w '%{url_effective}' "$releases/latest")
        case "$release_url" in
            "$releases"/tag/*) version=${release_url##*/} ;;
            *) fail 'could not resolve the latest release' ;;
        esac
    fi
    case "$version" in
        ''|*[!A-Za-z0-9._-]*) fail 'invalid release tag' ;;
    esac

    asset="zoltraak_${os}_${arch}.tar.gz"
    tmp_dir=$(mktemp -d)
    trap 'rm -rf "$tmp_dir"' 0
    trap 'exit 1' 1 2 15
    url="$releases/download/$version"
    printf 'Downloading zoltraak %s (%s/%s)\n' "$version" "$os" "$arch"
    curl --proto '=https' --tlsv1.2 -fsSL "$url/$asset" -o "$tmp_dir/$asset"
    curl --proto '=https' --tlsv1.2 -fsSL "$url/SHA256SUMS" -o "$tmp_dir/SHA256SUMS"
    awk -v file="$asset" '
        $2 == file || $2 == "./" file { print; found++ }
        END { if (found != 1) exit 1 }
    ' "$tmp_dir/SHA256SUMS" > "$tmp_dir/checksum" || fail "missing or duplicate checksum for $asset"
    (
        cd "$tmp_dir"
        if [ "$checksum" = sha256sum ]; then
            sha256sum -c checksum
        else
            shasum -a 256 -c checksum
        fi
    ) || fail 'checksum verification failed; nothing installed'

    tar -xzf "$tmp_dir/$asset" -C "$tmp_dir" zoltraak
    mkdir -p "$install_dir"
    install -m 755 "$tmp_dir/zoltraak" "$install_dir/zoltraak"
    printf 'Installed %s/zoltraak (%s)\n' "$install_dir" "$version"
    case ":${PATH:-}:" in
        *:"$install_dir":*) ;;
        *) printf 'Add %s to PATH in your shell profile.\n' "$install_dir" ;;
    esac
}

main "$@"
