# 12: Deleted pods lingered: volumes held by stopped containers (2026-11-16)

> **Update (research/15):** the dominant cause turned out to be different. For images with `/var/run → /run` (most
> Linux images), ocijail unmounted volumes by re-resolving a symlinked path after CRI-O had already unmounted the
> container root, so the mounts leaked and those pods stayed Terminating *forever*. Fixed in ocijail `aa3e111`. The
> release-on-stop change below still applies to images without that symlink.

**Symptom.** After a Deployment rollout, deleted pods stayed listed as `Completed`/`Error` for 1–3 minutes. That
slows anything that waits for a pod to be gone: StatefulSet ordinal replacement, namespace deletion, e2e
timeouts.

**Mechanism (from logs, pod `kv4b8`):**

| Time | Event |
|---|---|
| 07:28:20.22 | CRI-O: `Stopped container ac5d14a3…` |
| 07:28:20 → 07:29:06 | kubelet: `UnmountVolume` of the pod's secret and projected tmpfs fails 14× with `Device busy` |
| 07:29:06.43 | CRI-O: `RemoveContainer` → `ocijail delete` unmounts the container's volume mounts |
| 07:29:24 | kubelet: `UnmountVolume.TearDown succeeded`; the pod object is removed |

ocijail exposes volumes to a container as nullfs mounts into its root, made with `MNT_IGNORE`. That's why `mount`
doesn't show them; `df -a` does. These mounts were only removed by `delete`. On Linux, a container's mounts live in
its mount namespace and vanish when its init exits. The kubelet relies on that: it unmounts volumes once containers
have *stopped*, but removes dead containers separately. Whether removal happens at once or in the next container GC
(~1 min) depends on a race in the pod worker (`ShouldPodContentBeRemoved` when PLEG reports the exit). That makes
the delay intermittent. Five A/B pod deletions, with both old and new binaries, all finished in 1–3 s.

**Fix (ocijail `0e64f62`, patch ocijail/0011).** When `ocijail state` first observes a container as stopped, it
releases the container. CRI-O calls `state` right after it sees the exit. Releasing means removing the container's
jail, which kills any process left in it, as a Linux PID namespace does when its init exits, and unmounting its
volumes. A `released` flag makes this happen once. `delete` still releases if `state` never ran, then unmounts the
read-only root, removes rctl rules and runs poststop hooks as before. A release failure is logged and doesn't fail
`state`.

**Deterministic check.** This is a crash-looping pod with a secret volume: the kubelet keeps one dead container for
logs. The table counts the pod's volume mounts in `df -a`.

| | container running | between restarts |
|---|---|---|
| old ocijail | 10 (2 kubelet tmpfs + 4 live + **4 held by the dead container**) | 6 |
| new ocijail | 6 (2 + 4 live) | **2** (kubelet tmpfs only) |

Regression checks with the new binary all passed:
- exit code 3 is reported, and logs are readable after exit;
- a crash-looping container restarts, and its emptyDir keeps data across restarts;
- `kubectl exec` works;
- rollouts and deletes finish in 1–3 s.

ocijail's own Python test suite was **not** run: the nodes have no Python.

**Behaviour change to note.** Processes left in a container's jail after its init exits are now killed when the stop
is observed, rather than at delete. This matches Linux per-container PID namespaces. With
`shareProcessNamespace: true`, Linux behaves differently, but ocijail puts each container in its own jail anyway.
