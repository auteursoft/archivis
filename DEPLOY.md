# Deploying Archivis on macOS and Ubuntu

This guide sets Archivis up on one machine that can see your photo drives:
the web interface runs as a background service that starts at boot, and an
incremental index runs every night to pick up new photos.

* [Before you start](#before-you-start)
* [macOS](#macos)
* [Ubuntu](#ubuntu)
* [Using it from other devices](#using-it-from-other-devices)
* [Backups](#backups)
* [Teaching it your taste](#teaching-it-your-taste)
* [Updating](#updating)
* [Troubleshooting](#troubleshooting)

**What was tested.** The Ubuntu steps were run end to end on Ubuntu 24.04
from a fresh clone with an empty home directory: build, setup, indexing JPEG
and HEIC, the web interface with a password, HTTPS through Caddy, backup and
restore. The unit files were checked with `systemd-analyze verify`. The macOS
steps follow the same commands but **have not been run on a Mac**; the
launchd files were checked for valid syntax only. Please report anything that
differs.

## Before you start

**Hardware.** Plan for the catalogue, not the photos (Archivis only reads
your originals and never modifies them):

| | 1M photos | 3M photos, 5M faces |
|---|---|---|
| RAM for the web interface | ~4 GB | ~10 GB (16 GB machine recommended) |
| Catalogue database | ~3 GB | ~11 GB |
| Thumbnails | ~40 GB | ~120 GB |
| Models | 0.8 GB | 0.8 GB |

Put the data directory (`~/.archivis` by default) on an SSD. If your home
disk is small, point it elsewhere with `ARCHIVIS_DATA=/path` (or `--data`)
everywhere Archivis runs, including the service files below.

**Time.** Indexing on the CPU runs at roughly 0.5 photos per second per core:
about a week for 3M photos on a 10-core machine. It is incremental and
resumable: stop it any time, and the next run continues where it left off.
Later runs only look at new or changed files.

**Getting the code.** The code is at
[github.com/auteursoft/archivis](https://github.com/auteursoft/archivis).

## macOS

Apple Silicon (M1 or later) is the supported setup: everything installs with
one command and the neural engine is used automatically.

### 1. Install the tools

```sh
xcode-select --install          # C compiler (needed to build); skip if already installed
# Homebrew, if you don't have it: https://brew.sh
brew install go git
go version                      # must be 1.24 or newer
```

HEIC photos need nothing extra: macOS's built-in `sips` converts them.

### 2. Build and install

```sh
git clone https://github.com/auteursoft/archivis.git ~/src/archivis
cd ~/src/archivis
go build -o archivis ./cmd/archivis
sudo mkdir -p /usr/local/bin
sudo install -m 755 archivis /usr/local/bin/archivis
```

### 3. Download the runtime and models

```sh
archivis setup                  # ~800 MB into ~/.archivis, checksum-verified
```

**Intel Macs:** Microsoft no longer publishes ONNX Runtime for Intel Macs, so
`setup` stops at that step. Install it with `brew install onnxruntime` (it
must be version 1.29 or newer: check with `brew info onnxruntime`), then run
`archivis setup --skip-runtime`. Archivis looks in `/usr/local/lib` and
`/opt/homebrew/lib` automatically.

### 4. Allow access to your drives

macOS blocks programs from reading external drives and some folders until you
allow it, and a background service cannot show the permission prompt. Grant
access once:

1. Open **System Settings → Privacy & Security → Full Disk Access**.
2. Click **+**, press **⌘⇧G**, type `/usr/local/bin/archivis`, and add it.
3. Do the same for the Terminal app you use, for manual runs.

Rebuilding Archivis (see [Updating](#updating)) can reset this permission: if
the nightly index suddenly finds no files, remove and re-add the binary here.

### 5. First index

Run the first, long index from Terminal, and keep the Mac awake while it runs
(`caffeinate -i` prevents idle sleep; close the lid only if the Mac is set to
keep running on power):

```sh
caffeinate -i archivis index /Volumes/Archive1 /Volumes/Archive2
```

Interrupt it with Ctrl-C whenever you need to; running the same command again
resumes. You can start the web interface (next step) while this runs; new
photos appear in it within a minute.

### 6. Run the web interface as a service

This creates a per-user launchd agent that starts when you log in and
restarts if it stops:

```sh
mkdir -p ~/Library/LaunchAgents ~/Library/Logs
cat > ~/Library/LaunchAgents/com.archivis.serve.plist <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.archivis.serve</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/archivis</string>
    <string>serve</string>
    <string>--addr</string><string>127.0.0.1:8088</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>$HOME/Library/Logs/archivis-serve.log</string>
  <key>StandardErrorPath</key><string>$HOME/Library/Logs/archivis-serve.log</string>
</dict>
</plist>
EOF
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.archivis.serve.plist
open http://127.0.0.1:8088
```

Starting takes a while on a large catalogue (about 25 s per million photos)
while the search indexes load.

### 7. Index new photos every night

```sh
cat > ~/Library/LaunchAgents/com.archivis.index.plist <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.archivis.index</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/bin/caffeinate</string><string>-i</string>
    <string>/usr/local/bin/archivis</string>
    <string>index</string>
    <string>/Volumes/Archive1</string>
    <string>/Volumes/Archive2</string>
  </array>
  <key>StartCalendarInterval</key>
  <dict><key>Hour</key><integer>2</integer><key>Minute</key><integer>30</integer></dict>
  <key>ProcessType</key><string>Background</string>
  <key>LowPriorityIO</key><true/>
  <key>Nice</key><integer>10</integer>
  <key>StandardOutPath</key><string>$HOME/Library/Logs/archivis-index.log</string>
  <key>StandardErrorPath</key><string>$HOME/Library/Logs/archivis-index.log</string>
</dict>
</plist>
EOF
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.archivis.index.plist
```

Edit the `/Volumes/...` lines to match your drives. A drive that isn't
plugged in is skipped with a warning; the others are still indexed. If the
Mac is asleep at 2:30, launchd runs the job when it wakes.

### Managing the services

```sh
launchctl kickstart -k gui/$(id -u)/com.archivis.serve   # restart
launchctl kickstart gui/$(id -u)/com.archivis.index      # run the index now
launchctl bootout gui/$(id -u)/com.archivis.serve        # stop and disable
tail -f ~/Library/Logs/archivis-serve.log ~/Library/Logs/archivis-index.log
```

## Ubuntu

Tested on Ubuntu 24.04; 22.04 works the same way. These steps run Archivis
under your own account, with the catalogue in `~/.archivis`.

### 1. Install the packages

```sh
sudo apt update
sudo apt install -y build-essential git curl sqlite3 \
                    libheif-examples libheif-plugin-libde265
```

`libheif-plugin-libde265` is what decodes HEIC photos from iPhones. Ubuntu
does not install it with `libheif-examples`, and without it every HEIC file
fails with "Unsupported codec".

### 2. Install Go

Ubuntu's own `golang-go` package is too old (Archivis needs Go 1.24 or newer).
Install the official release:

```sh
curl -LO https://go.dev/dl/go1.24.7.linux-amd64.tar.gz     # use linux-arm64 on ARM machines
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.24.7.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.profile && source ~/.profile
go version
```

### 3. Build and install

```sh
git clone https://github.com/auteursoft/archivis.git ~/src/archivis
cd ~/src/archivis
go build -o archivis ./cmd/archivis          # ~3 minutes the first time
sudo install -m 755 archivis /usr/local/bin/archivis
```

### 4. Download the runtime and models

```sh
archivis setup                               # ~800 MB into ~/.archivis, checksum-verified
```

**NVIDIA GPU (optional, untested):** run `archivis setup --cuda` instead, and
add `--provider cuda` to the `index` command. This needs the CUDA 12 and
cuDNN 9 runtime libraries installed. If anything goes wrong, the CPU build
always works.

### 5. Mount your photo drives at fixed paths

So that the nightly index always finds them, mount the drives at the same
place on every boot. Find each drive's UUID with `lsblk -f`, create a mount
point with `sudo mkdir -p /mnt/archive1`, and add a line to `/etc/fstab`:

```
# ext4
UUID=1234-abcd  /mnt/archive1  ext4   ro,nofail,x-systemd.device-timeout=10s  0  2
# exFAT (drives shared with a Mac or Windows)
UUID=5678-ef01  /mnt/archive2  exfat  ro,nofail,uid=1000,gid=1000,x-systemd.device-timeout=10s  0  0
```

`ro` mounts read-only (Archivis never writes to your photos, so this is free
protection). `nofail` lets the machine boot when a drive is unplugged.
Then run `sudo mount -a`.

### 6. Run the web interface as a service

```sh
sudo tee /etc/systemd/system/archivis.service >/dev/null <<EOF
[Unit]
Description=Archivis web interface
After=network.target local-fs.target

[Service]
User=$(id -un)
Environment=ARCHIVIS_DATA=$HOME/.archivis
EnvironmentFile=-/etc/archivis.env
ExecStart=/usr/local/bin/archivis serve --addr 127.0.0.1:8088
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
sudo systemctl daemon-reload
sudo systemctl enable --now archivis
```

It listens on `127.0.0.1:8088` only. To use it from other devices, see
[Using it from other devices](#using-it-from-other-devices).

### 7. Index: first run and every night

One service does the indexing; a timer starts it nightly:

```sh
sudo tee /etc/systemd/system/archivis-index.service >/dev/null <<EOF
[Unit]
Description=Archivis index
After=local-fs.target

[Service]
Type=oneshot
User=$(id -un)
Environment=ARCHIVIS_DATA=$HOME/.archivis
Nice=10
IOSchedulingClass=idle
ExecStart=/usr/local/bin/archivis index /mnt/archive1 /mnt/archive2
EOF

sudo tee /etc/systemd/system/archivis-index.timer >/dev/null <<EOF
[Unit]
Description=Index new photos nightly

[Timer]
OnCalendar=*-*-* 02:30
Persistent=true

[Install]
WantedBy=timers.target
EOF
sudo systemctl daemon-reload
sudo systemctl enable --now archivis-index.timer
sudo systemctl start --no-block archivis-index   # start the first (long) index now
```

Edit the folders on the `ExecStart` line to match your mount points. The
first index runs in the background and survives logging out; if it is still
running at 2:30 the timer does not start a second copy. A drive that isn't
mounted is skipped with a warning.

### Managing the services

```sh
systemctl status archivis archivis-index
journalctl -u archivis-index -f          # indexing progress, one line a minute
journalctl -u archivis -f                # web interface log
sudo systemctl restart archivis
sudo systemctl stop archivis-index       # pause indexing; start it again to resume
```

## Using it from other devices

The web interface listens on the machine itself only (`127.0.0.1`), so
nothing is exposed until you choose one of these:

**SSH tunnel (simplest, encrypted, no configuration).** From your laptop:

```sh
ssh -N -L 8088:127.0.0.1:8088 you@photo-server
```

then open <http://127.0.0.1:8088> on the laptop.

**Your home network, with a password.** (Once you create
[accounts](#accounts), they replace this shared password.) Store the
password where only root can read it (it then never appears in the process
list or the service file):

```sh
echo 'ARCHIVIS_AUTH=yourname:a-long-password' | sudo tee /etc/archivis.env >/dev/null
sudo chmod 600 /etc/archivis.env
sudo sed -i 's/--addr 127.0.0.1:8088/--addr 0.0.0.0:8088/' /etc/systemd/system/archivis.service
sudo systemctl daemon-reload && sudo systemctl restart archivis
sudo ufw allow from 192.168.0.0/16 to any port 8088 proto tcp   # if ufw is enabled
```

On macOS, put the same value in the serve plist instead
(`<key>EnvironmentVariables</key><dict><key>ARCHIVIS_AUTH</key><string>yourname:a-long-password</string></dict>`),
change `127.0.0.1:8088` to `0.0.0.0:8088`, `chmod 600` the plist, and restart
the agent.

The password is sent unencrypted over plain HTTP, so use this only on a
network you trust. For anything else use the SSH tunnel or HTTPS.

**HTTPS with Caddy (Ubuntu).** `sudo apt install caddy`, keep Archivis on
`127.0.0.1:8088`, set the password as above, and replace
`/etc/caddy/Caddyfile` with:

```
photos.example.com {
	reverse_proxy 127.0.0.1:8088
}
```

then `sudo systemctl reload caddy`. Caddy obtains a certificate
automatically when the name points at the machine. On a home network without
a public name, use a local name with `tls internal` inside the block.
If you use a different proxy, it must pass the original `Host` header
through (Caddy does by default). Otherwise Archivis refuses label changes as
cross-site requests.

## Backups

Your originals are untouched, and the models and thumbnails can be
recreated. What can't be recreated is the database: every name you have
given, every face you have confirmed, and every aesthetic rating you have
given (with the models trained from them). Back it up; this is safe while
Archivis is running:

```sh
sqlite3 ~/.archivis/archivis.db ".backup '/path/to/backups/archivis-$(date +%F).db'"
```

To restore, stop the services, copy a backup to `~/.archivis/archivis.db`,
and start them again.

To back up automatically every night (keeping 30 days), run `crontab -e` and
add this line. In a crontab `%` means "new line", so it must be written `\%`
as below; copied without the backslashes, the job silently never runs.

```
17 4 * * * mkdir -p "$HOME/archivis-backups" && /usr/bin/sqlite3 "$HOME/.archivis/archivis.db" ".backup '$HOME/archivis-backups/archivis-$(date +\%F).db'" && find "$HOME/archivis-backups" -name 'archivis-*.db' -mtime +30 -delete
```

Point `archivis-backups` at another disk if you can. Time Machine and most
backup tools also work, but a `.backup` copy is guaranteed to be consistent.

## Accounts

A shared password suits one person. For several people, give each their own
account; the shared `ARCHIVIS_AUTH` password then stops working, and
everyone signs in with their own name and password. On the server:

```sh
archivis users add-admin yourname        # asks for a password (at least 12 characters)
```

That takes effect at once, even with the web interface running. Then invite
people from the **Users** page (top right, for admins): enter a name and
role, and send the link it shows. Each link works once, for 7 days; the
person chooses their own password with it. Or from the server:

```sh
archivis users invite ann --role editor --url https://photos.example.com
```

Roles:

| role   | can |
|--------|-----|
| viewer | browse, search, search by photo |
| editor | also name people, fix face labels, rate photos, download originals |
| admin  | also invite people and manage accounts |

On the Users page an admin can change a role, make a password-reset link,
sign someone out of every device, or disable an account (which signs it out
at once). There is always at least one active admin. Locked out? On the
server, `archivis users password yourname` sets a new password, and
`archivis users list` shows everyone and any pending invitations.

How it is protected:

- Passwords are stored as Argon2id hashes, never in the clear.
- Sign-ins last 30 days, in a cookie that scripts on a page cannot read. The
  cookie is sent only over HTTPS when the site is served that way (directly
  or through Caddy on the same machine).
- The database holds only hashes of sign-in and invitation tokens, so a copy
  of a backup cannot be used to sign in.
- After 10 failed sign-ins for a name, or 20 from one address, within 15
  minutes, further attempts wait. A name that does not exist takes as long
  to refuse as a wrong password.
- Forms and changes from other websites are refused, and pages cannot be
  embedded in other sites.

Use accounts over HTTPS (see above). Over plain HTTP, passwords and
sign-in cookies cross the network unencrypted.

## Teaching it your taste

Rate photos on their pages in the web interface (or say the aesthetic score
is too high or too low). Once you have given 20 or more judgements, run:

```sh
archivis aesthetic train     # fits, tests on held-out judgements, adopts it only if better
archivis aesthetic models    # every model used, its blend weight and how it was evaluated
```

This takes seconds, plus about 30 s per million photos to score them with
the new model. The web interface shows the new scores straight away; there
is no need to restart it. With accounts (or a password) on the web
interface, each person's judgements are recorded under their name; `--rater NAME`
trains on one person's judgements only. `train` and `use` wait for a running index to finish (they say so;
`train --dry-run` works meanwhile), since the indexer would keep scoring
new photos with the previous models. To go back to an earlier model:
`archivis aesthetic use eva-ridge`. IDs can be shortened to any unique
prefix.

## Updating

If you cloned the code from its earlier home (the `photodex-go` branch of
`auteursoft/DoFISaC`), point your copy at the new repository once:

```sh
cd ~/src/archivis
git remote set-url origin https://github.com/auteursoft/archivis.git
git fetch origin && git checkout -B main origin/main
```

Then, and for every later update:

```sh
cd ~/src/archivis
git pull
go build -o archivis ./cmd/archivis
sudo install -m 755 archivis /usr/local/bin/archivis
archivis setup                       # fetches anything new; skips what's already there
```

**Coming from photodex** (the project's earlier name): your catalogue moves
from `~/.photodex` to `~/.archivis` by itself the first time `archivis`
runs, and `photodex.db` becomes `archivis.db`; nothing is re-indexed. The
old `PHOTODEX_DATA` and `PHOTODEX_AUTH` variables still work. Replace the
old services with the ones in this guide:

```sh
# Ubuntu
sudo systemctl disable --now photodex photodex-index.timer photodex-index 2>/dev/null
sudo rm -f /etc/systemd/system/photodex*.service /etc/systemd/system/photodex*.timer
[ -f /etc/photodex.env ] && sudo mv /etc/photodex.env /etc/archivis.env   # then rename PHOTODEX_AUTH inside it, or leave it: both work
sudo rm -f /usr/local/bin/photodex
# macOS
for j in serve index; do launchctl bootout gui/$(id -u)/com.photodex.$j 2>/dev/null; rm -f ~/Library/LaunchAgents/com.photodex.$j.plist; done
sudo rm -f /usr/local/bin/photodex
```

then follow the service steps above. On macOS, give `/usr/local/bin/archivis`
Full Disk Access (step 4).

Then restart the web interface (`sudo systemctl restart archivis`, or
`launchctl kickstart -k gui/$(id -u)/com.archivis.serve`). Catalogue changes
are applied automatically when Archivis opens it; nothing needs
re-indexing unless the release notes say so. A release with a new built-in
aesthetic model adds it next to the old one, and it scores your photos from
their stored embeddings. Earlier scores, and models you trained, are kept. On macOS, check Full Disk
Access afterwards (step 4).

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| `onnxruntime shared library not found` | Run `archivis setup`. On Intel Macs install it with Homebrew (macOS step 3). A custom location can be given with `--ort-lib` or `ONNXRUNTIME_LIB`. |
| HEIC files listed by `archivis errors` with "Unsupported codec" | Ubuntu: `sudo apt install libheif-plugin-libde265`, then `archivis index --retry-errors <folders>`. |
| macOS: the index finds no files on `/Volumes/...`, or "operation not permitted" | Full Disk Access is missing (macOS step 4). |
| `warning: skipping /mnt/archive1: not a folder` | That drive isn't mounted; the rest were indexed. Check `lsblk` / `mount`. |
| The web interface is killed, or the machine swaps heavily | Not enough RAM for the catalogue (about 1.1 GB per million photos plus faces, plus 1.4 GB). Add memory, or run `serve` on a bigger machine pointed at the same data directory. |
| `address already in use` | Something else uses port 8088: change `--addr` (e.g. `127.0.0.1:8090`). |
| Names can't be saved through a proxy ("cross-origin request refused") | The proxy rewrites the `Host` header. Pass it through unchanged. |
| `another archivis index is already running on this catalogue` | Only one index runs at a time (for example, the nightly job while your first index is still going). Wait for it to finish, or stop it (`sudo systemctl stop archivis-index`, or Ctrl-C in its terminal); the next run resumes. |
| `list` or `export` says "only the first 100" | Results are capped at `--limit` (default 100); pass a larger number, or `--limit 0` for all. |
| A run was interrupted | Run the same `index` command again; it resumes. `archivis errors` lists files that could not be read. |
