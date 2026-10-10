# Deploying the tc server (DigitalOcean / any Linux VM)

Needs: an Ubuntu/Debian VM with a public IP, inbound **UDP 7443** (QUIC), **TCP 7443** (TLS
fallback + webhooks) and TCP 22 (SSH, for you).

## 1. Harden the VM (do this first)

```sh
ssh root@SERVER
# Key-only SSH: make sure your key is in ~/.ssh/authorized_keys first.
sed -i 's/^#\?PasswordAuthentication .*/PasswordAuthentication no/; s/^#\?KbdInteractiveAuthentication .*/KbdInteractiveAuthentication no/; s/^#\?PermitRootLogin .*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config
sshd -t && systemctl reload ssh

# Firewall: only SSH and tc.
ufw default deny incoming
ufw default allow outgoing
ufw allow 22/tcp
ufw allow 7443/tcp
ufw allow 7443/udp
ufw --force enable

apt-get update && apt-get -y install unattended-upgrades   # security updates
```

On DigitalOcean you can do the same with a cloud firewall (inbound `22/tcp`, `7443/tcp`, `7443/udp`).

## 2. Get the binary

Either the release build (on the server):

```sh
curl -fsSL https://raw.githubusercontent.com/pranavjayakumar/terminal-chat/main/install.sh | TC_INSTALL_DIR=/usr/local/bin sh
```

(it picks linux/amd64 or arm64, checks the sha256 against the release's `checksums.txt` and
installs `/usr/local/bin/tc`), or build it on your laptop and copy it:

```sh
make build-linux                       # → bin/tc-linux-amd64 (build-linux-arm64 for ARM VMs)
scp bin/tc-linux-amd64 root@SERVER:/usr/local/bin/tc
```

Then the service file:

```sh
scp deploy/tc.service root@SERVER:/etc/systemd/system/tc.service
```

## 3. Set up the service

```sh
ssh root@SERVER
useradd --system --home /var/lib/tc --shell /usr/sbin/nologin tc
install -d -o tc -g tc -m 700 /var/lib/tc
# Save the address teammates will use (it goes into every invite):
sudo -u tc tc server --data /var/lib/tc --public SERVER_IP:7443 & sleep 2; kill %1
systemctl daemon-reload && systemctl enable --now tc
journalctl -u tc -n 30                  # shows your admin invite (first run only)
```

Run the admin invite's one-liner (or `tc join '<code>'`) on your laptop.

## 4. Invite the team

```sh
sudo -u tc tc server invite --data /var/lib/tc --uses 15 --ttl 72h   # one invite, 15 people, 3 days
sudo -u tc tc server invite --data /var/lib/tc                       # one person, 7 days
sudo -u tc tc server invite --data /var/lib/tc --admin               # another admin
```

Each prints a line like

```sh
curl -fsSL https://raw.githubusercontent.com/pranavjayakumar/terminal-chat/main/install.sh | sh -s -- 'tc://SERVER_IP:7443/…#…'
```

Teammates paste it: it installs tc and runs `tc join`. Inside tc, admins can run
`/invite 15 3d` instead; it copies the same line. Invites are limited to 100 uses and 30 days;
anyone holding one can join, so share it where only your team can read it.

## Upgrades & backups

- Upgrade: `tc update` as root on the server (or copy a new binary), then `systemctl restart tc`.
  Clients reconnect and catch up automatically; they update themselves with `tc update`.
- Backup: `sqlite3 /var/lib/tc/tc.db ".backup /root/tc-$(date +%F).db"` (safe while running), plus
  `server.key`/`server.crt` — **losing the key invalidates every client's pin and every invite**.
