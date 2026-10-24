#!/bin/bash
# setup.sh - install or update skycam on the observatory Pi.
# Idempotent: the first run provisions the Pi, later runs just update.
# Run by deploy-pi.ps1 as: sudo bash /tmp/skycam-bundle/setup.sh /tmp/skycam-bundle
set -e

BUNDLE="${1:-$(dirname "$0")}"
SDK_VER="1.41"

echo "=== skycam setup ==="

# 1. Packages: ffmpeg for the timelapse, libusb for the ZWO SDK.
NEED=""
dpkg -s ffmpeg >/dev/null 2>&1 || NEED="$NEED ffmpeg"
dpkg -s libusb-1.0-0 >/dev/null 2>&1 || NEED="$NEED libusb-1.0-0"
if [ -n "$NEED" ]; then
    echo "[1/7] installing$NEED"
    apt-get update -qq
    apt-get install -y -qq $NEED
else
    echo "[1/7] packages present"
fi
systemctl enable --quiet systemd-time-wait-sync 2>/dev/null || true

# 2. ZWO SDK. The Windows download lost the .so symlink, so make it here.
echo "[2/7] ZWO SDK $SDK_VER"
install -m 644 "$BUNDLE/libASICamera2.so.$SDK_VER" /usr/local/lib/
ln -sf "libASICamera2.so.$SDK_VER" /usr/local/lib/libASICamera2.so
ldconfig

# 3. udev: let the service user open the camera, and give usbfs the 200 MB
# of buffer the SDK expects.
echo "[3/7] udev rule"
install -m 644 "$BUNDLE/asi.rules" /etc/udev/rules.d/99-asi.rules
udevadm control --reload-rules
udevadm trigger --subsystem-match=usb --attr-match=idVendor=03c3 || true
echo 200 > /sys/module/usbcore/parameters/usbfs_memory_mb

# 4. Service user and folders.
echo "[4/7] user and folders"
id skycam >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin skycam
for g in video plugdev; do getent group $g >/dev/null && usermod -aG $g skycam; done
install -d -o skycam -g skycam -m 755 /var/lib/skycam
install -d -o root -g skycam -m 750 /etc/skycam
if [ ! -f /etc/skycam/config.json ]; then
    install -o root -g skycam -m 640 "$BUNDLE/config.example.json" /etc/skycam/config.json
    echo "      created /etc/skycam/config.json from the example - edit it"
fi

# 5. Binary.
echo "[5/7] binary"
install -m 755 "$BUNDLE/skycam" /usr/local/bin/skycam

# 6. Service.
echo "[6/7] service"
install -m 644 "$BUNDLE/skycam.service" /etc/systemd/system/skycam.service
systemctl daemon-reload
systemctl enable --quiet skycam
systemctl restart skycam

# 7. Check.
echo "[7/7] checking"
for i in 1 2 3 4 5 6 7 8 9 10; do
    systemctl is-active --quiet skycam && break
    sleep 1
done
if systemctl is-active --quiet skycam; then
    echo "[ok] skycam is running: http://$(hostname).local/"
else
    echo "[error] skycam is not running"
    journalctl -u skycam --no-pager -n 30
    exit 1
fi
rm -rf "$BUNDLE"
