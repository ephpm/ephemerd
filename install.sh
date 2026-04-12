#!/bin/sh
# Install ephemerd — ephemeral GitHub Actions runner daemon.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/ephpm/ephemerd/main/install.sh | bash
#
# Options (env vars):
#   EPHEMERD_VERSION    specific version to install (default: latest)
#   EPHEMERD_INSTALL_DIR  install directory (default: /usr/local/bin)
#   EPHEMERD_NO_SERVICE   skip service installation if set to 1

set -e

REPO="ephpm/ephemerd"
INSTALL_DIR="${EPHEMERD_INSTALL_DIR:-/usr/local/bin}"
DATA_DIR="/var/lib/ephemerd"
CONFIG_FILE="$DATA_DIR/config.toml"

# --- Helper functions ---

info() { printf "  \033[32m=>\033[0m %s\n" "$1"; }
warn() { printf "  \033[33m=>\033[0m %s\n" "$1"; }
fail() { printf "  \033[31m=>\033[0m %s\n" "$1"; exit 1; }

need_cmd() {
    if ! command -v "$1" > /dev/null 2>&1; then
        fail "required command not found: $1"
    fi
}

# --- Detect platform ---

detect_os() {
    case "$(uname -s)" in
        Linux*)  echo "linux" ;;
        Darwin*) echo "darwin" ;;
        MINGW*|MSYS*|CYGWIN*) echo "windows" ;;
        *) fail "unsupported operating system: $(uname -s)" ;;
    esac
}

detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64) echo "amd64" ;;
        aarch64|arm64) echo "arm64" ;;
        *) fail "unsupported architecture: $(uname -m)" ;;
    esac
}

# --- Get latest version ---

latest_version() {
    need_cmd curl
    curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
        | grep '"tag_name"' \
        | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/'
}

# --- Download and install ---

install_binary() {
    local os="$1" arch="$2" version="$3"

    local ext="tar.gz"
    if [ "$os" = "windows" ]; then
        ext="zip"
    fi

    local filename="ephemerd_${version#v}_${os}_${arch}.${ext}"
    local url="https://github.com/$REPO/releases/download/${version}/${filename}"

    local tmpdir
    tmpdir="$(mktemp -d)"
    trap 'rm -rf "$tmpdir"' EXIT

    info "downloading ephemerd $version for $os/$arch..."
    curl -fsSL -o "$tmpdir/$filename" "$url" || fail "download failed: $url"

    info "extracting..."
    if [ "$ext" = "tar.gz" ]; then
        tar -xzf "$tmpdir/$filename" -C "$tmpdir"
    else
        need_cmd unzip
        unzip -q "$tmpdir/$filename" -d "$tmpdir"
    fi

    local binary="ephemerd"
    if [ "$os" = "windows" ]; then
        binary="ephemerd.exe"
    fi

    info "installing to $INSTALL_DIR/$binary..."
    mkdir -p "$INSTALL_DIR"
    mv "$tmpdir/$binary" "$INSTALL_DIR/$binary"
    chmod +x "$INSTALL_DIR/$binary"
}

# --- Create default config ---

create_config() {
    if [ -f "$CONFIG_FILE" ]; then
        info "config file already exists: $CONFIG_FILE"
        return
    fi

    mkdir -p "$DATA_DIR"
    cat > "$CONFIG_FILE" << 'TOML'
[github]
owner = "your-org"
# repos = ["repo1", "repo2"]  # optional — omit for org-level runners

[runner]
max_concurrent = 4

[log]
level = "info"
TOML

    info "created default config: $CONFIG_FILE"
}

# --- Install systemd service (Linux) ---

install_systemd() {
    if [ ! -d /etc/systemd/system ]; then
        warn "systemd not found, skipping service installation"
        return
    fi

    cat > /etc/systemd/system/ephemerd.service << EOF
[Unit]
Description=ephemerd - Ephemeral GitHub Actions Runner Daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$INSTALL_DIR/ephemerd serve --data-dir $DATA_DIR
Restart=on-failure
RestartSec=5
EnvironmentFile=-/etc/default/ephemerd
KillMode=mixed
TimeoutStopSec=300

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    info "systemd service installed: ephemerd.service"

    # Create env file for GITHUB_TOKEN
    if [ ! -f /etc/default/ephemerd ]; then
        cat > /etc/default/ephemerd << 'ENV'
# Set your GitHub token here
# GITHUB_TOKEN=ghp_your_token_here
ENV
        info "created env file: /etc/default/ephemerd (edit to set GITHUB_TOKEN)"
    fi
}

# --- Install launchd plist (macOS) ---

install_launchd() {
    local plist="/Library/LaunchDaemons/dev.ephpm.ephemerd.plist"

    cat > "$plist" << EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>dev.ephpm.ephemerd</string>
    <key>ProgramArguments</key>
    <array>
        <string>$INSTALL_DIR/ephemerd</string>
        <string>serve</string>
        <string>--data-dir</string>
        <string>$DATA_DIR</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>/var/log/ephemerd.log</string>
    <key>StandardErrorPath</key>
    <string>/var/log/ephemerd.log</string>
</dict>
</plist>
EOF

    info "launchd service installed: $plist"
    warn "set GITHUB_TOKEN in the plist EnvironmentVariables or /etc/default/ephemerd before starting"
}

# --- Main ---

main() {
    printf "\n  ephemerd installer\n\n"

    need_cmd curl
    need_cmd tar

    local os arch version
    os="$(detect_os)"
    arch="$(detect_arch)"
    version="${EPHEMERD_VERSION:-$(latest_version)}"

    if [ -z "$version" ]; then
        fail "could not determine latest version (set EPHEMERD_VERSION to install a specific version)"
    fi

    info "version: $version"
    info "platform: $os/$arch"

    install_binary "$os" "$arch" "$version"
    create_config

    if [ "${EPHEMERD_NO_SERVICE:-0}" != "1" ]; then
        case "$os" in
            linux)  install_systemd ;;
            darwin) install_launchd ;;
            windows) warn "Windows service installation not supported by this script — use 'sc.exe create' or NSSM" ;;
        esac
    fi

    printf "\n  \033[32mInstalled!\033[0m\n\n"
    info "binary:  $INSTALL_DIR/ephemerd"
    info "config:  $CONFIG_FILE"
    info ""
    info "Next steps:"
    info "  1. Edit $CONFIG_FILE (set github.owner)"
    info "  2. Set GITHUB_TOKEN in /etc/default/ephemerd"
    info "  3. sudo systemctl start ephemerd"
    info "  4. sudo systemctl enable ephemerd"
    info ""
    info "Or run manually: GITHUB_TOKEN=ghp_... sudo -E ephemerd serve"
    printf "\n"
}

main "$@"
