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

**Apple Photos library.** You can index `~/Pictures/Photos Library.photoslibrary`,
or a folder containing it. Only the originals inside it are catalogued. The
library also holds Apple's own resized copies of every photo, and those are
skipped. Copies catalogued by older versions of Archivis are removed at the
end of the next index run, once each one's original is in the catalogue.
Face names and ratings given to a copy are moved to the original first. With
**Optimize Mac Storage** turned on (Photos → Settings → iCloud), many originals
are only in iCloud, and those photos are skipped until you download them.
The index log reports how many copies were kept for that reason.

**Other photo apps.** iPhoto (`.photolibrary`), Aperture (`.aplibrary`) and
Capture One catalogs (`.cocatalog`) are handled the same way: only their
originals (`Masters/`, `Originals/`) are catalogued. A Capture One session's
`CaptureOne` folders (its cache and settings) are skipped wherever they are;
the session's own photos and exports are kept. Thumbnails and previews
catalogued by older versions are removed at the end of the next index run.
Face names and ratings on one move to the photo it copies first: one with
the same perceptual hash, or one taken in the same second by the same
camera that looks alike. A copy with no such twin is removed too, unless
you named faces or rated it, and Apple Photos copies with no twin anywhere
are kept, as they may be the only copy of a photo kept in iCloud. The index
log reports what was removed and kept.

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

### Start at boot, with nobody logged in

The agents above live in your login session: after a restart they start
only once you log in at the Mac's screen (or with Screen Sharing). A
Terminal over `ssh` is not that session, and there `launchctl` answers
`125: Domain does not support specified action`. On a Mac that serves
Archivis all the time, make them system services instead: they start at
boot and still run as you, with your catalogue and Full Disk Access.

With FileVault on, a restart still stops at the unlock screen until someone
types the password; this helps only with FileVault off.

This moves your existing agents, with any settings you added (a password
in `ARCHIVIS_AUTH`, other folders), and works over `ssh`:

```sh
old=~/.archivis/launchagents-old; mkdir -p "$old"
for j in serve index; do
  src=~/Library/LaunchAgents/com.archivis.$j.plist
  dst=/Library/LaunchDaemons/com.archivis.$j.plist
  [ -f "$src" ] || continue
  launchctl bootout gui/$(id -u)/com.archivis.$j 2>/dev/null   # if running in a login session
  sudo cp "$src" "$dst"
  sudo /usr/libexec/PlistBuddy -c "Add :UserName string $(id -un)" "$dst"
  sudo /usr/libexec/PlistBuddy -c "Add :GroupName string staff" "$dst"
  sudo /usr/libexec/PlistBuddy -c "Add :EnvironmentVariables dict" "$dst" 2>/dev/null
  sudo /usr/libexec/PlistBuddy -c "Delete :EnvironmentVariables:HOME" "$dst" 2>/dev/null
  sudo /usr/libexec/PlistBuddy -c "Add :EnvironmentVariables:HOME string $HOME" "$dst"
  sudo chown root:wheel "$dst"; sudo chmod 600 "$dst"   # launchd's rule; 600 also hides a password
  mv "$src" "$old/"          # so logging in does not start a second copy
  sudo launchctl bootstrap system "$dst"
done
sudo launchctl print system/com.archivis.serve | grep -E '^[[:space:]]*(state|last exit)'
```

`HOME` tells Archivis where your catalogue is (`~/.archivis`); a system
service may start without one.

**Drives.** macOS mounts external drives when someone logs in. To mount
them at boot, so the nightly index finds them:

```sh
sudo defaults write /Library/Preferences/SystemConfiguration/autodiskmount AutomountDisksWithoutUserLogin -bool true
```

It applies from the next restart; until then, `diskutil mount Archive1`
mounts a drive by name. Network shares still need a login.

**nginx or Caddy.** The web server in front of Archivis must start at boot
too, or the site stays down until someone logs in. Started with `sudo brew
services` (as the [nginx steps](#using-it-from-other-devices) do), it is
already a system service. Started without `sudo`, it is an agent; move it:

```sh
svc=nginx    # or caddy
if [ -f ~/Library/LaunchAgents/homebrew.mxcl.$svc.plist ]; then
  brew services stop $svc   # a running agent would hold ports 80 and 443 (over ssh, with nobody logged in, none is running)
  rm -f ~/Library/LaunchAgents/homebrew.mxcl.$svc.plist
  sudo brew services start $svc
fi
ls /Library/LaunchDaemons/homebrew.mxcl.$svc.plist    # there: it starts at boot
```

With nginx, the certificate renewal in root's crontab runs without a login
as well.

**Managing them** takes `sudo` and `system/` in place of `gui/$(id -u)/`:

```sh
sudo launchctl kickstart -k system/com.archivis.serve   # restart
sudo launchctl kickstart system/com.archivis.index      # run the index now
sudo launchctl bootout system/com.archivis.serve        # stop and disable
```

To change a setting, edit `/Library/LaunchDaemons/com.archivis.serve.plist`
with `sudo` (`sudo /usr/libexec/PlistBuddy …`, as above), then `sudo
launchctl bootout system/com.archivis.serve` and `sudo launchctl bootstrap
system /Library/LaunchDaemons/com.archivis.serve.plist`. For the password,
the [HTTPS steps](#using-it-from-other-devices) give the commands for both kinds of
service. The logs stay in `~/Library/Logs`.

To go back to agents, stop and remove both services, put the agents back,
and log in at the Mac:

```sh
for j in serve index; do
  sudo launchctl bootout system/com.archivis.$j
  sudo rm -f /Library/LaunchDaemons/com.archivis.$j.plist
done
mv ~/.archivis/launchagents-old/com.archivis.*.plist ~/Library/LaunchAgents/
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

**HTTPS with Caddy (macOS), reachable from anywhere.** For a domain that
already points at your home connection. Caddy obtains and renews a free
certificate itself.

1. On your router, forward **both** port 80 and port 443 to the Mac. Caddy
   needs 80 to prove it owns the domain (and to redirect to HTTPS), and 443
   serves the site.
2. Give Archivis a password first, before Caddy makes it reachable. Keep
   it on `127.0.0.1:8088`, so it is reachable only through Caddy. Use a
   long random password: the shared login has no lockout after failed
   attempts.

   ```sh
   pw=$(openssl rand -base64 24); echo "password: $pw"     # note it down
   plist=~/Library/LaunchAgents/com.archivis.serve.plist
   /usr/libexec/PlistBuddy -c "Add :EnvironmentVariables dict" "$plist" 2>/dev/null
   /usr/libexec/PlistBuddy -c "Delete :EnvironmentVariables:ARCHIVIS_AUTH" "$plist" 2>/dev/null
   /usr/libexec/PlistBuddy -c "Add :EnvironmentVariables:ARCHIVIS_AUTH string yourname:$pw" "$plist"
   chmod 600 "$plist"
   launchctl bootout gui/$(id -u)/com.archivis.serve       # a restart alone keeps the old settings
   launchctl bootstrap gui/$(id -u) "$plist"
   sleep 2; curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8088   # must print 401
   ```

   If you have already moved Archivis to [system services that start at
   boot](#start-at-boot-with-nobody-logged-in), the plist is elsewhere and
   needs `sudo`:

   ```sh
   pw=$(openssl rand -base64 24); echo "password: $pw"     # note it down
   plist=/Library/LaunchDaemons/com.archivis.serve.plist
   sudo /usr/libexec/PlistBuddy -c "Add :EnvironmentVariables dict" "$plist" 2>/dev/null
   sudo /usr/libexec/PlistBuddy -c "Delete :EnvironmentVariables:ARCHIVIS_AUTH" "$plist" 2>/dev/null
   sudo /usr/libexec/PlistBuddy -c "Add :EnvironmentVariables:ARCHIVIS_AUTH string yourname:$pw" "$plist"
   sudo launchctl bootout system/com.archivis.serve       # a restart alone keeps the old settings
   sudo launchctl bootstrap system "$plist"
   sleep 2; curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8088   # must print 401
   ```

   Go on only if that prints `401`. Anything else (`000`, `200`) means
   Archivis is not running with the password yet: check the commands above
   and `launchctl print gui/$(id -u)/com.archivis.serve` (`sudo launchctl
   print system/com.archivis.serve` for a system service) before starting
   Caddy, or the site would be published without a password.
3. Then install Caddy and point it at Archivis:

   ```sh
   brew install caddy
   cat > "$(brew --prefix)/etc/Caddyfile" <<'CADDY'
   photos.example.com {
   	reverse_proxy 127.0.0.1:8088
   }
   CADDY
   brew services start caddy
   ```

   If Caddy cannot bind ports 80 and 443, start it with
   `sudo brew services start caddy` instead. Allow incoming connections if
   macOS asks.
4. From outside your network (a phone off Wi-Fi, say), check that a password
   is required, then log in with a browser:

   ```sh
   curl -s -o /dev/null -w '%{http_code}\n' https://photos.example.com   # expect: 401
   ```

Things to know:

- **One shared login.** Anyone with the password can view every photo,
  download the originals, and rename or relabel people. The user name you
  choose is recorded with each aesthetic rating. For several people, use
  [accounts](#accounts) instead: each person signs in with their own
  password and role, failed sign-ins are limited, and the shared password
  stops working.
- **Stay awake.** The web interface is a per-user agent, so it runs only
  while you are logged in. Locking the screen keeps it running; after a
  restart, log in again (automatic login would leave the Mac open to anyone
  who can reach it), or [start it at boot](#start-at-boot-with-nobody-logged-in)
  so no login is needed. Stop the Mac sleeping (System Settings → Energy).
- **Without opening ports.** Tailscale or a Cloudflare Tunnel give encrypted
  remote access with nothing forwarded on the router.

**HTTPS with nginx (macOS or Ubuntu).** If you already run nginx, or
prefer it to Caddy. Archivis stays on `127.0.0.1:8088`, and nginx, on the
same machine, serves HTTPS and passes requests to it. Forward ports 80 and
443 on your router to the machine. As with Caddy, give Archivis a password
or [accounts](#accounts) before nginx makes it reachable, and check that
`curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8088` prints
`401` (shared password) or `303` (accounts).

1. Install nginx and certbot:

   ```sh
   brew install nginx certbot                                  # macOS
   sudo apt install nginx certbot                              # Ubuntu
   ```

   The configuration folder is `$(brew --prefix)/etc/nginx/servers/` on
   macOS and `/etc/nginx/sites-enabled/` on Ubuntu; below it is written
   `CONF`. Certificate challenges are served from a folder written `WEBROOT`:
   use `$(brew --prefix)/var/www` on macOS and `/var/www/html` on Ubuntu.

2. Serve port 80 first, so certbot can prove you own the name. Save as
   `CONF/archivis.conf` (replace `photos.example.com` and `WEBROOT`):

   ```nginx
   server {
       listen 80;
       server_name photos.example.com;
       location /.well-known/acme-challenge/ { root WEBROOT; }
       location / { return 301 https://$host$request_uri; }
   }
   ```

   Then start or reload nginx and get the certificate:

   ```sh
   sudo nginx -t && sudo brew services restart nginx           # macOS
   sudo nginx -t && sudo systemctl reload nginx                # Ubuntu
   sudo certbot certonly --webroot -w WEBROOT -d photos.example.com
   ```

3. Add the HTTPS server to the same file:

   ```nginx
   server {
       listen 443 ssl http2;
       server_name photos.example.com;
       ssl_certificate     /etc/letsencrypt/live/photos.example.com/fullchain.pem;
       ssl_certificate_key /etc/letsencrypt/live/photos.example.com/privkey.pem;

       client_max_body_size 200m;   # search by photo accepts uploads up to 200 MB (RAW files)

       location / {
           proxy_pass http://127.0.0.1:8088;
           proxy_set_header Host              $host;
           proxy_set_header X-Forwarded-Proto $scheme;
           proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
           proxy_read_timeout 300s;
       }
   }
   ```

   and reload nginx as in step 2. (nginx 1.25 and later warn that
   `listen … http2` is deprecated; it still works, or write `listen 443 ssl;`
   with `http2 on;` on the next line.)

   The three headers matter:
   - **`Host`**: nginx otherwise sends `127.0.0.1:8088`. Archivis then
     refuses every change (naming people, rating, signing in) as a
     cross-site request.
   - **`X-Forwarded-Proto`**: sign-in cookies are marked Secure only when
     Archivis knows the browser used HTTPS.
   - **`X-Forwarded-For`**: failed sign-ins are limited per address, so
     Archivis needs the browser's address, not nginx's. It believes these
     headers only from a proxy on the same machine.

4. Renew the certificate automatically. On Ubuntu the certbot package
   already does this; add a hook so nginx picks up the new certificate:

   ```sh
   echo 'systemctl reload nginx' | sudo tee /etc/letsencrypt/renewal-hooks/deploy/nginx >/dev/null
   sudo chmod +x /etc/letsencrypt/renewal-hooks/deploy/nginx
   ```

   On macOS, run `sudo crontab -e` and add:

   ```
   17 3 * * * /opt/homebrew/bin/certbot renew -q --deploy-hook "/opt/homebrew/bin/nginx -s reload"
   ```

   (On an Intel Mac, `/opt/homebrew` is `/usr/local`.)

5. From outside your network, check that a password is required:

   ```sh
   curl -s -o /dev/null -w '%{http_code}\n' https://photos.example.com   # expect 401 or 303
   ```

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

## Settings

Catalogue-wide options are on the **Settings** page (top right, for admins;
for everyone the server admits when there are no accounts), or from the
server:

```sh
archivis settings                    # list them
archivis settings set burst_gap 5    # frames up to 5 s apart form one burst
archivis settings set scene_gap 20      # shots up to 20 min apart are one scene (0-120, default 10)
archivis settings set stack_bursts off   # show every shot on the Photos page
archivis settings set stack_hamming 6    # stack only shots more alike (0-16, default 10)
```

**Stacks.** On the Photos page, alike shots of one scene show as one card:
the best shot, with the number of shots on it. A scene is one camera's
shots, each taken within the scene gap (10 minutes) of the last, so a run
of shots of one view over several minutes, and the RAW and JPEG of each,
stack together, while a shot of something else in between stays apart. A
shot joins a stack when its perceptual hash is within `stack_hamming` bits
of the best shot's. The card's page lists the others. A filter that leaves
out a stack's best shot shows the other shots that match. Text search
shows each stack once. Set `scene_gap 0` to stack only frames of one burst,
as before; turn stacking off on the Settings page, or with
`stack_bursts off`.

Settings saved before scenes stacked kept the old similarity default, 6:
they now read as 10 until saved again. A similarity set to anything else
is kept.

### Bursts and identical images

The **Bursts** and **Duplicates** pages read groups that are worked out once
and stored, so they open quickly however large the catalogue. An index run
works them out when it finishes. A running `archivis serve` does it when it
starts, at once after the burst gap changes, and otherwise at most every 30
minutes while new photos arrive. On a first run this takes about a minute
per million photos, in the background. While it runs, the pages say so and
show the previous grouping. Pages keep working, and naming faces is not
held up.

```sh
archivis groups              # up to date, or not
archivis groups rebuild      # work them out now
```

### Albums

**Albums** browses your photos by folder. It starts from the folders you
index, plus one **iPhone** album that holds every Apple Photos library.
Open a folder to see its subfolders at the top. Below them is every photo
in that folder and in all its subfolders. These photos have the Photos
page's filters, sorts and search.

Folder counts and covers (each folder's best photo) are worked out along
with the bursts, at the same times. That takes about 5 s per million
photos, and a run with nothing changed writes nothing. Until the first run
finishes, the page says so.

Each index run records its folders. For folders indexed before Albums
existed, add them once:

```sh
archivis roots add /Volumes/Archive ~/Pictures
archivis roots                       # the folders Albums starts from
archivis roots remove /Volumes/Old   # stop showing one; its photos stay catalogued
```

Quote a path that has spaces (`"/Volumes/G-RAID Shuttle XL/Sync"`). Without
quotes, the shell splits it into several folders.

**Names.** A top-level album can show a name of your choosing instead of
its folder's name. Admins set it under **Album names** on the Albums page.
Without accounts, anyone the server admits can. From the command line:

```sh
archivis roots name "/Volumes/G-RAID Shuttle XL/Sync/_photo_stage" "Photo stage"
archivis roots name iphone "Phones"                    # the iPhone album
archivis roots name "/Volumes/G-RAID Shuttle XL/Sync/_photo_stage" ""   # back to the folder's name
```

The folder's path stays visible on the album's page and when you hover over the card.

**Order.** Album cards sort by name (the default), by **newest photo**, or
by **average score**. Newest photo uses the latest date of any photo in the
folder or its subfolders. Average score is the mean overall score of the
photos below that have one. Undated and unscored albums go last. Each card
shows both figures. The chosen order carries into subfolders, and it is
separate from the sort of the photos below.

After upgrading, the folders are worked out once more in the background,
to add those dates and scores.

**Filters on the album list.** The Albums page has the Photos page's filters
and sorts. With any filter set (beyond leaving memes out) or a photo sort
chosen, the list shows only the albums that hold matching photos. Each card
shows its best match, and with a photo sort the cards follow that photo's
order; without one, "Albums by" still orders them. An album opened from a
filtered list opens filtered, and its subfolders are narrowed the same way.
The way back (the "Albums" link and the folders above) returns to the
filtered list, even after you change or clear the filters inside the album.

### Stars

Star a photo you like with the star on its card, or beside its path on the
photo's page. **Starred** (in the menu) lists your starred photos, and the
**Starred only** filter narrows any page to them, Albums included.

With accounts, each account's stars are its own: no one else sees them, and
viewers can star too. Without accounts, everyone the server admits shares
one list.

### Memes and screenshots

Pages leave out memes and screenshots unless you ask for them. The
**Source** filter offers:
- no memes or screenshots (the default);
- everything;
- camera photos;
- saved images;
- memes & screenshots.

The heading says when memes are hidden. From the command line, use
`--source photos|camera|saved|memes`; without it, the command line lists
everything.

**How a photo's source is worked out:**
- A photo whose metadata names a camera is a **camera photo**, whatever it
  shows. A phone's own photos count too.
- Anything else (sent through a messaging app, downloaded, screenshotted)
  is a **saved image**, unless its CLIP embedding looks like a meme, a
  social media post, a news or sports graphic, a quote card or a screenshot.
  Then it is a **meme or screenshot**.

**Apple Photos libraries.** Inside an Apple Photos library, Archivis also
reads which photos Photos itself calls screenshots. It reads them from the
library's `database/Photos.sqlite`, opened read-only, and needs macOS 10.15
or later. Those photos are memes and screenshots whatever they look like.
Archivis checks this after each index run, when `archivis serve` starts,
and after `archivis retag`.

In a test with 126 made-up memes and screenshots and 80 real phone photos
stripped of their metadata, 115 of the memes were hidden and 2 of the photos
were. Hidden photos are never lost: choose "everything" or "saved images" to
see them.

New photos are sorted as they are indexed. Photos catalogued before
upgrading are sorted from their stored embeddings, with no re-analysis:
- by `archivis serve` when it starts, in the background;
- or by the next index run.

Until then they show. `archivis retag` sorts every photo again.

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

## Dependencies and fallbacks

Everything Archivis depends on has a copy in the auteursoft GitHub
organization. If an original disappears or changes, Archivis keeps
building and setting up.

- **Downloaded files.** These are the ONNX Runtime, the face models, the
  CLIP models and the camera test files. Each has a copy in the
  [`deps` release](https://github.com/auteursoft/archivis/releases/tag/deps).
  `archivis setup` tries the original first, then the copy, and checks
  every file against its pinned SHA-256 either way. So a missing original,
  or one replaced by something else, falls back on its own.
  `tools/samples/fetch.sh` does the same for the test files.
- **Repositories.** `tools/deps/repos.txt` lists each one and what it is
  used for. Each is forked into auteursoft by `tools/deps/fork.sh`. Run it
  once, and again from time to time to sync the forks and copy new
  version tags:

  ```sh
  tools/deps/fork.sh           # needs a GitHub CLI that may create repositories in auteursoft
  tools/deps/fork.sh --check   # which forks exist
  ```

- **Go modules.** proxy.golang.org keeps every published version even if
  its repository is deleted, so builds keep working. If you need the
  forks anyway:

  ```sh
  tools/deps/use-forks.sh                    # every module, at the versions go.mod pins
  tools/deps/use-forks.sh golang.org/x/image # or just one
  tools/deps/use-forks.sh --undo
  ```

- **GitHub Actions** used by CI. If one goes, change its `uses:` line to
  `auteursoft/<name>@<same version>`.

A weekly job (`.github/workflows/deps.yml`) keeps the copies complete. It
fails, naming what went, when an original file, repository or Go module
stops answering. So a loss shows up while the copies still have it. Each
copied file keeps its own licence, which the release notes list. The
InsightFace models are for non-commercial research use only.

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
