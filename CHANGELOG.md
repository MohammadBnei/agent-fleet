# Changelog

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
