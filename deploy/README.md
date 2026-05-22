# Deploy

These manifests are local/private defaults. Build and load
`headscale-ingress-operator:local` into your test cluster before applying them,
or patch the image to a registry you control.

```bash
docker build -t headscale-ingress-operator:local .
kubectl kustomize deploy
```

Release builds are published as:

- `ghcr.io/lucasilverentand/headscale-ingress-operator`
- `oci://ghcr.io/lucasilverentand/charts/headscale-ingress-operator`
