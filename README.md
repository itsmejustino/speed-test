# Speed Test

A Windows speed test for network engineers. A native Go engine measures your connection against Cloudflare's speed test service, and a local web page shows the results.

## What it measures

- **Download and upload:** steady-state throughput over several TCP connections, after warm-up.
- **Ping and jitter:** idle, and while the line is busy (bufferbloat).
- **Packet loss:** lost pings, packets the server resent, and packets Windows resent.
- **Route:** each hop to the server, using ICMP with rising TTL.
- **Distance:** how far you are from the server, and the fastest round trip physics allows.
- **Comparison (optional):** how you rank against other people in your area, using Cloudflare Radar.

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
