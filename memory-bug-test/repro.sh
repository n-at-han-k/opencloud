set +e  # curl probes against a starting/dying server are expected to fail
files=@files@
out=/repro-out/$(uname -n)
mkdir -p "$out"
exec >"$out/log" 2>&1
set -x

cg=/sys/fs/cgroup/system.slice/opencloud.service
base=http://127.0.0.1:9200/remote.php/dav/files/admin
auth="admin:admin"

mark() {
  echo "$1 $(date +%s)" >> "$out/phases"
  echo "REPRO-MARK $1 $(date +%s)" > /dev/console
}

# 1 Hz memory sampler; keeps writing even after opencloud dies
( while :; do
    echo "$(date +%s) $(cat $cg/memory.current 2>/dev/null || echo dead)" >> "$out/samples"
    sync -f "$out" 2>/dev/null || true
    sleep 1
  done ) &
sampler=$!

mark boot
code=000
for i in $(seq 240); do
  code=$(curl -s -o /dev/null -w '%{http_code}' -u "$auth" -X PROPFIND "$base/")
  [ "$code" = 207 ] && break
  [ $((i % 15)) = 0 ] && echo "REPRO-PROBE i=$i code=$code" > /dev/console
  sleep 2
done
mark "server-up-$code"
if [ "$code" != 207 ]; then
  journalctl -u opencloud --no-pager | tail -40 > /dev/console
  journalctl -u opencloud --no-pager | tail -200 > "$out/journal-tail"
  sync
  systemctl poweroff
  exit 1
fi

echo x > /tmp/payload
curl -s -u "$auth" -X MKCOL "$base/big/"
mark mkcol

seq -f 'f%05g.txt' 1 "$files" \
  | xargs -P 16 -I{} curl -s -u "$auth" -T /tmp/payload -o /dev/null "$base/big/{}"
mark uploads-done

# poll until the last-numbered file shows up in search, i.e. indexing caught up
sentinel=$(printf 'f%05d.txt' "$files")
body='<?xml version="1.0"?><oc:search-files xmlns:d="DAV:" xmlns:oc="http://owncloud.org/ns"><oc:search><oc:pattern>'$sentinel'</oc:pattern><oc:limit>5</oc:limit></oc:search></oc:search-files>'
iters=$((files / 10)); [ $iters -lt 60 ] && iters=60; [ $iters -gt 360 ] && iters=360
for i in $(seq $iters); do
  curl -s -u "$auth" -X REPORT -H 'Content-Type: text/xml' --data "$body" "$base" | grep -q "$sentinel" && break
  sleep 2
done
mark indexed
journalctl -u opencloud --no-pager | grep '"service":"search"' | tail -60 > "$out/search-journal"
tail -8 "$out/search-journal" > /dev/console
du -sb /var/lib/opencloud/search* 2>/dev/null | tee "$out/index-size" > /dev/console
sleep 15
mark settled

curl -s --max-time 60 http://127.0.0.1:9224/debug/pprof/heap -o "$out/heap-baseline.pb.gz" || true
cat "$cg/memory.peak" > "$out/peak-before-delete" 2>/dev/null || true
# writable since 6.12; makes peak-after measure ONLY the delete window, so
# even a sub-second allocation spike is caught
echo reset > "$cg/memory.peak" 2>/dev/null && echo peak-reset-ok > /dev/console

# THE TRIGGER: deleting the folder makes the search service look up every
# descendant to flag it Deleted -- the wildcard path in the unpatched build
mark delete-start
curl -s --max-time 600 -u "$auth" -X DELETE "$base/big/" &

# 10 Hz burst sampler for the first 60s: the unthrottled query can spike and
# free within a second
( for n in $(seq 600); do
    echo "$(date +%s%N) $(cat $cg/memory.current 2>/dev/null || echo dead)" >> "$out/burst"
    sleep 0.1
  done; sync -f "$out" ) &

for t in 2 4 9 30 60; do
  sleep "$t"
  curl -s --max-time 30 http://127.0.0.1:9224/debug/pprof/heap -o "$out/heap-delete-t$t.pb.gz" || true
done
sleep 120
mark watch-done

kill $sampler || true
systemctl show opencloud -p Result -p NRestarts -p MemoryPeak -p ActiveState > "$out/unit"
cat "$cg/memory.peak" > "$out/peak-after" 2>/dev/null || true
journalctl -u opencloud --no-pager | tail -400 > "$out/journal-tail"
sync
systemctl poweroff
