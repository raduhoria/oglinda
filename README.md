# oglinda

A small, self-contained looking glass for hosting providers.

- **ping** and **traceroute** from your server to any public address, streamed live.
  The target defaults to the visitor's own IP, so they see the route back to them.
- **Browser speed test**: latency, jitter, download and upload between the visitor's
  browser and your server. Nothing to install.
- Location and test IPs, with click-to-copy.
- English and Romanian interface.

*Oglindă* is Romanian for "mirror".

## Why another one

Most looking glasses are PHP scripts that call shell commands. oglinda is built to be
safe to run directly on a production host:

| | |
|---|---|
| No shell | Tools run through `exec` with fixed arguments. User input never reaches an argument list. |
| Resolve, then run | The target is parsed or resolved to an IP first; only that IP is passed to the tool, after `--`. Hostnames cannot inject options, and DNS cannot change between check and use. |
| Public targets only | Private, loopback, link-local, multicast, CGNAT, documentation, benchmarking and other reserved ranges are refused, for IPv4 and IPv6. You can block extra prefixes (`LG_BLOCK`). |
| No privileges | Runs as a systemd `DynamicUser` with an empty capability set. ping uses unprivileged ICMP sockets, traceroute uses UDP. |
| Bounded | Per-client token bucket (IPv6 grouped by /64), one job per client at a time, global job cap, hard timeouts, output line cap. Speed transfers have their own size and concurrency limits. |
| Honest client IP | `X-Forwarded-For` is trusted only from configured proxies, and only its right-most entry. |
| Strict browser policy | Strict CSP (`default-src 'none'`, no inline code), `nosniff`, no referrer, no framing. |
| Small | One static Go binary with the web UI embedded. Standard library only, no third-party dependencies. |
| No leaks | Internal hops (e.g. your provider's `10.x` routers) can be masked in traceroute output. |

## Requirements

- Linux with `ping` (iputils) and `traceroute`
- `net.ipv4.ping_group_range` covering all groups (the systemd default on modern
  distributions: `0 2147483647`)
- A reverse proxy for TLS (Caddy, nginx)

## Install

```sh
make build
sudo install -m 0755 oglinda /usr/local/bin/oglinda
sudo install -m 0644 deploy/oglinda.service /etc/systemd/system/
sudo install -m 0600 deploy/oglinda.env.example /etc/oglinda.env   # then edit it
sudo systemctl daemon-reload
sudo systemctl enable --now oglinda
```

Caddy:

```
lg.example.com {
	reverse_proxy 127.0.0.1:8090
}
```

## Configuration

Every option is an environment variable (see `deploy/oglinda.env.example`) or a flag
(`oglinda -h`).

| Variable | Default | |
|---|---|---|
| `LG_LISTEN` | `127.0.0.1:8090` | Listen address |
| `LG_TRUSTED_PROXY` | `127.0.0.1,::1` | Proxies whose `X-Forwarded-For` is trusted |
| `LG_BRAND`, `LG_BRAND_URL` | | Name and link in the header |
| `LG_LOCATION`, `LG_DATACENTER` | | Shown on the page |
| `LG_IPV4`, `LG_IPV6` | | Test addresses shown on the page |
| `LG_MASK_INTERNAL` | `true` | Mask non-public hops |
| `LG_BLOCK` | | Extra prefixes that may not be targeted |
| `LG_PER_MINUTE` | `6` | Network tests per client per minute |
| `LG_MAX_JOBS` | `4` | Concurrent network tests, all clients |
| `LG_SPEED_MAX_MB` | `100` | Largest speed test transfer, MiB |
| `LG_SPEED_JOBS` | `3` | Concurrent speed transfers, all clients |

## API

| | |
|---|---|
| `GET /api/info` | Server info and the caller's IP |
| `GET /api/run?tool=ping\|traceroute&family=4\|6&target=…` | Server-sent events: `target`, `line`…, then `done` or `error` |
| `GET /speed/ping` | Empty response, for RTT |
| `GET /speed/down?mb=N` | N MiB of random data |
| `POST /speed/up` | Discards the body, returns its size |

## License

MIT
