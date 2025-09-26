# frp4wg

A minimal frp-like UDP forwarder focused on a single WireGuard port, implementing the design in [design-draft.md](design-draft.md).

Goals:
- No extra auth/encryption (WireGuard handles security).
- Zero datagram overhead (preserve MTU).
- 0-RTT connection use after initial frpc-frps handshake.
- Small, known set of WireGuard clients.

Status: MVP implemented in a single binary with client/server subcommands, using Go's standard library and slog. nft offload is not included yet; code leaves clear extension points for future integration.

## Build

Requires Go toolchain compatible with the module directive in [go.mod](go.mod). Then:

```
go build ./cmd/frp4wg
```

This produces the executable `frp4wg` in the repository root when run as above.

## Usage

The binary provides two subcommands: client (c) and server (s).

Environment variable:
- `FRP4WG_LOG_LEVEL` = debug | info | warn | error (default: info)

### Server

Listens on a single UDP port (typically the public WireGuard port). Handles handshake and establishes active mappings.

Examples:
```
# Listen on default :51820 (WireGuard public port)
./frp4wg s

# Listen on a specific interface/port
./frp4wg s -bind 0.0.0.0:40000

# Tune housekeeping
./frp4wg s -standby-ttl 30s -active-timeout 2m -max-standby 128 -gc-interval 5s
```

Flags:
- `-bind` string: UDP bind address (default `:51820`)
- `-standby-ttl` duration: expire standby if no handshake seen within TTL (default `30s`)
- `-active-timeout` duration: idle timeout for active mappings (default `2m`)
- `-max-standby` int: maximum number of standby entries kept in FIFO (default `128`)
- `-gc-interval` duration: background GC sweep interval (default `5s`)
- `-read-buffer` int: socket read buffer size in bytes (default `65535`)

### Client

Maintains N standby connections from behind NAT toward the server. Upon receiving non-handshake traffic from server (forwarded WireGuard packet), promotes the specific standby to active and relays data between local WireGuard and the server endpoint.

Examples:
```
# Minimal: forward local WireGuard (127.0.0.1:51820) to server 1.2.3.4:51820
./frp4wg c -server 1.2.3.4:51820

# If your local WG is on a different port or IP
./frp4wg c -server 1.2.3.4:40000 -local 127.0.0.1:51821

# Tune handshake frequency and standby pool
./frp4wg c -server 1.2.3.4:51820 -standby 8 -handshake-interval 5s -handshake-tries 3
```

Flags:
- `-server` string: server UDP address (host:port), required
- `-local` string: local WireGuard UDP address to forward to (default `127.0.0.1:51820`)
- `-standby` int: number of standby UDP sockets to maintain (default `8`)
- `-handshake-interval` duration: interval between client handshakes (default `5s`)
- `-handshake-tries` int: missed handshake-replies before recreating standby (default `3`)
- `-active-timeout` duration: idle timeout for active relaying (default `2m`)
- `-read-buffer` int: socket read buffer size in bytes (default `65535`)

## How it works

- Handshake: 6-byte literal `frp4wg`. Both client and server use this to probe liveness. No additional framing is used.
- Standby (client):
  - Maintain N UDP sockets bound to :0 so the OS assigns ephemeral ports, establishing NAT pinholes.
  - Periodically send handshake to server and expect handshake reply.
  - If a non-handshake packet arrives from server, promote that socket to active immediately (0-RTT).
  - If several consecutive replies are missed, the standby socket is closed and replaced.
- Server:
  - Single UDP port for both handshake and data.
  - Handshake from a new src => record/refresh a standby entry and reply handshake.
  - Any non-handshake from an unknown src (e.g., incoming WireGuard packet) consumes the oldest standby (FIFO) to create an active mapping (client<->remote). The first packet is forwarded immediately (0-RTT).
  - Idle active mappings are garbage-collected.
- Active:
  - Pure UDP relay between two known endpoints (client socket and remote address). No extra headers, preserving MTU.
  - Liveness relies on WireGuard keepalives; frp4wg does not add keepalives while active.
- nft offload (future):
  - Hooks can be added to install nft rules upon activation and remove them on GC based on counters while retaining the owning UDP socket.

## Typical topology

- Server has a public IP and runs WireGuard on a UDP port (e.g., 51820). Run `./frp4wg s -bind :51820` on the same machine; both share the same port.
- Client is behind NAT where inbound UDP to WireGuard is blocked. Run `./frp4wg c -server your.server.ip:51820 -local 127.0.0.1:51820`.
- WireGuard on client continues to talk to its local UDP endpoint. frp4wg relays between local WireGuard and the server.

## Notes

- Ensure WireGuard itself is configured normally on both ends. frp4wg does not alter WG configs.
- No encryption/authentication is added by frp4wg; it forwards raw datagrams. WireGuard provides confidentiality and integrity.
- For best results, keep `-standby` large enough to cover expected concurrent initial handshakes when multiple clients might start simultaneously.
- Logging uses Go's `slog` with a text handler. Set `FRP4WG_LOG_LEVEL=debug` to aid troubleshooting.

## Source

- Main implementation: [cmd/frp4wg/main.go](cmd/frp4wg/main.go)
- Design document: [design-draft.md](design-draft.md)
- Module: [go.mod](go.mod)
- License: [LICENSE](LICENSE)