#!/usr/bin/env bash
# Runs spike cases against spike-envoy.yaml + harness (see research.md S1-S4).
t(){ local id=$RANDOM$RANDOM name=$1 upath=$2 modes=$3; shift 3
  local bodyf; bodyf=$(mktemp)
  local start end out code n
  start=$(date +%s.%N 2>/dev/null || perl -MTime::HiRes=time -e 'print time')
  out=$(curl -s -D - -o "$bodyf" -H "x-spike-id: $id" -H "x-modes: $modes" "$@" "127.0.0.1:19000$upath")
  end=$(perl -MTime::HiRes=time -e 'printf "%.2f", time')
  start=$(perl -MTime::HiRes=time -e 'print 0'); 
  code=$(echo "$out" | head -1 | awk '{print $2}')
  n=$(curl -s "127.0.0.1:19101/__count?id=$id")
  hv(){ echo "$out" | grep -i "^$1:" | cut -d' ' -f2- | tr -d '\r'; }
  printf "%-36s status=%s attempts=%s tag=%s flags=%s extproc=[%s] body=%s\n" "$name" "$code" "$n" "$(hv x-front-saw-tag)" "$(hv x-dispatch-saw-flags)" "$(hv x-extproc-seen)" "$(head -c 70 "$bodyf" | tr '\n' ' ')"
  rm -f "$bodyf"; }
timed(){ local s e; s=$(perl -MTime::HiRes=time -e 'print time'); "$@"; e=$(perl -MTime::HiRes=time -e 'print time'); perl -e "printf \"    elapsed=%.2fs\n\", $e-$s"; }
t "S1 ok (extproc both hops?)" / ok
t "S2a 429 tagged -> retry" / status:429,ok
t "S2b untagged 500 -> no retry" / status:500,ok
t "S2c non-eligible 502 -> no retry" / status:502,ok
t "S2d eligible 503 -> retry" / status:503,ok
timed t "S2e per-try timeout -> retry" / hang:3,ok
timed t "S2f stream (3s) longer than per-try" / stream:3
t "S2g all fail, 3 attempts max" / status:429,status:429,status:429,ok
timed t "S2h no gateway-error: timeout" /notimeoutretry hang:3,ok
t "S2i no gateway-error: 503 tagged" /notimeoutretry status:503,ok
t "S3a dead upstream + hop secret" /dead ok -H "x-wso2-failover-hop: s3cret"
t "S3b dead upstream, no secret" /dead ok
t "S4a body 4KiB > limit, 429,ok" /bufsmall status:429,ok --data-binary "$(head -c 4096 /dev/zero | tr '\0' a)"
t "S4b body < limit, 429,ok" /bufsmall status:429,ok --data-binary small
# Which retry_on value retries a per-try timeout? (hang:3 with per_try_timeout 1s)
for r in rt-reset rt-headers rt-connect rt-refused rt-hdr-reset; do timed t "S2j per-try timeout, retry_on=$r" /$r hang:3,ok; done
t "S2k untagged 502, retry_on=reset" /rt-reset status:502,ok
t "S2l 503 untagged-path, retry_on=reset" /rt-reset status:500,ok
