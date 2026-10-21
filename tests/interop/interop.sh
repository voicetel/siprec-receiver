#!/usr/bin/env bash
# In-container half of the siprec-receiver live interop test. Runs in
# the mod-siprec-load image (FreeSWITCH + this tree's mod_siprec) with
# host networking; drives real sofia calls whose SIPREC recordings go
# to the siprec-receiver under test on the host (see run.sh).
#
# Scenarios (dialplan extensions below):
#   main      siprec -> 4 s -> pause -> 3 s -> resume -> 4 s -> hangup
#   stop      siprec <ad-hoc uri> -> 2 s -> siprec_stop -> 3 s -> hangup
#   separate  separate-streams: recorded leg plays 1000 Hz, far end
#             plays 440 Hz -> pause -> resume; two labelled streams
set -uo pipefail
PREFIX=/usr/local/freeswitch
CONF=${PREFIX}/etc/freeswitch
FS=${PREFIX}/bin/freeswitch
CLI=${PREFIX}/bin/fs_cli
OUT=${OUT:-/tmp/interop}
mkdir -p "$OUT"

IP=$(hostname -I | awk '{print $1}')
echo "[interop] container/host IP: ${IP}"

# Vanilla vars.xml STUN-resolves external IPs; pin to our own address
# so in-dialog requests between the local profiles stay local.
sed -i -E "s#<X-PRE-PROCESS cmd=\"stun-set\" data=\"(external_(rtp|sip)_ip)=[^\"]*\"/>#<X-PRE-PROCESS cmd=\"set\" data=\"\\1=${IP}\"/>#" \
    "${CONF}/vars.xml"

cat > "${CONF}/autoload_configs/siprec.conf.xml" <<EOF
<configuration name="siprec.conf" description="siprec-receiver interop test">
  <settings>
    <param name="src-enabled" value="true"/>
    <param name="originate-timeout" value="5"/>
  </settings>
  <recording-servers>
    <recording-server name="default">
      <settings>
        <param name="host" value="${IP}"/>
        <param name="port" value="5070"/>
        <param name="transport" value="udp"/>
      </settings>
    </recording-server>
    <recording-server name="sep">
      <settings>
        <param name="host" value="${IP}"/>
        <param name="port" value="5070"/>
        <param name="transport" value="udp"/>
        <param name="separate-streams" value="true"/>
      </settings>
    </recording-server>
  </recording-servers>
</configuration>
EOF

cat > "${CONF}/dialplan/default/00_siprec_interop.xml" <<EOF
<include>
  <extension name="siprec_live_main">
    <condition field="destination_number" expression="^siprec_live_main\$">
      <action application="answer"/>
      <action application="sleep" data="500"/>
      <action application="siprec" data="default"/>
      <action application="sleep" data="4000"/>
      <action application="siprec_pause" data="default"/>
      <action application="sleep" data="3000"/>
      <action application="siprec_resume" data="default"/>
      <action application="sleep" data="4000"/>
      <action application="hangup"/>
    </condition>
  </extension>
  <extension name="siprec_live_stop">
    <condition field="destination_number" expression="^siprec_live_stop\$">
      <action application="answer"/>
      <action application="sleep" data="500"/>
      <action application="siprec" data="adhoc sip:${IP}:5070;transport=udp"/>
      <action application="sleep" data="2000"/>
      <action application="siprec_stop"/>
      <action application="sleep" data="3000"/>
      <action application="hangup"/>
    </condition>
  </extension>
  <extension name="siprec_live_sep">
    <condition field="destination_number" expression="^siprec_live_sep\$">
      <action application="answer"/>
      <action application="sleep" data="500"/>
      <action application="siprec" data="sep"/>
      <action application="playback" data="tone_stream://%(3000,0,1000)"/>
      <action application="siprec_pause" data="sep"/>
      <action application="playback" data="tone_stream://%(2000,0,1000)"/>
      <action application="siprec_resume" data="sep"/>
      <action application="playback" data="tone_stream://%(2500,0,1000)"/>
      <action application="hangup"/>
    </condition>
  </extension>
</include>
EOF

cat > "${CONF}/dialplan/public/00_siprec_interop.xml" <<'EOF'
<include>
  <extension name="siprec_tone">
    <condition field="destination_number" expression="^siprec_tone$">
      <action application="answer"/>
      <action application="endless_playback" data="tone_stream://%(1000,0,440)"/>
    </condition>
  </extension>
</include>
EOF

"$FS" -nonat -nc -nf -nonatmap > "${OUT}/fs.stdout" 2>&1 &
FSPID=$!
for _ in $(seq 1 60); do "$CLI" -x status >/dev/null 2>&1 && break; sleep 1; done
for _ in $(seq 1 30); do
    st=$("$CLI" -x "sofia status" 2>/dev/null)
    echo "$st" | grep -qE "internal .*RUNNING" && echo "$st" | grep -qE "external .*RUNNING" && break
    sleep 1
done
LOGFILE="$("$CLI" -x "global_getvar log_dir" | tr -d '[:space:]')/freeswitch.log"
"$CLI" -x "load mod_siprec"

run_call() {
    echo "[interop] scenario: $1 -> $2"
    echo "[interop] originate: $("$CLI" -x "originate sofia/internal/siprec_tone@${IP}:5080 $2 XML default")"
    for _ in $(seq 1 40); do
        sleep 1
        n=$("$CLI" -x "show channels count" | grep -oE '^[0-9]+' || echo 0)
        [ "${n:-0}" = 0 ] && break
    done
    sleep 2
}

run_call main siprec_live_main
run_call stop siprec_live_stop
run_call separate siprec_live_sep

"$CLI" -x "shutdown" >/dev/null 2>&1 || kill "$FSPID"
wait "$FSPID" 2>/dev/null
cp "$LOGFILE" "${OUT}/fs.log" 2>/dev/null || true
echo "[interop] done"
