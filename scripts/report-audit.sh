#!/bin/sh
# report-audit.sh — recompute every number in the chain-registry measurement report
# from its attached artifacts, showing the arithmetic for each. No trust required:
# run it, compare, disagree loudly.
#
# Inputs (release assets of chain-registry-sentinel v0.8.3):
#   RUN  = 20260801T172659Z-github-runner.jsonl   sentinel records, registry @ e1c86c4
#   TLOG = 0_test-endpoints.txt                   the registry's own test_endpoints log, 2026-07-31
#   MAY  = 20260731T080445Z-github-runner.jsonl   optional: May-state measurement @ 1e92f162
#                                                 (movement + fourteen-for-fourteen claims)
# Usage:
#   ./report-audit.sh RUN.jsonl TLOG.txt [MAY.jsonl]
#
# Requires: jq, awk, grep, sort (POSIX userland). Nothing else.

set -eu
LC_ALL=C; export LC_ALL

RUN=${1:?usage: report-audit.sh RUN.jsonl TESTLOG.txt [MAY.jsonl]}
TLOG=${2:?usage: report-audit.sh RUN.jsonl TESTLOG.txt [MAY.jsonl]}
MAY=${3:-}

# Structural failure classes, mirroring the classifier (see PROBES.md / README taxonomy).
STRUCTURAL='dns_nxdomain conn_refused net_unreachable tls_expired tls_hostname tls_untrusted tls_unrecognized_name http_404 cf_origin_down gateway_no_backend bad_json eof_no_response malformed_registry_address not_served_by_provider'

# The fourteen chains marked killed upstream 2026-06-03..2026-07-15
# (PRs #7710, #7739, #7744, #7745, #7813). Their display names in the pytest log.
KILLED='evmos umee stargaze nillion starname tgrade onomy onex routerchain fxcore dhealth omniflixhub lambda milkyway'
KILLED_PYTEST='Evmos Umee Stargaze Nillion Starname Tgrade Onomy Onex Routerchain Fxcore Dhealth Omniflixhub Lambda Milkyway'

RULE='────────────────────────────────────────────────────────────────────────────────────────'
sec() { printf '\n%s\n  %s\n%s\n' "$RULE" "$1" "$RULE"; }
row() { printf '  %-30s %-40s %s\n' "$1" "$2" "$3"; }

# liveness FILE → TSV: chain domain check passed class order latency http provider
liveness() {
  jq -r 'select((.check|endswith("_liveness")) and ((.skipped//false)|not))
         | [.chain, .domain, .check, (.passed|tostring), (.failure_class//""),
            (.order//0|tostring), (.latency_ms//0|tostring), (.http_status//0|tostring),
            (.provider//"")] | @tsv' "$1"
}

printf '%s\n' "$RULE"
printf '  REPORT AUDIT — every number, with its arithmetic\n'
printf '%s\n' "$RULE"
head -1 "$RUN" | jq -r '"  dataset   " + .run_ts + "   registry @ " + .registry_commit[0:9] + "   vantage: " + .vantage'
printf '  tester    %s\n' "$(grep -o '[0-9-]*T[0-9:]*\.[0-9]*Z' "$TLOG" | head -1) (their test_endpoints run)"

sec "HEADLINE — endpoints, dead rate, chains"
liveness "$RUN" | awk -F'\t' '
  {t++; if($4=="false") d++}
  END {
    printf "  %-30s %-40s %s\n", "endpoints probed",   "count of non-skipped liveness records", t
    printf "  %-30s %-40s %d / %d = %.1f%%\n", "dead", "failed / probed", d, t, d/t*100
    printf "  %-30s %-40s %s\n", "live",               "probed - dead", t-d }'
liveness "$RUN" | awk -F'\t' '$3~/^(rpc|rest|evm)_liveness$/ {c[$1]=1}
  END {printf "  %-30s %-40s %s\n", "chains probed", "distinct chains with core checks", length(c)}'
liveness "$RUN" | awk -F'\t' '$3=="rpc_liveness" {c[$1]=1}
  END {printf "  %-30s %-40s %s\n", "chains with RPC entries", "distinct chains with rpc_liveness", length(c)}'

sec "STRUCTURAL SHARE"
liveness "$RUN" | awk -F'\t' -v S="$STRUCTURAL" '
  BEGIN {n=split(S,a," "); for(i=1;i<=n;i++) s[a[i]]=1}
  $4=="false" {d++; if($5 in s) st++}
  END {printf "  %-30s %-40s %d / %d = %.1f%%\n", "structural failures", "structural-class dead / all dead", st, d, st/d*100}'

sec "DECOMPOSITION — dead chains / operator exits / residual"
liveness "$RUN" | awk -F'\t' '
  {rec[NR]=$0
   if($3~/^(rpc|rest|evm)_liveness$/){ct[$1]++; if($4=="true")cl[$1]++}
   dt[$2]++; if($4=="true")dl[$2]++}
  END {
    for(c in ct) if(cl[c]==0) dead[c]=1
    for(d in dt) if(dl[d]==0) deadd[d]=1
    for(i=1;i<=NR;i++){split(rec[i],f,"\t")
      all++; if(f[4]=="false") alldead++
      if(f[1] in dead){dc++; continue}
      surv++; if(f[4]=="true"){sl++; continue}
      if(f[2] in deadd) op++; else res++}
    printf "  %-30s %-40s %s\n",              "fully-dead chains",   "chains with 0 live core endpoints", length(dead)
    printf "  %-30s %-40s %d / %d = %.0f%%\n","  their endpoints",   "on dead chains / all dead", dc, alldead, dc/alldead*100
    printf "  %-30s %-40s %d / %d = %.0f%%\n","operator exits",      "dead on live chains, 0%-live domain / all dead", op, alldead, op/alldead*100
    printf "  %-30s %-40s %d / %d = %.1f%%\n","residual dead",       "remaining dead / (survivors - exits)", res, surv-op, res/(surv-op)*100
    printf "  %-30s %-40s %s\n",              "0%-live domains",     "domains with no live endpoint anywhere", length(deadd)}'

sec "FIRST-LISTED RPC"
liveness "$RUN" | awk -F'\t' -v S="$STRUCTURAL" '
  BEGIN {n=split(S,a," "); for(i=1;i<=n;i++) s[a[i]]=1}
  $3=="rpc_liveness" && $6=="1" {t[$1]=1; if($4=="false"){d[$1]=1; if($5 in s) st[$1]=1}}
  END {
    printf "  %-30s %-40s %d / %d = %.1f%%\n", "first-listed RPC dead", "chains whose order-1 RPC failed / with RPC", length(d), length(t), length(d)/length(t)*100
    printf "  %-30s %-40s %s\n", "  of them structural", "failed with a structural class", length(st)}'

sec "WITHDRAWAL-SIGNATURE QUEUE (zero-core-live, >=3 operators alive elsewhere)"
liveness "$RUN" | awk -F'\t' '
  {rec[NR]=$0
   if($3~/^(rpc|rest|evm)_liveness$/){ct[$1]++; if($4=="true")cl[$1]++}
   dt[$2]++; if($4=="true")dl[$2]++}
  END {
    for(c in ct) if(cl[c]==0) dead[c]=1
    for(i=1;i<=NR;i++){split(rec[i],f,"\t")
      if((f[1] in dead) && f[4]=="false" && dl[f[2]]>0) w[f[1]"\t"f[2]]=1}
    for(k in w){split(k,f,"\t"); wc[f[1]]++}
    for(c in wc) if(wc[c]>=3)
      printf "%04d\t  %-30s %-40s %d witnesses\n", wc[c], c, "dead domains here, live elsewhere", wc[c]}' |
  sort -rn | cut -f2-

sec "NODE QUALITY (live endpoints)"
jq -r 'select((.check|endswith("_liveness")) and .passed==true)
       | [(.tx_index//"-"),(.catching_up//false|tostring),(.method_restricted//false|tostring)] | @tsv' "$RUN" |
awk -F'\t' '{if($1=="off")a++; if($2=="true")b++; if($3=="true")c++}
  END {
    printf "  %-30s %-40s %s\n", "tx_index off",       "live records with tx_index==off", a+0
    printf "  %-30s %-40s %s\n", "still syncing",      "live records with catching_up",   b+0
    printf "  %-30s %-40s %s\n", "method-restricted",  "live via fallback query",         c+0}'

sec "WRONG CHAIN-ID"
jq -r 'select((.check|endswith("_chain_id")) and .passed==false) | .chain' "$RUN" | sort | uniq -c |
  awk '{printf "  %-30s %-40s %s\n", $2, "failing chain-id checks", $1}'
jq -r 'select((.check|endswith("_chain_id")) and .passed==false) | .chain' "$RUN" | wc -l |
  awk '{printf "  %-30s %-40s %s\n", "TOTAL", "sum of the above", $1}'

sec "CASE-STUDY ANCHORS"
liveness "$RUN" | awk -F'\t' '$2=="autostake.com" {t++; c[$1]=1; if($4=="true")l++}
  END {printf "  %-30s %-40s %d entries / %d chains / %d live\n", "autostake.com", "entries, chains, live count", t, length(c), l+0}'
liveness "$RUN" | awk -F'\t' '$2=="publicnode.com" {t++; if($5=="not_served_by_provider")ns++}
  END {printf "  %-30s %-40s %d / %d\n", "publicnode tombstones", "not_served_by_provider / their entries", ns, t}'
jq -r 'select(.chain=="migaloo" and .failure_class=="not_served_by_provider") | .endpoint' "$RUN" | wc -l |
  awk '{printf "  %-30s %-40s %s\n", "migaloo tombstones", "not_served interfaces at publicnode", $1}'
liveness "$RUN" | awk -F'\t' '$2=="stakeflow.io" && $4=="false" {n++}
  END {printf "  %-30s %-40s %s\n", "stakeflow.io dead entries", "spot-check dig anchor", n}'

sec "THEIR TEST_ENDPOINTS RUN (summary table = one row per test)"
TBL_START=$(grep -n '| filepath |' "$TLOG" | head -1 | cut -d: -f1)
grep -o '[0-9]* failed, [0-9]* passed' "$TLOG" | head -1 | awk -F'[ ,]' '
  {printf "  %-30s %-40s %d / %d = %.1f%%\n", "their gross rate", "failed / (failed+passed), their summary", $1, $1+$4, $1/($1+$4)*100}'
awk -v s="$TBL_START" 'NR>=s' "$TLOG" | grep -c 'chain: ' |
  awk '{printf "  %-30s %-40s %s\n", "table rows", "one per test; must equal failed+passed", $1}'
awk -v s="$TBL_START" 'NR>=s' "$TLOG" | grep -c '▌ RPC ' |
  awk '{printf "  %-30s %-40s %s\n", "  RPC tests", "table rows typed RPC", $1}'
awk -v s="$TBL_START" 'NR>=s' "$TLOG" | grep -c '▌ REST' |
  awk '{printf "  %-30s %-40s %s\n", "  REST tests", "table rows typed REST", $1}'
kt=0; kf=0
for c in $KILLED_PYTEST; do
  n=$(awk -v s="$TBL_START" 'NR>=s' "$TLOG" | grep -c "chain: $c " || true)
  f=$(awk -v s="$TBL_START" 'NR<s'  "$TLOG" | grep -c "chain: $c " || true)
  kt=$((kt+n)); kf=$((kf+f))
done
row "killed-chain tests" "table rows for the 14 killed chains" "$kt"
row "  of them failing" "FAILED lines for the same 14" "$kf"
grep -o '[0-9]* failed, [0-9]* passed' "$TLOG" | head -1 | awk -F'[ ,]' -v kt="$kt" -v kf="$kf" '
  {printf "  %-30s %-40s (%d-%d) / (%d-%d) = %d / %d = %.1f%%\n", "their net-of-killed rate",
     "excluding killed-chain tests", $1, kf, $1+$4, kt, $1-kf, ($1+$4)-kt, ($1-kf)/(($1+$4)-kt)*100}'

sec "REPLICA — their criteria vs mine, on my records (RPC+REST only)"
liveness "$RUN" | awk -F'\t' '$3~/^(rpc|rest)_liveness$/ {
    t++; if(!($8==200 && $7<=2000)) their++; if($4=="false") mine++}
  END {
    printf "  %-30s %-40s %s\n", "population", "my rpc+rest non-skipped entries", t
    printf "  %-30s %-40s %d / %d = %.1f%%\n", "fail, their criteria", "NOT(HTTP 200 within 2s)", their, t, their/t*100
    printf "  %-30s %-40s %d / %d = %.1f%%\n", "fail, my criteria", "liveness verdict", mine, t, mine/t*100}'

sec "GRANULARITY EXHIBIT — neutron REST, provider \"Neutron\""
jq -r 'select(.chain=="neutron" and .check=="rest_liveness" and (.provider//"")=="Neutron") | .endpoint' "$RUN" | wc -l |
  awk '{printf "  %-30s %-40s %s\n", "my entries", "per-entry probing", $1}'
awk -v s="$TBL_START" 'NR>=s' "$TLOG" | grep 'chain: Neutron ' | grep -c 'REST ▌ Neutron' |
  awk '{printf "  %-30s %-40s %s\n", "their tests, same triple", "name-keyed parametrization", $1}'

if [ -n "$MAY" ]; then
  sec "MAY-STATE MEASUREMENT (movement + fourteen-for-fourteen)"
  head -1 "$MAY" | jq -r '"  " + .run_ts + "   registry @ " + .registry_commit[0:9]'
  liveness "$MAY" | awk -F'\t' '{t++; if($4=="false")d++}
    END {printf "  %-30s %-40s %d / %d = %.1f%%\n", "may dead rate", "failed / probed", d, t, d/t*100}'
  printf '  %-30s %-40s\n' "killed chains at May state" "want: 0 live of any type, each"
  for c in $KILLED; do
    liveness "$MAY" | awk -F'\t' -v c="$c" '$1==c {t++; if($4=="true")l++}
      END {if(t==0) printf "  %-30s %-40s %s\n", c, "", "ABSENT"
           else printf "  %-30s %-40s %d live of %d\n", "  " c, "", l+0, t}'
  done
fi

printf '\n%s\n' "$RULE"
printf '  done — compare against the report; every mismatch is probably a bug in the report.\n'
printf '%s\n' "$RULE"