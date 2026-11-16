# 16: Node.js under the Linuxulator on arm64: three kernel bugs (2026-11-16)

**Result [verified]:** `node:22-alpine` (Node v22.23.5) runs unmodified in a pod with RuntimeClass `linux`, limited
to 256Mi/500m:
- `process.constrainedMemory()` = **256 MiB**, read from the cgroup view (research/15);
- V8 `heap_size_limit` = **259 MiB**, i.e. sized to the pod;
- `os.availableParallelism()` = 8, from affinity, which is also what Linux reports;
- an `http` server answers through Service DNS and ClusterIP;
- the process exits cleanly.

Before this, `node --version` died with SIGBUS on every kernel. Each fix exposed the next problem.

## 1. SIGBUS at startup: SME traps (arm64 kernel, not Linux-specific)

DTrace on `trapsignal()` showed the fatal signal came from exception class **0x1d**: an SME access trap. The host is
an **Apple M6 with FEAT_SME**. Node's embedded OpenSSL probes CPU features by executing instructions and catching
SIGILL. FreeBSD 15.1 doesn't support SME, so the SME instruction trapped, and `do_el0_sync()` has no case for EC 0x1d.
The default case delivered **SIGBUS** (`BUS_OBJERR`), which the probe doesn't catch. Linux delivers SIGILL.

Fix: `EXCP_SME` (0x1d) delivers SIGILL/ILL_ILLTRP like an undefined instruction. This affects native FreeBSD
programs on SME hardware too, not only the Linuxulator.
- Branches: `bsdk8s/arm64-sme-sigill[-15.1]`.
- Patch: freebsd-src/0010.

## 2. Hang at startup: `mremap` returned ENOMEM for unmapped memory (Linuxulator, 15.x only)

ktrace: **9.7 million** `mremap(addr, 4K, 8K, 0)` calls in 15 s, at decreasing addresses. musl's
`pthread_getattr_np()` sizes the main thread's stack by growing one page in place downwards until `mremap` fails
with something other than ENOMEM. Linux returns EFAULT for an unmapped old range. 15.1's `linux_mremap()` returned
ENOMEM for any growth, so musl walked the whole address space.

Fix: EFAULT when the old range isn't within one mapping; stack guard entries count as unmapped.
- **FreeBSD main already has equivalent code**, a `useracc()` check, so this is a 15.x backport.
- Branch: `bsdk8s/linux-mremap-efault-15.1`.
- Patch: freebsd-src/0011, annotated accordingly.

## 3. Hang at exit: no musl brand on arm64 (Linuxulator)

The main thread blocked joining a worker. The worker's `futex(FUTEX_REQUEUE)`, which musl's condition variables
use, got EINVAL, and the wakeup was lost. The Linuxulator allows FUTEX_REQUEUE only for brands flagged
`LINUX_BI_FUTEX_REQUEUE`, i.e. the musl brand, but **arm64 has no musl brand**. amd64 has
`/lib/ld-musl-x86_64.so.1`.

There was also a second bug: arm64's glibc brand names amd64's interpreter, `/lib64/ld-linux-x86-64.so.2`. So arm64
dynamic Linux binaries were only ever recognized through `kern.elf64.fallback_brand`, which research/10 had to set.

Fix: add the arm64 musl brand (`/lib/ld-musl-aarch64.so.1`, FUTEX_REQUEUE allowed) and correct the glibc path to
`/lib/ld-linux-aarch64.so.1`.
- Branches: `bsdk8s/linux-arm64-musl-brand[-15.1]`.
- Patch: freebsd-src/0012.

Both nodes boot `kernel.bsdk8s9`, which contains all kernel fixes so far.

## Notes

- The kernel still logs `unsupported FUTEX_REQUEUE` once per process: the message is printed before the brand check.
  It's cosmetic.
- With the brands fixed, `fallback_brand=3` may no longer be needed for dynamic binaries. It is still needed for
  static musl binaries that have no interpreter or brand note. Not re-tested yet.
- A fork-side mistake: an empty `bsdk8s/linux-mremap-efault` (main) branch was pushed. It points at an upstream commit
  with no changes of ours, because main already had the fix. Deleting it is a no-op cleanup.
