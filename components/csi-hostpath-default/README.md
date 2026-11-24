# csi-hostpath-default: a default StorageClass for the test cluster

A PersistentVolumeClaim without `storageClassName` needs a default StorageClass. Without one, 22 upstream e2e tests
skip themselves (14 sig-apps StatefulSet tests, 8 storage tests), and so does anything that just asks for a volume.

`csi-hostpath.yaml` is the csi-hostpath driver exactly as the e2e framework deploys it
(`test/e2e/testing-manifests/storage-csi/`: the sidecars' RBAC, the driver info and the plugin StatefulSet; FreeBSD
builds of every image, research/24), with three changes the framework also makes or implies:

- everything in its own namespace, `csi-hostpath`;
- `/var/lib/kubelet` rewritten to the FreeBSD kubelet root, `/var/db/kubelet`;
- the `csi-external-health-monitor-controller` and `csi-snapshot-metadata` containers (and the latter's certificate
  volume) removed, as the framework removes them unless a test needs them.

`storageclass.yaml` makes the driver the cluster default. The plugin runs as one replica, so every volume is a
directory on the node where it runs: test and demo storage, not for data you keep.

```sh
kubectl create ns csi-hostpath
kubectl apply -n csi-hostpath -f csi-hostpath.yaml
kubectl apply -f storageclass.yaml
```
