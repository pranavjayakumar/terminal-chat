# Deploying the tc server (DigitalOcean / any Linux VM)

Needs: Ubuntu/Debian VM with a public IP, inbound **UDP 7443** (QUIC), **TCP 7443** (fallback), TCP 22.

## 1. Build and copy (from your laptop)

```sh
make build-linux                       # → bin/tc-linux-amd64 (use build-linux-arm64 for ARM VMs)
scp bin/tc-linux-amd64 root@SERVER:/usr/local/bin/tc
scp deploy/tc.service root@SERVER:/etc/systemd/system/tc.service
```

## 2. Set up on the server

```sh
ssh root@SERVER
useradd --system --home /var/lib/tc --shell /usr/sbin/nologin tc
install -d -o tc -g tc -m 700 /var/lib/tc
# Tell tc the address teammates will use (saved for invite codes):
sudo -u tc tc server --data /var/lib/tc --public SERVER_IP:7443 & sleep 2; kill %1
systemctl daemon-reload && systemctl enable --now tc
journalctl -u tc -n 20                  # shows the first admin invite
```

DigitalOcean cloud firewall (or `ufw`): allow `7443/udp`, `7443/tcp`, `22/tcp`.

## 3. Invite people

```sh
sudo -u tc tc server invite --data /var/lib/tc            # one per teammate
sudo -u tc tc server invite --data /var/lib/tc --admin    # admin invite
```
Admins can also run `/invite` inside tc.

## Upgrades & backups

- Upgrade: copy the new binary, `systemctl restart tc`. Clients reconnect and catch up automatically.
- Backup: `sqlite3 /var/lib/tc/tc.db ".backup /root/tc-$(date +%F).db"` (safe while running), plus
  `server.key`/`server.crt` — **losing the key invalidates every client's pin**.
