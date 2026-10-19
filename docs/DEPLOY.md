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

### Serve on 443 too (optional)

Some networks (offices, hotels, phone hotspots) block outbound 7443, and teammates there see
`can't reach the server at SERVER_IP:7443`. tc speaks QUIC on UDP and TLS on TCP, both of which pass
on 443, so redirect 443 to 7443 in the kernel and keep the server where it is. Add this block at the
very top of `/etc/ufw/before.rules`, above the `*filter` line:

```
# tc: answer on 443 as well as 7443
*nat
:PREROUTING ACCEPT [0:0]
-A PREROUTING -p tcp --dport 443 -j REDIRECT --to-ports 7443
-A PREROUTING -p udp --dport 443 -j REDIRECT --to-ports 7443
COMMIT
```

then `ufw reload`. The redirect happens before filtering, so the `7443` rules above already admit
it (on a DigitalOcean cloud firewall, also allow inbound `443/tcp` and `443/udp`). Check from a
laptop with `tc login 'tc://SERVER_IP:443#PIN'`. The pin doesn't depend on the port, so any invite
works on either port: teammates whose network blocks 7443 change `:7443` to `:443` in it. To put
443 in every new invite instead, start the server once with `--public SERVER_IP:443` (step 3).
Nothing else may be listening on 443 (a web server, for instance).

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
sudo -u tc tc server invite --data /var/lib/tc --uses 15 --ttl 24h   # one invite, 15 people, 1 day
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
`/invite 15 1d` instead; it copies the same line. `/invites` lists open invites and
`/invite-revoke ID` revokes one. Invites are limited to 100 uses and 30 days
(admin invites: one use, 24 hours); anyone holding one can join, so share it where only your
team can read it, keep the TTL short (a day, not a month) and revoke what's left once everyone is
in. The invite sits in shell history and, while `install.sh` runs, in the `sh -s -- '…'` command line
that `ps` shows other users on a shared machine; `install.sh` hands it to `tc join` in `TC_INVITE`,
not argv, so it isn't visible for the length of the join.

## Upgrades & backups

- Update check: release builds of the tc *client* make one HTTPS `HEAD` request to
  `github.com/pranavjayakumar/terminal-chat/releases/latest` at most once every 24 hours (per
  `TC_HOME`, at TUI start, 5 s timeout) to show `update vX` in the sidebar. It sends no version,
  user, server or channel data, but GitHub sees the machine's IP and that it runs tc. On private or
  air-gapped teams, set `TC_NO_UPDATE_CHECK=1` in teammates' environment. The server never checks.
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
   creates a draft release with the tarballs and `checksums.txt`. There are eight tarballs:
   `tc_<os>_<arch>` (static, `CGO_ENABLED=0`) and the voice flavor `tc-voice_<os>_<arch>`
   (`-tags voice`, libopus 1.5.2 built from a checksummed xiph.org tarball and linked
   statically by `scripts/build-voice.sh` on native runners: macos-14 for both darwin arches,
   ubuntu-22.04 and ubuntu-22.04-arm for Linux, so Linux voice builds need glibc ≥ 2.35).
   One `checksums.txt` covers all eight, so one signature covers the voice builds too.
3. `make release-sign TAG=v0.2.0 KEY=~/.ssh/id_ed25519` downloads the draft's assets, checks
   them against `checksums.txt`, signs it (`ssh-keygen -Y sign -n tc-release`), checks the
   signature exactly as `tc update` will, uploads `checksums.txt.sig` and publishes the release
   as latest.
4. Homebrew: `make release-sign` ends by regenerating `Formula/tc.rb` from the just-signed
   `checksums.txt` (`scripts/brew-formula.sh`, which re-verifies the signature first). Commit the
   change on a branch and open a PR to `main`; until it merges, `brew upgrade` stays on the old
   version. Users tap this repo directly:
   `brew tap pranavjayakumar/tc https://github.com/pranavjayakumar/terminal-chat && brew install pranavjayakumar/tc/tc`.
   To regenerate by hand: `scripts/brew-formula.sh vX.Y.Z`. `tc update` on a Homebrew install
   (binary under `Cellar/`) tells the user to run `brew upgrade tc` instead of replacing the binary.

Downloads are https-only, redirects included (`install.sh` runs curl with `--proto '=https'
--proto-redir '=https' --tlsv1.2` and ignores `~/.curlrc`; `tc update` refuses non-https redirects).
`TC_RELEASE_BASE` may point at a mirror, but only over https (plain http only to localhost, for tests).
`TC_INSECURE_NO_SIG=1 sh install.sh` skips the signature check. Don't: anyone who can change the
release (or what your network serves for it) can then run code as you.

Repository settings that back this up: branch protection on `main`, 2FA for everyone with
write access, a tag ruleset limiting who can push `v*` tags, and required reviewers on the
`release` environment the publish job uses.
