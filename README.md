# Headscale Ingress Operator

Design notes for a Kubernetes operator that exposes ordinary Ingress-backed
HTTP(S) services through a Headscale tailnet and publishes the matching
Headscale MagicDNS records.

The intended user flow is:

1. An app declares an Ingress with `ingressClassName: headscale`.
2. The operator creates the implementation Ingress or Traefik `IngressRoute`
   used by the cluster ingress controller.
3. The operator publishes A/AAAA records into Headscale's managed
   `dns.extra_records_path` file.
4. Tailnet clients resolve the app hostname through Headscale and reach the
   internal ingress endpoint through an advertised route or a managed tailnet
   gateway.

The design is intentionally private-data-free. Examples use placeholder domains
and addresses.

See [docs/design.md](docs/design.md) for the proposed architecture, setup
steps, CRD/API shape, and migration notes for the existing cluster patterns.
