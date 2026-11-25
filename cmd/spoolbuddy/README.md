# SpoolBuddy station

The Go daemon runs on a Raspberry Pi connected to a PN5180 NFC reader and/or an NAU7802 scale. It registers through the Bambastic API, sends heartbeats, reports stable weights and tag presence, and polls for calibration, NFC writes, diagnostics and station commands. The backend remains responsible for inventory changes.

## Install

Build the binary for the Pi's operating system. From the Bambastic repository root, for 64-bit Raspberry Pi OS, run the following command.

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o bin/spoolbuddy-linux-arm64 ./cmd/spoolbuddy
```

Copy the binary to the Pi. Create a dedicated `spoolbuddy` account with its own home directory. The deployment below assumes that directory is `/home/spoolbuddy`. On the Pi, enable I²C and SPI using its hardware configuration and grant the service account access to the appropriate device groups.

```sh
sudo useradd --create-home --shell /bin/bash spoolbuddy
sudo usermod -aG gpio,i2c,spi spoolbuddy
sudo install -d -o spoolbuddy -g spoolbuddy -m 0755 /home/spoolbuddy/.local/bin
sudo install -o spoolbuddy -g spoolbuddy -m 0755 ./spoolbuddy-linux-arm64 /home/spoolbuddy/.local/bin/spoolbuddy
sudo install -d -o root -g root -m 0755 /etc/spoolbuddy
sudo install -o root -g spoolbuddy -m 0640 /dev/null /etc/spoolbuddy/station.env
```

Skip the account-creation command when the station already has this account. Edit `/etc/spoolbuddy/station.env`. Use the installation's reachable URL and, for an authenticated server, an inventory-management API key dedicated to this station. The device ID should be stable and distinct for each Pi.

```dotenv
SPOOLBUDDY_BACKEND_URL=https://bambastic.example.org
SPOOLBUDDY_DEVICE_ID=material-room
SPOOLBUDDY_API_KEY=replace-with-station-key
SPOOLBUDDY_CONFIG_FILE=/var/lib/spoolbuddy/config.json
```

Save this unit as `/etc/systemd/system/spoolbuddy.service`.

```ini
[Unit]
Description=Bambastic SpoolBuddy station
After=network-online.target
Wants=network-online.target

[Service]
User=spoolbuddy
Group=spoolbuddy
EnvironmentFile=/etc/spoolbuddy/station.env
StateDirectory=spoolbuddy
StateDirectoryMode=0700
ExecStart=/home/spoolbuddy/.local/bin/spoolbuddy
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now spoolbuddy.service
journalctl -u spoolbuddy.service -f
```

Open SpoolBuddy in the operator app. The new station should appear with its hostname and last heartbeat. Use the scale or NFC diagnostic to check the actual sensor reading. A changed backend URL is written to the mode-0600 configuration file. The file takes precedence over the bootstrap environment on the next start. Supply a new API key when moving the station to another backend.

## Spool metadata on scan

A readable OpenTag3D v1.000 or v2.004 record prefills Quick Add with its material, brand, color and label weight. A Bambu spool can also be identified through an enrolled printer’s AMS report. When one physical roll has two RFID stickers, the shared tray UUID allows either sticker to match the same inventory record. [OpenTag3D specification](https://opentag3d.info/spec.html)

For MIFARE Classic media with issuer-provisioned read keys, set `SPOOLBUDDY_TAG_KEYS_FILE` on the station to a local JSON file readable by the station service. Index entries by the uppercase eight-character tag UID, then sector number. Each sector entry has a `type` of `A` or `B` and a `key` containing the 12 hexadecimal characters supplied for that tag and sector. The station reads the configured spool data blocks and sends the decoded identity and spool data to Bambastic. Keep this file local to the Pi. With an unavailable key or unreadable record, the kiosk can still match an existing spool by its UID. [PN5180 data sheet, MIFARE authentication](https://www.nxp.com/docs/en/data-sheet/PN5180A0XX-C1-C2.pdf)

## Pair updates

The server can install a binary staged by an administrator. Build for the target Pi's architecture and place it on the server at `DATA/spoolbuddy/spoolbuddy-linux-arm64`, or set `BAMBASTIC_SPOOLBUDDY_BINARY` to the path read by the server process. An installation with a mixture of ARM architectures should stage the matching image before each update. Version checks compare the station's reported version with the server's station version.

Retrieve the pairing public key with a credential that has `settings:read` from the API endpoint `GET /api/v1/spoolbuddy/ssh/public-key`. Append its `public_key` value as a single line in `/home/spoolbuddy/.ssh/authorized_keys` on the Pi. Keep that directory owned by the station account at mode 0700 and its authorized-keys file at mode 0600. The server uses SSH as `spoolbuddy` on port 22 of the station's registered IP. Its first successfully authenticated SSH session pins the Pi's host key. Later updates verify the stored key. Check the registered address on the Pi before pairing. After a legitimate host replacement, delete the old station registration in Bambastic and register the replacement.

The station account needs permission to restart its fixed service after installing an update. Create a root-owned mode-0440 sudoers drop-in using `visudo -f /etc/sudoers.d/spoolbuddy`.

```sudoers
spoolbuddy ALL=(root) NOPASSWD: /usr/bin/systemctl restart spoolbuddy.service
```

The operator's optional restart-browser, reboot, and shutdown actions use the following additional entries when those actions are enabled for the station. The browser restart targets the kiosk login session on tty1.

```sudoers
spoolbuddy ALL=(root) NOPASSWD: /usr/bin/systemctl restart getty@tty1.service
spoolbuddy ALL=(root) NOPASSWD: /usr/bin/systemctl reboot
spoolbuddy ALL=(root) NOPASSWD: /usr/bin/systemctl poweroff
```

The update streams the local executable through the paired SSH session. The station verifies the transmitted SHA-256 digest before replacing its user-owned binary and restarting the unit. The next registration and heartbeat report the running version.
