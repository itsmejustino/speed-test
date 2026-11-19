# Speed Test

A Windows speed test for network engineers. A native Go engine measures your connection against Cloudflare's speed test service, and a local web page shows the results.

## What it measures

- **Download and upload:** steady-state throughput over several TCP connections, after warm-up.
- **Ping and jitter:** idle, and while the line is busy (bufferbloat).
- **Packet loss:** lost pings, packets the server resent, and packets Windows resent.
- **Route:** each hop to the server, using ICMP with rising TTL.
- **Distance:** how far you are from the server, and the fastest round trip physics allows.
- **This computer:** the adapter in use (Ethernet, Wi-Fi, VPN), its link speed, local IP, gateway, DNS servers and MTU.
- **Connection setup:** DNS, TCP and TLS time for the first connection.
- **Findings:** what stands out, and where to look: a link capping the speed, a 100 Mbps Ethernet link, loss on a quiet line, bufferbloat, slow DNS.
- **Comparison (optional):** how you rank against other people in your area, using Cloudflare Radar.

**Copy report** puts a plain-text summary of any test on the clipboard, ready for a ticket or a chat.

## Web version

**[itsmejustino.github.io/speed-test](https://itsmejustino.github.io/speed-test/)** runs the same test in your browser, with nothing to install. It measures download, upload, idle and loaded latency, jitter, and the packets Cloudflare's server had to resend. Route tracing, ICMP pings, Windows' TCP counters and the comparison with other users need the desktop app. Settings and history stay in your browser.

## Run it

1. Download `SpeedTest.exe` from the [latest release](../../releases/latest).
2. Run it. Your browser opens `http://localhost:8765/`.
3. Press **Start test**.

Windows SmartScreen may warn you because the program isn't code-signed. Choose **More info › Run anyway**.

Settings and history are saved next to the program in `settings.json` and `history.json`.

### Options

| Flag | Default | Purpose |
|---|---|---|
| `-port` | `8765` | Port for the local page |
| `-no-browser` | off | Don't open the browser |
| `-data` | program folder | Where settings and history are kept |
| `-web` | built in | Serve `index.html` from a folder (for development) |
| `-run` | off | Run one test in the console and print the result |
| `-json` | off | With `-run`: print JSON |
| `-seconds` | from settings | With `-run`: seconds each way (5–30) |

### Command line

For scripts and remote-management tools, run one test without the page:

```
SpeedTest.exe -run                 summary for people
SpeedTest.exe -run -json           the whole result as JSON
SpeedTest.exe -run -seconds 10     10 seconds each way
```

Progress goes to stderr and the result to stdout. The exit code is 0 when the test finished, 1 when it failed. Results are also saved to the history.

### Comparing with other users (optional)

1. In the Cloudflare dashboard, create an API token with **Account › Radar › Read**.
2. Paste it into **Settings** and press **Check token**.

The token is stored only in your local `settings.json`.

## Build from source

You need [Go](https://go.dev/dl/) 1.24 or later.

```sh
go test -race ./...
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o SpeedTest.exe .
```

The page in `web/index.html` is embedded into the program when it's built.

Each push is tested by GitHub Actions. Pushing a tag such as `v1.0.0` builds `SpeedTest.exe` and attaches it to a release.

## License

GNU General Public License v3.0. See [LICENSE](LICENSE).

## Trademarks

Cloudflare and Cloudflare Radar are trademarks and/or registered trademarks of Cloudflare, Inc. Cisco and CCNA are trademarks or registered trademarks of Cisco Systems, Inc. and/or its affiliates. This project is not affiliated with or endorsed by Cloudflare or Cisco.
