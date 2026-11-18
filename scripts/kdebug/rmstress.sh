#!/bin/sh
# rmstress.sh SECONDS: interface churn (pf attach/detach handlers take the pf
# rules write lock inside the net epoch; if_alloc/if_detach wait for the epoch)
# against pf rules readers (packets through pf on lo0, pfctl -sr).
dur=${1:-600}
end=$(( $(date +%s) + dur ))
for i in 1 2 3 4; do
  ( while [ $(date +%s) -lt $end ]; do a=$(ifconfig epair create) && ifconfig ${a%a}b up && ifconfig $a destroy; done ) &
done
for i in 1 2 3 4; do ping -f -q -s 1200 127.0.0.1 >/dev/null 2>&1 & done
( while [ $(date +%s) -lt $end ]; do pfctl -sr >/dev/null 2>&1; pfctl -a 'cni-rdr/*' -sn >/dev/null 2>&1; done ) &
n=0
while [ $(date +%s) -lt $end ]; do sleep 5; n=$((n+5)); echo "RMSTRESS alive ${n}s epairs=$(ifconfig -l | wc -w)" > /dev/console; done
pkill -f "ping -f -q -s 1200"
wait
echo "RMSTRESS done" > /dev/console
