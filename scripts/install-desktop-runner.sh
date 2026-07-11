#!/usr/bin/env bash
set -euo pipefail

REPO_URL="${1:-}"
TOKEN="${2:-}"
RUNNER_NAME="${3:-$(hostname)}"
INSTALL_DIR="${4:-/opt/actions-runner}"

if [ -z "$REPO_URL" ] || [ -z "$TOKEN" ]; then
  echo "Usage: $0 <REPO_URL> <TOKEN> [RUNNER_NAME] [INSTALL_DIR]"
  exit 1
fi

echo "Creating installation directory at $INSTALL_DIR..."
sudo mkdir -p "$INSTALL_DIR"
sudo chown "$USER:$USER" "$INSTALL_DIR"
cd "$INSTALL_DIR"

echo "Downloading GitHub Actions Runner..."
curl -o actions-runner-linux-x64-2.317.0.tar.gz -L https://github.com/actions/runner/releases/download/v2.317.0/actions-runner-linux-x64-2.317.0.tar.gz

echo "Extracting Runner..."
tar xzf ./actions-runner-linux-x64-2.317.0.tar.gz
rm ./actions-runner-linux-x64-2.317.0.tar.gz

echo "Configuring Runner with UECB desktop labels..."
./config.sh --unattended --url "$REPO_URL" --token "$TOKEN" --name "$RUNNER_NAME" --labels "desktop-runner,linux,desktop" --replace

echo "Setting up auto-recovery (systemd service)..."
sudo ./svc.sh install
sudo ./svc.sh start

echo "Installation Complete! Runner is running as a systemd service."
