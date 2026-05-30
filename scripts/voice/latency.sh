#!/usr/bin/env bash
# latency.sh -- compute decision->first-audio latency from voice-trace logs (#484).
#
# Reads structured voice-trace lines (slog text: time=... stage=... space_id=...)
# captured from a live voice session and pairs the four per-turn stamps
#   T0 voice.final -> T1 cognition.gate.engage -> T2 turntaking.assistant.speak
#   -> T3 realtime.audio.first
# per space, then prints per-turn deltas + a median summary. No silent caps: it
# reports the matched-turn count.
#
# Usage: bash scripts/voice/latency.sh <voicetrace.log>
#   (capture with: make dev-logs 2>&1 | grep "voice trace" > /tmp/voicetrace.log)
set -euo pipefail

main() {
	local log="${1:-}"
	if [[ -z "$log" || ! -r "$log" ]]; then
		echo "usage: bash scripts/voice/latency.sh <voicetrace.log>" >&2
		exit 2
	fi
	report "$log"
}

report() {
	awk '
		function ms(ts,   cmd, out) {
			cmd = "date -d \"" ts "\" +%s%3N 2>/dev/null"
			cmd | getline out; close(cmd); return out
		}
		{
			t=""; stage=""; sp="default"
			for (i=1;i<=NF;i++) {
				if ($i ~ /^time=/)     t=substr($i,6)
				if ($i ~ /^stage=/)    { stage=substr($i,7); gsub(/"/,"",stage) }
				# space key differs by process: voice-agent stamps space_id,
				# cognition stamps spaceId. Accept either; pairing across the two
				# is best-effort (id forms can differ), the headline falls back to T0.
				if ($i ~ /^space_id=/) { sp=substr($i,10); gsub(/"/,"",sp) }
				if ($i ~ /^spaceId=/)  { sp=substr($i,9);  gsub(/"/,"",sp) }
			}
			if (stage=="") next
			m=ms(t); if (m=="") next

			if (stage=="voice.final")                { t0[sp]=m; t1[sp]=""; t2[sp]=""; next }
			if (t0[sp]=="") next
			if (stage=="cognition.gate.engage")      { t1[sp]=m; next }
			if (stage=="turntaking.assistant.speak") { t2[sp]=m; next }
			if (stage=="realtime.audio.first") {
				n++
				base = (t1[sp]!="") ? t1[sp] : t0[sp]
				headline = m - base
				printf "turn %d  space=%s  decision->first-audio=%dms", n, sp, headline
				if (t1[sp]!="") printf "  gate(T1-T0)=%dms", t1[sp]-t0[sp]
				if (t2[sp]!="") printf "  modelTTFB(T3-T2)=%dms", m-t2[sp]
				printf "  e2e(T3-T0)=%dms\n", m-t0[sp]
				vals[n]=headline
				t0[sp]=""
			}
		}
		END {
			if (n==0) {
				print "no complete turns matched (need voice.final ... realtime.audio.first per space)"
				exit
			}
			for (i=1;i<=n;i++) for (j=i+1;j<=n;j++) if (vals[j]<vals[i]) { x=vals[i]; vals[i]=vals[j]; vals[j]=x }
			med = vals[int((n+1)/2)]
			p95i = int(n*0.95); if (p95i < 1) p95i = 1; if (p95i > n) p95i = n
			printf "\n%d turns | decision->first-audio median=%dms p95=%dms (min=%dms max=%dms)\n", n, med, vals[p95i], vals[1], vals[n]
		}
	' "$1"
}

main "$@"
