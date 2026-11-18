# 21. Node freeze under pod churn: an rmlock lost wakeup

*2026-11-18. FreeBSD 15.1 arm64, kernel `bsdk8s17` (fix in `bsdk8s18`).*

## Symptom

Node 2 froze twice during conformance runs: run 9 (during the GC test's 100-pod burst; not reproduced in 34 churn
cycles afterwards) and run 11 (after a burst at 17:03:04 that took the node from 7 to 34 jails and from 6 to 30
interfaces in 15 seconds). The node went `NotReady`, SSH timed out during the banner, and the run stalled until
the suite timeout.

The serial console still offered a login prompt, so the kernel itself wasn't hung. Logging in worked, but no
command ran. Ctrl-T on the console showed why:

```
load: 0.64  cmd: login 2956 [allproc] 6080.69r 0.00u 0.09s 0% 3236k
```

`login` was waiting for `allproc_lock` to fork the shell, as was every other fork and exit.

## Getting kernel stacks without a working shell

The debugger break sequence (`debug.kdb.alt_break_to_debugger=1`, CR `~` Ctrl-B) was set but didn't fire, and
QEMU's `virt` machine has no NMI. What worked was **QEMU's gdb stub**: `gdbserver tcp:127.0.0.1:1234` on the
monitor socket, then on the Mac `lldb kernel.debug` plus the module `.ko.debug` files (from
`/usr/lib/debug/boot/<kernel>/`, loaded at their `linker_files` addresses) and `gdb-remote`. Small lldb Python
scripts walked `allproc` and unwound sleeping threads from `td_pcb` (arm64: `pcb_x[10]` is the frame pointer,
`pcb_x[11]` the return address; strip the PAC bits; `scripts/kdebug/*.py`). All 8 vCPUs were idle, so this was a pure sleep deadlock.

## What the stacks showed

| Thread | Holds | Waits for |
|---|---|---|
| 2 threads (shared holders of `allproc`) | `allproc` (shared) | `allprison`; with them, 227 threads (CRI-O, `exit1`, fork) wait on `allproc` |
| `ocijail` (`jail -r`): `prison_deref` → `tuntap_prison_remove` → `if_clone_detach` | `allprison` (exclusive) | `ifnet_detach_sx` |
| `ifconfig epairN destroy`: `if_detach_internal` | `ifnet_detach_sx` | `epoch_wait_preempt` (net epoch) |
| `ifconfig epair create`: `if_alloc_domain` | `ifnet_sx` | `epoch_wait_preempt` (net epoch) |
| 6 more `ifconfig`, 30 `ocijail`, `bridge`, `jls` | | `ifnet_sx` or `allprison` |

Both epoch waiters were asleep on the turnstile of `pf rulesets` (pf's rules rmlock). That lock had **no active
readers, no writer, and its turnstile had no owner**. Its exclusive queue held exactly the two `ifconfig`
threads, and no CPU's net-epoch record listed any thread. Nothing would ever wake them.

## Cause

- `pfi_attach_ifnet_event()` and `pfi_detach_ifnet_event()`, run on every interface attach and detach, enter the
  net epoch and then write-lock pf's rules lock. A writer waiting for active readers (packets in `pf_test()`)
  sleeps on the rmlock's turnstile, with the reader as owner.
- `if_alloc_domain()` and `if_detach_internal()` wait for the net epoch. When a thread in the epoch section is
  blocked on a lock, `epoch_block_handler_preempt()` puts the waiter on that lock's turnstile, here the rmlock's.
- When the last tracked reader leaves, `_rm_unlock_hard()` calls `turnstile_signal()`, which **wakes one thread**:
  rmlock assumes its writer is the only waiter. The writer woke and finished; the epoch waiters slept on an
  ownerless turnstile forever, holding `ifnet_sx` and `ifnet_detach_sx`. Jail removal blocked behind them while
  holding `allprison`, and so did everything taking `allprison` while holding `allproc`.

Kubernetes makes this likely: pod starts and stops create and destroy epairs and VNET jails while pf filters
traffic. FreeBSD main has the same code.

## Fix (freebsd-src patch 0032, branches `bsdk8s/rmlock-wake-all[-15.1]`)

`_rm_unlock_hard()` now uses `turnstile_broadcast()`. Spurious wakeups are harmless: the writer loops while
readers remain, and epoch waiters go back to ck_epoch's scan. rwlock unlocks use the same broadcast + unpend
pattern. Both nodes boot `kernel.bsdk8s18` (= `bsdk8s17` + this). The main twin builds on the main test rig.

## Reproducing it deterministically

A system-level stress didn't hit the window: on a drained node, 8 epair create/destroy loops, flood pings through
pf on lo0, pf anchor loads, `pfctl -sr` and 8 CPU hogs (`scripts/kdebug/rmstress*.sh`) ran 30 minutes on the
unfixed kernel without hanging.

A small kernel module does hit it every time: `scripts/kdebug/rmepoch/rmepoch_test.c`, run with
`sysctl debug.rmepoch_test.run=1`. It builds the exact pattern with its own rmlock and preemptible epoch:

1. A kernel thread takes a tracked read lock and spins.
2. A second thread enters the epoch and calls `rm_wlock()`. The test waits until it's on a turnstile.
3. Two threads call `epoch_wait_preempt()`. The test waits until both are on a turnstile too.
4. The reader unlocks.

| Kernel | Result (5 runs each, node 2) |
|---|---|
| `bsdk8s17` (unfixed) | 5 × `STUCK: writer done, 0 of 2 epoch waiters finished 3s after the last reader left` |
| `bsdk8s18` (patch 0032) | 5 × `ok: writer and 2 epoch waiters finished` |

The stuck state matches the post-mortem exactly: the writer was woken and finished, and both epoch waiters stayed
asleep on the rmlock's turnstile. To clean up after a stuck run, the module does what the fix does, a
`turnstile_broadcast()` on that turnstile. The waiters then finish, which shows that a missing wakeup was all
that held them, and the module unloads.

Conformance run 12 (`bsdk8s18`, research/07) ran the full suite without a freeze.
