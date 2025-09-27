#!/usr/bin/env bash
# Quick local loopback performance test orchestrator
# Pipeline modeled after user's manual steps:
#   1) iperf3 -s
#   2) ./frp4wg s -bind :50000
#   3) ./frp4wg c -server localhost:50000 -local 127.0.0.1:5201
#   4) socat TCP4-LISTEN:50000,fork,reuseaddr TCP4:127.0.0.1:5201
#   5) iperf3 -c 127.0.0.1 -p 50000 -u -b <BW>
#
# Notes on roles:
# - socat forwards the TCP control channel from 127.0.0.1:50000 -> 127.0.0.1:5201 (iperf3 server control)
# - frp4wg is presumed to carry UDP payload path bind :50000 -> local 127.0.0.1:5201
#   This matches the provided command sequence for UDP tests with iperf3.
#
# Requirements:
# - iperf3, socat present in PATH
# - ./frp4wg (will be built automatically if missing and Go toolchain available)
#
# Usage examples:
#   scripts/loopback_perf.sh --help
#   scripts/loopback_perf.sh -b "200M,500M,1G" -t 10
#   scripts/loopback_perf.sh -b 100M -b 200M -b 300M --msg-len 1200
#   scripts/loopback_perf.sh --bind-port 50000 --server-port 5201 -t 15 -b 1G
#
# Exit codes:
#   0 success
#   1 usage or dependency error
#   2 runtime orchestration error

set -Eeuo pipefail

# ------------- Defaults -------------
BWS=()                       # If empty after parsing, defaults will be applied
TEST_DURATION=10             # seconds
IPERF_PORT=5201              # iperf3 server port
BIND_PORT=50000              # exposed test port for client
MSG_LEN=1200                 # iperf3 UDP payload length in bytes
NO_BUILD=0                   # 1=skip building frp4wg
NO_SOCAT=0                   # 1=do not run socat TCP forwarder
NO_FRP=0                     # 1=do not run frp4wg server/client
VERBOSE=0                    # 1=verbose output
KEEP_LOGS=0                  # 1=keep temp logs
LIVE=0                       # 1=stream iperf3 output live (auto-enabled with -v)
EXTRA_TIMEOUT=5              # extra seconds over TEST_DURATION to enforce iperf3 timeout
# ------------------------------------

# ------------- Globals --------------
TMP_DIR=""
IPERF_SRV_PID=""
SOCAT_PID=""
FRP_S_PID=""
FRP_C_PID=""
# ------------------------------------

log() { printf '[%(%Y-%m-%dT%H:%M:%S%z)T] %s\n' -1 "$*"; }
vlog() { if [[ "$VERBOSE" -eq 1 ]]; then log "$@"; fi; }
err() { printf 'ERROR: %s\n' "$*" >&2; }

cleanup() {
  local code=$?
  trap - EXIT INT TERM

  if [[ -n "${SOCAT_PID:-}" ]] && kill -0 "$SOCAT_PID" 2>/dev/null; then
    vlog "Stopping socat PID=$SOCAT_PID"
    kill "$SOCAT_PID" 2>/dev/null || true
    sleep 0.2 || true
    kill -9 "$SOCAT_PID" 2>/dev/null || true
  fi

  if [[ -n "${FRP_C_PID:-}" ]] && kill -0 "$FRP_C_PID" 2>/dev/null; then
    vlog "Stopping frp4wg client PID=$FRP_C_PID"
    kill "$FRP_C_PID" 2>/dev/null || true
    sleep 0.2 || true
    kill -9 "$FRP_C_PID" 2>/dev/null || true
  fi

  if [[ -n "${FRP_S_PID:-}" ]] && kill -0 "$FRP_S_PID" 2>/dev/null; then
    vlog "Stopping frp4wg server PID=$FRP_S_PID"
    kill "$FRP_S_PID" 2>/dev/null || true
    sleep 0.2 || true
    kill -9 "$FRP_S_PID" 2>/dev/null || true
  fi

  if [[ -n "${IPERF_SRV_PID:-}" ]] && kill -0 "$IPERF_SRV_PID" 2>/dev/null; then
    vlog "Stopping iperf3 server PID=$IPERF_SRV_PID"
    kill "$IPERF_SRV_PID" 2>/dev/null || true
    sleep 0.2 || true
    kill -9 "$IPERF_SRV_PID" 2>/dev/null || true
  fi

  if [[ "$KEEP_LOGS" -eq 0 && -n "${TMP_DIR:-}" && -d "$TMP_DIR" ]]; then
    rm -rf "$TMP_DIR" || true
  fi

  if [[ $code -ne 0 ]]; then
    err "Exited with code $code"
  fi
  exit $code
}

trap cleanup EXIT INT TERM

usage() {
  cat <<EOF
Loopback UDP throughput test orchestrator

Options:
  -b, --bandwidth BW       Bandwidth(s) to test. Repeatable or comma-separated list.
                           Examples: -b 500M -b 1G   OR   -b "200M,500M,1G"
  -t, --time SECONDS       Test duration per bandwidth (default: ${TEST_DURATION})
      --server-port PORT   iperf3 server port (default: ${IPERF_PORT})
      --bind-port PORT     Public bind/target port for tests (default: ${BIND_PORT})
  -l, --msg-len BYTES      iperf3 UDP payload length (default: ${MSG_LEN})
      --no-build           Do not attempt to build ./frp4wg if missing
      --no-socat           Do not run socat TCP forwarder (expect TCP control handled elsewhere)
      --no-frp             Do not run frp4wg server/client
      --keep-logs          Keep temporary output logs
      --live               Show iperf3 client output live (disables JSON mode); auto-enabled with -v
  -v, --verbose            Verbose logging
  -h, --help               Show this help

Defaults if no -b provided: 100M,200M,500M,1G

Examples:
  scripts/loopback_perf.sh -b "200M,500M,1G" -t 10
  scripts/loopback_perf.sh -b 100M -b 200M --msg-len 1200
  scripts/loopback_perf.sh --bind-port 50000 --server-port 5201 -t 15 -b 1G
EOF
}

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      -b|--bandwidth)
        if [[ $# -lt 2 ]]; then err "Missing value for $1"; usage; exit 1; fi
        IFS=',' read -r -a _bws <<< "$2"
        for bw in "${_bws[@]}"; do
          [[ -n "$bw" ]] && BWS+=("$bw")
        done
        shift 2
        ;;
      -t|--time)
        TEST_DURATION="$2"; shift 2;;
      --server-port)
        IPERF_PORT="$2"; shift 2;;
      --bind-port)
        BIND_PORT="$2"; shift 2;;
      -l|--msg-len|--len)
        MSG_LEN="$2"; shift 2;;
      --no-build)
        NO_BUILD=1; shift;;
      --no-socat)
        NO_SOCAT=1; shift;;
      --no-frp)
        NO_FRP=1; shift;;
      --keep-logs)
        KEEP_LOGS=1; shift;;
      --live)
        LIVE=1; shift;;
      -v|--verbose)
        VERBOSE=1; LIVE=1; shift;;
      -h|--help)
        usage; exit 0;;
      *)
        err "Unknown argument: $1"
        usage; exit 1;;
    esac
  done

  if [[ ${#BWS[@]} -eq 0 ]]; then
    BWS=(100M 200M 500M 1G)
  fi
}

require_cmd() {
  local c="$1"
  if ! command -v "$c" >/dev/null 2>&1; then
    err "Required command not found: $c"
    exit 1
  fi
}

check_dependencies() {
  require_cmd iperf3
  if [[ "$NO_SOCAT" -eq 0 ]]; then
    require_cmd socat
  fi

  if [[ "$NO_FRP" -eq 0 ]]; then
    if [[ ! -x "./frp4wg" ]]; then
      if [[ "$NO_BUILD" -eq 1 ]]; then
        err "./frp4wg is missing and --no-build specified"
        exit 1
      fi
      if command -v go >/dev/null 2>&1; then
        log "Building ./frp4wg from ./cmd/frp4wg ..."
        GO111MODULE=on go build -o ./frp4wg ./cmd/frp4wg
      else
        err "./frp4wg not found and Go toolchain unavailable to build"
        exit 1
      fi
    fi
  fi
}

with_timeout() {
  # with_timeout SECONDS command [args...]
  local deadline="$1"; shift
  if command -v timeout >/dev/null 2>&1; then
    timeout -k 2 "${deadline}s" "$@"
  else
    # Fallback: run command and kill after deadline
    "$@" &
    local cmd_pid=$!
    (
      sleep "$deadline"
      if kill -0 "$cmd_pid" 2>/dev/null; then
        kill -s INT "$cmd_pid" 2>/dev/null || true
        sleep 2
        kill -s KILL "$cmd_pid" 2>/dev/null || true
      fi
    ) &
    wait "$cmd_pid" || true
  fi
}

wait_for_tcp_port() {
  local host="$1" port="$2" timeout_s="${3:-5}"
  local start ts
  start=$(date +%s)
  while true; do
    if (exec 3</dev/tcp/"$host"/"$port") 2>/dev/null; then
      exec 3>&- 2>&-
      return 0
    fi
    ts=$(date +%s)
    if (( ts - start >= timeout_s )); then
      return 1
    fi
    sleep 0.1
  done
}

start_iperf_server() {
  vlog "Starting iperf3 server on 127.0.0.1:${IPERF_PORT}"
  iperf3 -s -p "${IPERF_PORT}" >/dev/null 2>&1 &
  IPERF_SRV_PID=$!
  if ! wait_for_tcp_port 127.0.0.1 "${IPERF_PORT}" 5; then
    err "iperf3 server did not become ready on port ${IPERF_PORT}"
    exit 2
  fi
}

start_socat() {
  if [[ "$NO_SOCAT" -eq 1 ]]; then
    vlog "Skipping socat per --no-socat"
    return
  fi
  vlog "Starting socat TCP forwarder: 127.0.0.1:${BIND_PORT} -> 127.0.0.1:${IPERF_PORT}"
  socat TCP4-LISTEN:"${BIND_PORT}",fork,reuseaddr TCP4:127.0.0.1:"${IPERF_PORT}" >/dev/null 2>&1 &
  SOCAT_PID=$!
  if ! wait_for_tcp_port 127.0.0.1 "${BIND_PORT}" 5; then
    err "socat TCP listener did not become ready on port ${BIND_PORT}"
    exit 2
  fi
}

start_frp() {
  if [[ "$NO_FRP" -eq 1 ]]; then
    vlog "Skipping frp4wg per --no-frp"
    return
  fi
  vlog "Starting frp4wg server: ./frp4wg s -bind :${BIND_PORT}"
  ./frp4wg s -bind ":${BIND_PORT}" >/dev/null 2>&1 &
  FRP_S_PID=$!

  # Give server a moment (UDP bind, can't port-probe easily)
  sleep 0.8

  vlog "Starting frp4wg client: ./frp4wg c -server localhost:${BIND_PORT} -local 127.0.0.1:${IPERF_PORT}"
  ./frp4wg c -server "localhost:${BIND_PORT}" -local "127.0.0.1:${IPERF_PORT}" >/dev/null 2>&1 &
  FRP_C_PID=$!

  # Brief stabilization wait
  sleep 0.5
}

human_bps() {
  # Convert bits/sec to human-readable
  local bps="$1"
  awk -v b="$bps" 'BEGIN{
    u="bits/sec";
    if (b>=1e9){ printf "%.2f G%s", b/1e9,u; }
    else if (b>=1e6){ printf "%.2f M%s", b/1e6,u; }
    else if (b>=1e3){ printf "%.2f K%s", b/1e3,u; }
    else { printf "%.0f %s", b,u; }
  }'
}

run_one_test_json() {
  local bw="$1" out_json="$2"
  local deadline=$((TEST_DURATION + EXTRA_TIMEOUT))
  with_timeout "$deadline" iperf3 -J -c 127.0.0.1 -p "${BIND_PORT}" -u -b "${bw}" -l "${MSG_LEN}" -t "${TEST_DURATION}" >"$out_json" 2>&1 || true
}

parse_json_summary() {
  local out_json="$1"
  local ok=0
  if command -v jq >/dev/null 2>&1; then
    # Validate minimal structure
    if jq -e '.end.sum.receiver|.!=null' "$out_json" >/dev/null 2>&1; then
      local bps jitter lost pkts loss_pct
      bps=$(jq -r '.end.sum.receiver.bits_per_second' "$out_json" 2>/dev/null || echo "")
      jitter=$(jq -r '.end.sum.receiver.jitter_ms' "$out_json" 2>/dev/null || echo "")
      lost=$(jq -r '.end.sum.receiver.lost_packets' "$out_json" 2>/dev/null || echo "")
      pkts=$(jq -r '.end.sum.receiver.packets' "$out_json" 2>/dev/null || echo "")
      if [[ -n "$bps" && -n "$pkts" && "$pkts" != "0" && "$bps" != "null" ]]; then
        loss_pct=$(awk -v l="$lost" -v p="$pkts" 'BEGIN{ if(p==0){print "0"} else {printf "%.3f", (l*100.0)/p} }')
        printf '%s\t%s\t%s\t%s\t%s\n' "$bps" "$jitter" "$lost" "$pkts" "$loss_pct"
        ok=1
      fi
    fi
  fi
  return $(( ok==1 ? 0 : 1 ))
}

run_one_test_text() {
  local bw="$1" out_txt="$2"
  local deadline=$((TEST_DURATION + EXTRA_TIMEOUT))
  with_timeout "$deadline" iperf3 -c 127.0.0.1 -p "${BIND_PORT}" -u -b "${bw}" -l "${MSG_LEN}" -t "${TEST_DURATION}" >"$out_txt" 2>&1 || true
}

run_one_test_text_live() {
  local bw="$1" out_txt="$2"
  local deadline=$((TEST_DURATION + EXTRA_TIMEOUT))

  : > "$out_txt"
  local args=(iperf3 -c 127.0.0.1 -p "${BIND_PORT}" -u -b "$bw" -l "${MSG_LEN}" -t "${TEST_DURATION}")
  echo "cmd: ${args[*]} (deadline: ${deadline}s)"

  if command -v stdbuf >/dev/null 2>&1; then
    stdbuf -oL -eL "${args[@]}" >>"$out_txt" 2>&1 &
  else
    "${args[@]}" >>"$out_txt" 2>&1 &
  fi
  local iperf_pid=$!

  # tail output live until iperf exits
  if command -v tail >/dev/null 2>&1; then
    tail -f --pid="$iperf_pid" "$out_txt" &
    local tail_pid=$!
  fi

  # enforce deadline
  (
    sleep "$deadline"
    if kill -0 "$iperf_pid" 2>/dev/null; then
      kill -s INT "$iperf_pid" 2>/dev/null || true
      sleep 2
      kill -s KILL "$iperf_pid" 2>/dev/null || true
    fi
  ) &
  local killer=$!

  wait "$iperf_pid" || true
  [[ -n "${tail_pid:-}" ]] && kill "$tail_pid" 2>/dev/null || true
  kill "$killer" 2>/dev/null || true
}

parse_text_summary() {
  local out_txt="$1"
  # Try to parse the receiver summary line
  local line
  line=$(grep -E 'receiver$' "$out_txt" | tail -n 1 || true)
  if [[ -z "$line" ]]; then
    # Fallback to last non-empty line
    line=$(grep -E '.+' "$out_txt" | tail -n 1 || true)
  fi
  if [[ -z "$line" ]]; then
    return 1
  fi

  local rate jitter loss_pair lost total loss_pct

  rate=$(echo "$line" | grep -Eo '[0-9.]+ [KMG]bits/sec' | head -n1 || true)
  jitter=$(echo "$line" | grep -Eo '[0-9.]+ ms' | head -n1 | awk '{print $1}' || true)
  loss_pair=$(echo "$line" | grep -Eo '[0-9]+/[0-9]+ \([0-9.]+%\)' | head -n1 || true)

  if [[ -n "$loss_pair" ]]; then
    lost=${loss_pair%%/*}
    local rest=${loss_pair#*/}
    total=${rest%% *}
    loss_pct=$(echo "$loss_pair" | grep -Eo '\([0-9.]+%\)' | tr -d '()%')
  else
    lost="N/A"; total="N/A"; loss_pct="N/A"
  fi

  # Convert rate to bits/sec numeric if possible for uniformity
  local bps_num=""
  if [[ -n "$rate" ]]; then
    # split: value unit
    local val unit
    val=$(echo "$rate" | awk '{print $1}')
    unit=$(echo "$rate" | awk '{print $2}')
    case "$unit" in
      Kbits/sec) bps_num=$(awk -v v="$val" 'BEGIN{printf "%.0f", v*1e3}') ;;
      Mbits/sec) bps_num=$(awk -v v="$val" 'BEGIN{printf "%.0f", v*1e6}') ;;
      Gbits/sec) bps_num=$(awk -v v="$val" 'BEGIN{printf "%.0f", v*1e9}') ;;
      bits/sec)  bps_num=$(awk -v v="$val" 'BEGIN{printf "%.0f", v}') ;;
      *) bps_num="";;
    esac
  fi

  # Output: bps jitter lost total loss_pct
  printf '%s\t%s\t%s\t%s\t%s\n' "${bps_num:-}" "${jitter:-}" "${lost:-}" "${total:-}" "${loss_pct:-}"
}

main() {
  parse_args "$@"
  check_dependencies

  TMP_DIR=$(mktemp -d -t loopback-perf.XXXXXXXX)
  vlog "Using temp dir: $TMP_DIR"

  start_iperf_server
  start_frp
  start_socat

  local have_jq=0
  if command -v jq >/dev/null 2>&1; then have_jq=1; fi

  # Results arrays
  declare -a RQ_BW=()      # requested bw
  declare -a RX_BPS=()     # receiver bits/sec numeric
  declare -a RX_JITTER=()
  declare -a RX_LOST=()
  declare -a RX_PKTS=()
  declare -a RX_LOSS_PCT=()

  # Use JSON parsing only when jq is available and LIVE is disabled
  local use_json=0
  if [[ $have_jq -eq 1 && $LIVE -eq 0 ]]; then
    use_json=1
  fi

  for bw in "${BWS[@]}"; do
    log "Running iperf3 UDP test: -b ${bw} -l ${MSG_LEN} -t ${TEST_DURATION}"
    local out="${TMP_DIR}/iperf_${bw//\//_}.out"
    local bps jitter lost pkts lpct

    if [[ $use_json -eq 1 ]]; then
      run_one_test_json "$bw" "$out"
      if parse_json_summary "$out" > "${out}.parse" 2>/dev/null; then
        read -r bps jitter lost pkts lpct < "${out}.parse"
        RX_BPS+=("$bps"); RX_JITTER+=("$jitter"); RX_LOST+=("$lost"); RX_PKTS+=("$pkts"); RX_LOSS_PCT+=("$lpct")
      else
        # Fallback to text mode if JSON parsing failed
        run_one_test_text "$bw" "$out"
        if parse_text_summary "$out" > "${out}.parse" 2>/dev/null; then
          read -r bps jitter lost pkts lpct < "${out}.parse"
          RX_BPS+=("${bps:-}"); RX_JITTER+=("${jitter:-}"); RX_LOST+=("${lost:-}"); RX_PKTS+=("${pkts:-}"); RX_LOSS_PCT+=("${lpct:-}")
        else
          RX_BPS+=(""); RX_JITTER+=(""); RX_LOST+=(""); RX_PKTS+=(""); RX_LOSS_PCT+=("")
        fi
      fi
    else
      if [[ $LIVE -eq 1 ]]; then
        run_one_test_text_live "$bw" "$out"
      else
        run_one_test_text "$bw" "$out"
      fi
      if parse_text_summary "$out" > "${out}.parse" 2>/dev/null; then
        read -r bps jitter lost pkts lpct < "${out}.parse"
        RX_BPS+=("${bps:-}"); RX_JITTER+=("${jitter:-}"); RX_LOST+=("${lost:-}"); RX_PKTS+=("${pkts:-}"); RX_LOSS_PCT+=("${lpct:-}")
      else
        RX_BPS+=(""); RX_JITTER+=(""); RX_LOST+=(""); RX_PKTS+=(""); RX_LOSS_PCT+=("")
      fi
    fi

    RQ_BW+=("$bw")
  done

  printf "\nSummary (receiver metrics):\n"
  printf "%-10s  %-14s  %-10s  %-12s  %-12s\n" "ReqBW" "RxRate" "Jitter(ms)" "Lost/Total" "Loss(%)"
  printf "%-10s  %-14s  %-10s  %-12s  %-12s\n" "------" "------" "----------" "----------" "-------"

  local idx=0 n=${#RQ_BW[@]}
  local first_loss_idx=""
  for ((idx=0; idx<n; idx++)); do
    local bw="${RQ_BW[$idx]}"
    local bps="${RX_BPS[$idx]}"
    local jitter="${RX_JITTER[$idx]}"
    local lost="${RX_LOST[$idx]}"
    local pkts="${RX_PKTS[$idx]}"
    local lpct="${RX_LOSS_PCT[$idx]}"

    local rate_h="N/A"
    if [[ -n "$bps" && "$bps" != "null" ]]; then
      rate_h=$(human_bps "$bps")
    fi
    local lt="N/A"
    if [[ -n "$lost" && -n "$pkts" && "$lost" != "null" && "$pkts" != "null" ]]; then
      lt="${lost}/${pkts}"
      if [[ -z "$first_loss_idx" ]] && [[ "$lost" =~ ^[0-9]+$ ]] && (( lost > 0 )); then
        first_loss_idx="$idx"
      fi
    fi
    printf "%-10s  %-14s  %-10s  %-12s  %-12s\n" "$bw" "$rate_h" "${jitter:-N/A}" "$lt" "${lpct:-N/A}"
  done

  if [[ -n "$first_loss_idx" ]]; then
    printf "\nFirst observed packet loss at requested bandwidth: %s\n" "${RQ_BW[$first_loss_idx]}"
  else
    printf "\nNo packet loss observed across tested bandwidths.\n"
  fi
}

main "$@"