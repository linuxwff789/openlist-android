#!/system/bin/sh
# One-time setup: create GitHub repo and push.
# Run this when you have network access (VPN off).
#
# Usage:  bash bridge/setup-repo.sh <repo-name>
# Example: bash bridge/setup-repo.sh openlist-android

set -e
NAME="${1:-openlist-android}"
USER="linuxwff789"

echo "==> Creating repo: $USER/$NAME"
gh repo create "$NAME" --public --description "OpenList Android JNI bridge: WebDAV/189Cloud/AliyunDrive" --push --source .

echo "==> Done. Repo: https://github.com/$USER/$NAME"
echo ""
echo "Next steps:"
echo "  1. Go to https://github.com/$USER/$NAME/actions"
echo "  2. Wait for build to finish"
echo "  3. Download artifacts: libopenlist-arm64 + openlist-cli-arm64"
echo ""
echo "In Termux:"
echo "  chmod +x openlist-cli.arm64"
echo '  ./openlist-cli.arm64 webdav '\''{"address":"...","username":"u","password":"p"}'\'' list /'
