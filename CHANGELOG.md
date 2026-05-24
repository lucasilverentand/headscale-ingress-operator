# Changelog

## [2.0.1](https://github.com/lucasilverentand/headscale-ingress-operator/compare/v2.0.0...v2.0.1) (2026-05-24)


### Bug Fixes

* allow proxy cleanup deletes ([882d3cb](https://github.com/lucasilverentand/headscale-ingress-operator/commit/882d3cbd5d777c8ce0ca40fa1beb783e94c80fab))

## [2.0.0](https://github.com/lucasilverentand/headscale-ingress-operator/compare/v1.2.0...v2.0.0) (2026-05-24)


### ⚠ BREAKING CHANGES

* Services annotated with headscale-ingress-operator.lucasilverentand.dev/hostname are no longer reconciled. Declare a networking.k8s.io/v1 Ingress with ingressClassName: headscale instead.

### Features

* require headscale ingress resources ([1c032f3](https://github.com/lucasilverentand/headscale-ingress-operator/commit/1c032f30bb2d8759b450adc1a266fe66dd4c8403))

## [1.2.0](https://github.com/lucasilverentand/headscale-ingress-operator/compare/v1.1.1...v1.2.0) (2026-05-23)


### Features

* own managed proxy headscale resources ([38faca7](https://github.com/lucasilverentand/headscale-ingress-operator/commit/38faca70d1f7e4bd9c3411d7a29fc4430f63228f))

## [1.1.1](https://github.com/lucasilverentand/headscale-ingress-operator/compare/v1.1.0...v1.1.1) (2026-05-23)


### Bug Fixes

* allow proxy role delegation ([e245c7d](https://github.com/lucasilverentand/headscale-ingress-operator/commit/e245c7d17b9fd511f30bb2ded30dc3592879dab1))

## [1.1.0](https://github.com/lucasilverentand/headscale-ingress-operator/compare/v1.0.1...v1.1.0) (2026-05-23)


### Features

* manage per-service tailnet proxies ([#25](https://github.com/lucasilverentand/headscale-ingress-operator/issues/25)) ([9b1e1b6](https://github.com/lucasilverentand/headscale-ingress-operator/commit/9b1e1b6d0bfdf7465f3b91fa5274e1cf3722bda7))
* support Headscale extra records path seed ([#12](https://github.com/lucasilverentand/headscale-ingress-operator/issues/12)) ([18e2fdf](https://github.com/lucasilverentand/headscale-ingress-operator/commit/18e2fdf20490000f491ef7359e7e9c511d3ded22)), closes [#11](https://github.com/lucasilverentand/headscale-ingress-operator/issues/11)

## [1.0.1](https://github.com/lucasilverentand/headscale-ingress-operator/compare/v1.0.0...v1.0.1) (2026-05-22)


### Bug Fixes

* speed up multi-arch image builds ([#8](https://github.com/lucasilverentand/headscale-ingress-operator/issues/8)) ([25741b4](https://github.com/lucasilverentand/headscale-ingress-operator/commit/25741b496cac5505ceb557c85b7c8a5bd0ef6fba)), closes [#3](https://github.com/lucasilverentand/headscale-ingress-operator/issues/3)

## 1.0.0 (2026-05-22)


### Features

* implement headscale ingress operator ([a471d01](https://github.com/lucasilverentand/headscale-ingress-operator/commit/a471d011d79c7ed1d8ba5ef2e31961b4907fc918))


### Bug Fixes

* guard operator ownership boundaries ([5debfd1](https://github.com/lucasilverentand/headscale-ingress-operator/commit/5debfd15a18eccc3146182630d0b00444c8b2258))
* harden ingress reconciliation ([7b86de4](https://github.com/lucasilverentand/headscale-ingress-operator/commit/7b86de448728b91349562dff3ea2796f7741f05c))
* harden operator reconciliation ([22ea711](https://github.com/lucasilverentand/headscale-ingress-operator/commit/22ea7119611fc9db92694bc7e382aa7ce4032f6b))
