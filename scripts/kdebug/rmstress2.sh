#!/bin/sh
# rmstress2.sh SECONDS: rmstress.sh plus CPU hogs (pf readers get preempted
# while holding the rules read lock) and pf rule loads (more writers).
dur=${1:-900}
end=$(( $(date +%s) + dur ))
for i in 1 2 3 4 5 6 7 8; do
  ( while [ $(date +%s) -lt $end ]; do a=$(ifconfig epair create) && ifconfig ${a%a}b up && ifconfig $a destroy; done ) &
done
for i in 1 2 3 4 5 6 7 8; do ( while [ $(date +%s) -lt $end ]; do :; done ) & done
for i in 1 2 3 4; do ping -f -q -s 1200 127.0.0.1 >/dev/null 2>&1 & done
printf "pass in quick on lo0 proto icmp from any to 127.0.0.1\npass out quick on lo0 proto icmp\n" > /tmp/rmstress.rules
( while [ $(date +%s) -lt $end ]; do pfctl -a cni-rdr/zz-rmstress -f /tmp/rmstress.rules 2>/dev/null; pfctl -sr >/dev/null 2>&1; done ) &
n=0
while [ $(date +%s) -lt $end ]; do sleep 5; n=$((n+5)); echo "RMSTRESS2 alive ${n}s epairs=$(ifconfig -l | wc -w)" > /dev/console; done
pkill -f "ping -f -q -s 1200"
wait
pfctl -a cni-rdr/zz-rmstress -F rules 2>/dev/null
echo "RMSTRESS2 done" > /dev/console
