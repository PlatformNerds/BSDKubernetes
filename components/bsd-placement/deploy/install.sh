#!/bin/sh
# Install bsd-placement: self-signed CA + serving cert, Secret, manifests.
# Usage: IMAGE=localhost/bsd-placement:dev ./install.sh
set -eu
IMAGE=${IMAGE:-localhost/bsd-placement:dev}
dir=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
svc=bsd-placement.bsd-placement.svc
openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -subj "/CN=bsd-placement-ca" \
    -keyout "$tmp/ca.key" -out "$tmp/ca.crt" 2>/dev/null
openssl req -newkey rsa:2048 -nodes -subj "/CN=$svc" \
    -keyout "$tmp/tls.key" -out "$tmp/tls.csr" 2>/dev/null
printf 'subjectAltName=DNS:%s,DNS:%s.cluster.local\nextendedKeyUsage=serverAuth\n' "$svc" "$svc" > "$tmp/ext"
openssl x509 -req -in "$tmp/tls.csr" -CA "$tmp/ca.crt" -CAkey "$tmp/ca.key" -CAcreateserial \
    -days 3650 -extfile "$tmp/ext" -out "$tmp/tls.crt" 2>/dev/null
CA_BUNDLE=$(base64 < "$tmp/ca.crt" | tr -d '\n')
kubectl apply -f - <<YAML
apiVersion: v1
kind: Namespace
metadata: {name: bsd-placement}
YAML
kubectl -n bsd-placement create secret tls bsd-placement-tls \
    --cert="$tmp/tls.crt" --key="$tmp/tls.key" --dry-run=client -o yaml | kubectl apply -f -
sed -e "s|\${IMAGE}|$IMAGE|" -e "s|\${CA_BUNDLE}|$CA_BUNDLE|" "$dir/bsd-placement.yaml" | kubectl apply -f -
kubectl -n bsd-placement rollout restart deploy/bsd-placement > /dev/null
kubectl -n bsd-placement rollout status deploy/bsd-placement --timeout=120s
