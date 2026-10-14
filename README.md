# codemowers/device-agent

Lightweight agent that connects a Linux device (Raspberry Pi, etc.) to a [codemowers/agent](https://github.com/codemowers/agent) controller.

The device initiates an outbound WebSocket connection — no inbound ports, no SSH required. Once connected it appears in the controller UI and can be toggled into any Task, giving the agent `shell`, `file_read` and `file_write` access to the device.

## Install

Download the binary for your architecture from [Releases](../../releases):

```sh
# Raspberry Pi 4/5 (arm64)
curl -fsSL https://github.com/codemowers/device-agent/releases/latest/download/device-agent-linux-arm64 \
  -o /usr/local/bin/device-agent && chmod +x /usr/local/bin/device-agent

# Raspberry Pi 3/4 32-bit (armv7)
curl -fsSL https://github.com/codemowers/device-agent/releases/latest/download/device-agent-linux-armv7 \
  -o /usr/local/bin/device-agent && chmod +x /usr/local/bin/device-agent
```

## Connect

Copy the endpoint URL from the agent UI (Devices → Add device) and run:

```sh
device-agent --endpoint wss://your-agent-host/devices/YOUR-KEY
```

The device reconnects automatically on disconnect. Run as a systemd service for persistence:

```sh
cat > /etc/systemd/system/device-agent.service << EOF
[Unit]
Description=Codemowers device agent
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/device-agent --endpoint wss://your-agent-host/devices/YOUR-KEY
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
systemctl enable --now device-agent
```

## Protocol

The device agent speaks a simple JSON protocol over WebSocket:

- On connect it sends `{"type":"hello","hostname":"...","version":"..."}`.
- The controller sends commands: `{"id":"r1","cmd":"shell","args":{"command":"..."}}`.
- The device replies: `{"id":"r1","ok":true,"stdout":"...","stderr":"...","exit_code":0}`.

Supported commands: `shell`, `file_read`, `file_write`.
