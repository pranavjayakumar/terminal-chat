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
sudo -u tc tc server invite --data /var/lib/tc --admin               # another admin (single use, 24h)
sudo -u tc tc server invite --data /var/lib/tc --list                # open invites: id, kind, uses left, expiry, creator
sudo -u tc tc server invite --data /var/lib/tc --revoke ID           # kill a leaked invite at once
```

Each prints a line like

```sh
curl -fsSL https://raw.githubusercontent.com/pranavjayakumar/terminal-chat/main/install.sh | sh -s -- 'tc://SERVER_IP:7443/…#…'
```

Teammates paste it: it installs tc and runs `tc join`. Inside tc, admins can run
`/invite 15 3d` instead; it copies the same line. `/invites` lists open invites and
`/invite-revoke ID` revokes one. Invites are limited to 100 uses and 30 days
(admin invites: one use, 24 hours); anyone holding one can join, so share it where only your
team can read it.

## Upgrades & backups

- Upgrade: `tc update` as root on the server (or copy a new binary), then `systemctl restart tc`.
  Clients reconnect and catch up automatically; they update themselves with `tc update`.
- Backup: `sqlite3 /var/lib/tc/tc.db ".backup /root/tc-$(date +%F).db"` (safe while running), plus
  `server.key`/`server.crt` — **losing the key invalidates every client's pin and every invite**.

## Cutting a release (maintainers)

Releases are signed by a maintainer's own SSH key, never by a key in CI: the workflow only
builds a **draft**, and nothing installs a release until its `checksums.txt` carries a valid
`checksums.txt.sig`. Both `tc update` and `install.sh` check that signature against the
allowed signers compiled into `internal/release/signers.go` and embedded in `install.sh`
(`ssh-keygen -Y verify -I tc-release -n tc-release`).

1. Once: put your public key, in allowed_signers format
   (`tc-release namespaces="tc-release" ssh-ed25519 AAAA… you@host`), into both
   `AllowedSigners` in `internal/release/signers.go` and `ALLOWED_SIGNERS='…'` in `install.sh`
   (`go test ./internal/release` checks they match), and merge to `main`. Builds that still
   hold `REPLACE_WITH_MAINTAINER_KEY` refuse to update, and such an `install.sh` refuses to install.
2. Tag a commit that is on `main`: `git tag v0.2.0 && git push origin v0.2.0`. The workflow
   rejects anything but `vX.Y.Z` and commits not on `origin/main`, runs the tests, builds, and
   creates a draft release with the tarballs and `checksums.txt`.
3. `make release-sign TAG=v0.2.0 KEY=~/.ssh/id_ed25519` downloads the draft's assets, checks
   them against `checksums.txt`, signs it (`ssh-keygen -Y sign -n tc-release`), checks the
   signature exactly as `tc update` will, uploads `checksums.txt.sig` and publishes the release
   as latest.

`TC_INSECURE_NO_SIG=1 sh install.sh` skips the signature check. Don't: anyone who can change the
release (or what your network serves for it) can then run code as you.

Repository settings that back this up: branch protection on `main`, 2FA for everyone with
write access, a tag ruleset limiting who can push `v*` tags, and required reviewers on the
`release` environment the publish job uses.
