# Design

frp4wg a minimal frp-like application that aims to forward one single wireguard udp port from a client behind NAT to a public server. Aim:
1. no auth/encryption needed because wireguard will handle it.
2. zero datagram size overhead so that wireguard MTU do not decrease.
3. 0-RTT new connection establishment after initial frpc-frps handshake
4. only a small known number of clients will connect to wireguard.

This is my design:

At the core, we maintain M + N connections from frpc to frps, where M connections are active and N, a constant, connections are standby. one connection is a state machine.

frpc connection:

"standby" connections are created when the number of connections in this state is less than N. :0 is used in listen packet to let os allocate a port.

standby:
- send "handshake" to the frps port periodically and expect "handshake reply"
- if no "handshake reply" is received several times, close the connection.
- if something other than "handshake reply", i.e. wireguard handshake, is received from the server address, go to "active state"

active:
- relays data between two known addr-ports.
- if the connection is idle for some time, close it.
- no active keepalive by frpc/frps because wireguard will do it.
- use golang listener to track liveness, but
- when enabled, use nft:
    - forward datagrams with golang first because this take effect immediately
    - create a nft rule to do faster forward with counters
    - periodically check and cleanup connections if counters does not increase. a rough check is enough and use only one global checker for efficiency.
    - still occupy the UDPConn to prevent it being used by other programs

to close the connection simply mean removing it. no close message.

frps:

Listen on one single port only.

When a new addr-port connects to the port, and the datagram is a "handshake", start a new standby connection. If the data is not handshake, consume a standby connection FIFO. if there is none, drop.

standby:
- respond to handshake
- if no handshake arrive for some time, close the connection.
- if the connection is used, forward the data and go to active state.

active:
- use golang and optionally nft, like frpc.

data scheme:
both handshake and handshake reply message is "frp4wg" 6-byte sequence.

To avoid exe name conflict, do not use frpc nor frps. call it 'frp4wg'. usage is `frp4wg c[lient]` or `frp4wg s[erver]`.

Implementation note:
use the latest slog package.