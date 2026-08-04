# Changelog

## [1.0.2](https://github.com/MohammadBnei/agent-fleet/compare/1.0.1...1.0.2) (2026-08-04)


### Bug Fixes

* **ci:** contents: read alongside pull-requests: read on the changes job ([9eeb9f4](https://github.com/MohammadBnei/agent-fleet/commit/9eeb9f4015b37d7a46d4daf1b2aa4aad555b1d41))

## [1.0.1](https://github.com/MohammadBnei/agent-fleet/compare/1.0.0...1.0.1) (2026-08-04)


### Bug Fixes

* **logs:** use JSON structured logging, not slog's default text handler ([68ea8b8](https://github.com/MohammadBnei/agent-fleet/commit/68ea8b8110b804267a0382b640da4c306081bd82))

# [1.0.0](https://github.com/MohammadBnei/agent-fleet/compare/0.7.0...1.0.0) (2026-08-04)


* feat!: replace Redis coordination with Go fleet-core; rewrite e2e-provisioner and bot in Go ([56d1b09](https://github.com/MohammadBnei/agent-fleet/commit/56d1b09ba6565a587e15ad443cb454686aaecb9e))


### Bug Fixes

* **ci:** buf breaking git ref, missing protoc-gen plugin installs, caching ([943b963](https://github.com/MohammadBnei/agent-fleet/commit/943b9630348b48b59118213717a53ea4431dd59f))
* **ci:** pin golangci-lint v2.12.2 + action v9 for go1.26 support ([354a884](https://github.com/MohammadBnei/agent-fleet/commit/354a88435fd528c80ffb9e154a4996687a599ba6))
* **test:** use a log-occurrence wait strategy for testcontainers postgres ([1e883a2](https://github.com/MohammadBnei/agent-fleet/commit/1e883a2809063eabc299f49d3359b7c8c76f3d19))


### BREAKING CHANGES

* bot/ and mcp-redis/ no longer exist; REDIS_* env vars are
no longer read by any component. Requires a coordinated redeploy (new
fleet-core.yaml, e2e-provisioner Deployment env/port changes in
infra-bootstrap) and an empty task queue at cutover time.

Co-Authored-By: ukubi-claude-macbook <noreply@bnei.dev>

# [0.7.0](https://github.com/MohammadBnei/agent-fleet/compare/0.6.0...0.7.0) (2026-08-01)


### Bug Fixes

* **ci:** use a PR-safe image tag for build-push-e2e-runner ([be044fe](https://github.com/MohammadBnei/agent-fleet/commit/be044fece4ca2ed5d154e04f10b25335a123dcba))
* **k8s:** use Recreate strategy for RWO-PVC worker pods ([f2d94f6](https://github.com/MohammadBnei/agent-fleet/commit/f2d94f6dfbadca4da3067e59d51da957c88ee6c7))


### Features

* on-demand e2e test environments for the implementation phase ([4317d37](https://github.com/MohammadBnei/agent-fleet/commit/4317d37f69515f93d53599b5bb821f63b91ce20e))

# [0.6.0](https://github.com/MohammadBnei/agent-fleet/compare/0.5.0...0.6.0) (2026-08-01)


### Features

* opt-out critic session + proposer/critic context handoff ([f49ebf3](https://github.com/MohammadBnei/agent-fleet/commit/f49ebf307b5a701348a00fc9587dcbfa6cff4bd1))

# [0.5.0](https://github.com/MohammadBnei/agent-fleet/compare/0.4.0...0.5.0) (2026-07-30)


### Bug Fixes

* **worker:** allow MCP redis tools in implementation phase too ([0343402](https://github.com/MohammadBnei/agent-fleet/commit/0343402399a771ac6cd8576a54ba2bf294973831))
* **worker:** set git commit identity from the authenticated bot account ([eaee251](https://github.com/MohammadBnei/agent-fleet/commit/eaee2517f987a37b00a8cee8e2b1b6c40cc33b13))


### Features

* **worker:** web access + rtk/ponytail tooling for proposer/critic/impl ([015f45c](https://github.com/MohammadBnei/agent-fleet/commit/015f45c4400b28d03b4ed9d81c429c266da8642e))

# [0.4.0](https://github.com/MohammadBnei/agent-fleet/compare/0.3.0...0.4.0) (2026-07-30)


### Bug Fixes

* **worker:** bump maxTurns to 100/100, 40 still wasn't enough headroom ([23fe8f5](https://github.com/MohammadBnei/agent-fleet/commit/23fe8f53e96395b0b5624b62d9ba34761f941597))
* **worker:** drop maxBudgetUsd entirely, no metered API cost to cap ([629d619](https://github.com/MohammadBnei/agent-fleet/commit/629d6192ccc9db4877806cb75e74da87bb14b3e1))
* **worker:** make maxTurns opt-in, unbounded by default ([8e56fa7](https://github.com/MohammadBnei/agent-fleet/commit/8e56fa7770fb2b48f8755d84841826f384f59ba2))
* **worker:** raise planning turn/budget cap, 15/\$2 was too tight ([60f2f64](https://github.com/MohammadBnei/agent-fleet/commit/60f2f6482dd7a814ad40790ddbeeaf7204178719))


### Features

* **worker:** stream proposer/critic/implementation LLM text to Discord ([a89a09e](https://github.com/MohammadBnei/agent-fleet/commit/a89a09e285b3e15bb134a2b1e0e87bb084eb3d72))

# [0.3.0](https://github.com/MohammadBnei/agent-fleet/compare/0.2.2...0.3.0) (2026-07-30)


### Features

* **worker:** stream every SDK message to stdout, log aborts ([f41bcd7](https://github.com/MohammadBnei/agent-fleet/commit/f41bcd7ebc3223783979348563cc9af3f1837f54))

## [0.2.2](https://github.com/MohammadBnei/agent-fleet/compare/0.2.1...0.2.2) (2026-07-30)


### Bug Fixes

* **worker:** allow the MCP redis tools, not just built-ins, in planning ([b55b9d5](https://github.com/MohammadBnei/agent-fleet/commit/b55b9d513eeaa30ff0124d61c398461d765dbea3))

## [0.2.1](https://github.com/MohammadBnei/agent-fleet/compare/0.2.0...0.2.1) (2026-07-30)


### Bug Fixes

* **ci:** quote the sed step as a YAML block scalar ([60863c1](https://github.com/MohammadBnei/agent-fleet/commit/60863c170da935ac061e46ffd0e3194c1d471b20))

# [0.2.0](https://github.com/MohammadBnei/agent-fleet/compare/0.1.5...0.2.0) (2026-07-30)


### Features

* **deploy:** self-contained k8s/ values, CI bumps pinned image tag on release ([287c49d](https://github.com/MohammadBnei/agent-fleet/commit/287c49dab08d259031643735f625772d5b2aaa93))

## [0.1.5](https://github.com/MohammadBnei/agent-fleet/compare/0.1.4...0.1.5) (2026-07-30)


### Bug Fixes

* relay planning transcript to Discord live, stop watchBatch hanging forever ([bebb192](https://github.com/MohammadBnei/agent-fleet/commit/bebb192636f131b1cbc30b8ee07b29c146ca7f86))

## [0.1.4](https://github.com/MohammadBnei/agent-fleet/compare/0.1.3...0.1.4) (2026-07-30)

## [0.1.3](https://github.com/MohammadBnei/agent-fleet/compare/0.1.2...0.1.3) (2026-07-30)

## [0.1.2](https://github.com/MohammadBnei/agent-fleet/compare/0.1.1...0.1.2) (2026-07-30)

## 0.1.1 (2026-07-30)
