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
    <string>/Volumes/G-RAID\ Shuttle\ XL/Sync/_photo_stage/</string>
    <string>/Volumes/super_40/gDrives-sean.goggins/Google\ Photos/</string>
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