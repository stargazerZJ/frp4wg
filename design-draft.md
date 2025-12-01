# Design

frp4wg a minimal frp-like application that aims to forward one single wireguard udp port from a client behind NAT to a public server. Aim:
1. no auth/encryption needed because wireguard will handle it.
2. zero datagram size overhead so that wireguard MTU do not decrease.
3. 0-RTT new connection establishment after initial frpc-frps handshake
4. only a small known number of clients will connect to wireguard.
5. on the frpc machine the wireguard 'server' is run, and multiple wireguard 'client's will connect to the frps port from different addresses.

This is my design:

At the core, we maintain M + N connections from frpc to frps, where M connections are active and N, a constant, connections are standby. one connection is a state machine.

frpc connection:

"standby" connections are created when the number of connections in this state is less than N. :0 is used in listen packet to let os allocate a port.

standby:
- send "handshake" to the frps port periodically and expect "handshake reply"
- if no "handshake reply" is received several times, close the connection.
- if something other than "handshake reply", i.e. wireguard handshake, is received from the server address, go to "active state"

active:
- relays data between two known addr-ports. i.e. if the packet is from A send to B, if it's from B send to A.
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

When a new addr-port connects to the port, and the datagram is a "handshake", start a new standby connection. If the data is not handshake, consume the standby connection that sends the most recent handshake. if there is none, drop.

standby:
- respond to handshake
- if no handshake arrive for some time, close the connection.
- if the connection is used, forward the data and go to active state.

active:
- use golang and optionally nft, like frpc.

data scheme:
both handshake and handshake reply message is "frp4wg" 6-byte sequence.

parameters and defaults:
frpc: 12s handshake period, 1 handshake failure results in standby connection close, 2 standby connections. up to 8 active connections
frps: track up to 4 standby connections.
both: up to 8 active connections. 60s active connection idle timeout.

fs := flag.NewFlagSet("client", flag.ExitOnError)
server := fs.String("server", "", "frps UDP address (host:port)")
local := fs.String("local", "127.0.0.1:51820", "local WireGuard UDP address to forward to")
standbyN := fs.Int("standby", 8, "number of standby connections to maintain")
hsInterval := fs.Duration("handshake-interval", 5*time.Second, "interval between client handshakes")
hsTries := fs.Int("handshake-tries", 3, "max consecutive missed handshake replies before recreating standby")
activeIdle := fs.Duration("active-timeout", 2*time.Minute, "idle timeout for active connection")
readBuf := fs.Int("read-buffer", 65535, "read buffer size in bytes")

fs := flag.NewFlagSet("server", flag.ExitOnError)
bind := fs.String("bind", ":51820", "bind UDP address for frps (and WireGuard public port)")
standbyTTL := fs.Duration("standby-ttl", 30*time.Second, "expire standby if no handshake within this duration")
activeIdle := fs.Duration("active-timeout", 2*time.Minute, "idle timeout for active mapping")
maxStandby := fs.Int("max-standby", 128, "max standby entries to keep (FIFO)")
gcEvery := fs.Duration("gc-interval", 5*time.Second, "background GC check interval")
readBuf := fs.Int("read-buffer", 65535, "read buffer size in bytes")
_ = fs.Parse(os.Args[2:])

To avoid exe name conflict, do not use frpc nor frps. call it 'frp4wg'. usage is `frp4wg c[lient]` or `frp4wg s[erver]`.


testing and benchmarking:

1) iperf3 -s
2) ./frp4wg s -bind :50000
3) ./frp4wg c -server localhost:50000 -local 127.0.0.1:5201
4) socat TCP4-LISTEN:50000,fork,reuseaddr TCP4:127.0.0.1:5201
5) iperf3 -c 127.0.0.1 -p 50000 -u -b <BW>