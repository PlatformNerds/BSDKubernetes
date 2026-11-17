# 17: Pods with `spec.os.name: linux` on FreeBSD nodes

*2026-11-17. Kubernetes `bsdk8s-v1.36.1` `875310bdad1`, CRI-O `bsdk8s-1.37` `2804e3d13`, bsd-placement (this repo).
Implements the semantics proposed in research/01 §4.*

**Result [verified]:** a pod that declares `spec.os.name: linux` now runs on a FreeBSD node through the `linux`
RuntimeClass, with no other change to the pod. Before, the kubelet rejected it with `PodOSNotSupported`. A mismatched
image (a FreeBSD image under a tag the Linux runtime is asked to run) is now refused with a clear error, instead of
silently running FreeBSD binaries in a "Linux" pod.

## The problem

`spec.os.name` is the Kubernetes API's way for a pod to say which OS it needs. Only `linux` and `windows` are valid
values. The kubelet admits a pod only if `spec.os.name` equals the node's own OS (`rejectPodAdmissionBasedOnOSField`).
Upstream marks this with a TODO to relax it for LCOW (Linux containers on Windows), which is the same situation as
Linux containers on FreeBSD.

So a pod that says "I need Linux" could never run here, even though the node runs Linux images through the
Linuxulator RuntimeClass (research/10). Charts written for mixed Linux/Windows clusters set this field. So do
upstream e2e tests: all six in-place resize Conformance tests hard-code `spec.os.name: linux`, and those six were
conformance failures that are not `[LinuxOnly]` (research/07).

## The change

1. **The RuntimeClass declares the OS it runs.** Annotation `bsdk8s.io/pod-os: linux` on the `linux` RuntimeClass
   (`kubernetes-demo` `420fc85`).
2. **kubelet** (`patches/kubernetes/v1.36.1/bsdk8s/0021`): a pod whose `spec.os.name` differs from the node OS is
   admitted only if its RuntimeClass declares that OS. Pods with no RuntimeClass, or with one that declares nothing,
   are rejected exactly as before. Unit tests cover all four cases.
3. **bsd-placement**: `spec.os.name: linux` counts as a Linux pin, like nodeSelector `kubernetes.io/os=linux`. The
   webhook sets `runtimeClassName: linux` and the Linux-ABI nodeSelector. It keeps `spec.os` unchanged, because the
   pod really does need the Linux ABI.

Verified on node 2, then on both nodes:

| Pod | Before | After |
|---|---|---|
| `spec.os.name: linux` + `runtimeClassName: linux` | `PodOSNotSupported` | Running, `uname -s` = `Linux` |
| `spec.os.name: linux` alone, webhook not updated | `PodOSNotSupported` | `PodOSNotSupported` (unchanged) |
| `spec.os.name: linux` alone, with the updated webhook | `PodOSNotSupported` | Running (`placement: runtimeClassName=linux (spec.os.name=linux)`) |
| no `spec.os` (FreeBSD busybox) | Running | Running |

## What it exposed: the wrong image for the handler

With admission fixed, the resize tests' pods started, but under the `linux` handler they ran **FreeBSD** busybox.
The cause:
- the FreeBSD e2e images are published under the upstream names (research/07);
- the kubelet asks whether an image is present **by name only**, so it never pulls the Linux variant;
- CRI-O creates the container from whatever image that tag points to locally.

Only one image per tag can exist in local storage. The proper fix is the alpha `RuntimeClassInImageCriApi` gate
(KEP-4216), which passes the runtime handler to image status and pull calls. Until then, CRI-O refuses the mismatch
(`patches/cri-o/0016`–`0017`):

```
image "registry.k8s.io/e2e-test-images/busybox:1.37.0-1" is a freebsd image, but runtime handler "linux" runs
linux images; the local copy of this tag is for another OS, remove it or use a linux image
```

While doing this I found that the fork's `crio` did not build for Linux at all: a dfr WIP base commit had moved
`getOCICapabilitiesList` without its imports. That is fixed in `patches/cri-o/0015`.

## The resize Conformance tests

Before this change, all six failed at admission. With the change they run, but they still fail:
1. **The image.** On this cluster their busybox tag is the FreeBSD build. With the guard above, they now fail at
   container creation with the error shown, which is the honest outcome.
2. **Unenforced values, even with a Linux busybox.** The tests check `cpu.weight` (from CPU requests). FreeBSD has no
   CPU-weight scheduler control: rctl has caps (`pcpu`), not shares. lincgroupfs reports `cpu.weight` as 100, and
   faking the requested value would claim an enforcement that does not exist.

So the six tests stay failing, and the reason is now clear: they need cgroup CPU weights, which FreeBSD does not
provide. Making them pass by running test pods under the Linuxulator would not reflect anything real about the node.
