# Changelog

# [1.47.0](https://github.com/MohammadBnei/agent-fleet/compare/1.46.0...1.47.0) (2026-08-12)


### Features

* let one session prompt another, and prove a pod is still the pod ([#130](https://github.com/MohammadBnei/agent-fleet/issues/130)) ([44a67ce](https://github.com/MohammadBnei/agent-fleet/commit/44a67ce11debc3acc84a0fb8c2820fcd3685f33c))

# [1.46.0](https://github.com/MohammadBnei/agent-fleet/compare/1.45.0...1.46.0) (2026-08-12)


### Features

* **core:** derive session liveness, tear down pods that never start ([#129](https://github.com/MohammadBnei/agent-fleet/issues/129)) ([cc3fc5d](https://github.com/MohammadBnei/agent-fleet/commit/cc3fc5db2524fa782b7f894738068f78c353fb5a))

# [1.45.0](https://github.com/MohammadBnei/agent-fleet/compare/1.44.0...1.45.0) (2026-08-12)


### Features

* **dashboard:** render the whole SDK stream, once instead of twice ([#128](https://github.com/MohammadBnei/agent-fleet/issues/128)) ([b91ff27](https://github.com/MohammadBnei/agent-fleet/commit/b91ff279bd826cb2d7c4171b66f4fc2725c4579a))

# [1.44.0](https://github.com/MohammadBnei/agent-fleet/compare/1.43.3...1.44.0) (2026-08-12)


### Features

* **worker:** relay the SDK message kinds that were dropped silently ([#127](https://github.com/MohammadBnei/agent-fleet/issues/127)) ([63922e4](https://github.com/MohammadBnei/agent-fleet/commit/63922e4bd6d038d716ebd7c843dfa669f541acbf))

## [1.43.3](https://github.com/MohammadBnei/agent-fleet/compare/1.43.2...1.43.3) (2026-08-12)


### Bug Fixes

* **provisioner:** pin ImagePullPolicy on tool init containers ([b2d1ab1](https://github.com/MohammadBnei/agent-fleet/commit/b2d1ab18dca849681eac88264089e492a58adf8b))

## [1.43.2](https://github.com/MohammadBnei/agent-fleet/compare/1.43.1...1.43.2) (2026-08-12)


### Bug Fixes

* **e2e-ssh:** scope Infisical path to /e2e-ssh, drop cluster-wide 2222 rule ([5bd34b6](https://github.com/MohammadBnei/agent-fleet/commit/5bd34b6cd7cd73c7097e592681a4b8cef7fa8f5c))
* PR [#125](https://github.com/MohammadBnei/agent-fleet/issues/125) review comments - key remap, NetworkPolicy, docs ([a2d94a8](https://github.com/MohammadBnei/agent-fleet/commit/a2d94a83384fd43176dd2521a8c0a1745ba71509))

## [1.43.1](https://github.com/MohammadBnei/agent-fleet/compare/1.43.0...1.43.1) (2026-08-12)

# [1.43.0](https://github.com/MohammadBnei/agent-fleet/compare/1.42.1...1.43.0) (2026-08-11)


### Bug Fixes

* **alerts:** the error-log alert never said which error ([16b27f0](https://github.com/MohammadBnei/agent-fleet/commit/16b27f0ee42a0e1172386b99c21a3e081f04b958)), closes [#123](https://github.com/MohammadBnei/agent-fleet/issues/123)
* **e2e:** publish not-ready addresses so the pod stays reachable while the app is down ([08c1b55](https://github.com/MohammadBnei/agent-fleet/commit/08c1b555c9329bafe34c6d4d8b1ac4f818049cdb))


### Features

* **e2e:** make the e2e pod the worker's build/test sandbox ([8a4c5ce](https://github.com/MohammadBnei/agent-fleet/commit/8a4c5ce294bad86a10f4bea4db783e237a27619f))


### Performance Improvements

* **e2e:** size the sandbox for building, raising the namespace LimitRange ([bc5da8f](https://github.com/MohammadBnei/agent-fleet/commit/bc5da8f9ea2b18a79105172a29e0ba2ae7e19ca2))

## [1.42.1](https://github.com/MohammadBnei/agent-fleet/compare/1.42.0...1.42.1) (2026-08-11)


### Bug Fixes

* **logs:** the agent-facing log format spent more tokens on prefixes than on logs ([#121](https://github.com/MohammadBnei/agent-fleet/issues/121)) ([805c4e7](https://github.com/MohammadBnei/agent-fleet/commit/805c4e75c67ecceb1ad50b444934bbf4e9ddbce9))

# [1.42.0](https://github.com/MohammadBnei/agent-fleet/compare/1.41.0...1.42.0) (2026-08-11)


### Bug Fixes

* **core:** make MAX_IN_FLIGHT_TASKS a real ceiling under concurrency ([d52a80a](https://github.com/MohammadBnei/agent-fleet/commit/d52a80adb14a5c43c0ae2b69f4a11009d72eb421))


### Features

* **core:** machine-created thot tasks are proposals, not dispatches ([f90d133](https://github.com/MohammadBnei/agent-fleet/commit/f90d133792c874ccef0265ac0a7ae585a1913814))
* **core:** turn firing Alertmanager alerts into thot tasks ([8a5bc7c](https://github.com/MohammadBnei/agent-fleet/commit/8a5bc7c04d6a83388ba1721a34ba0e9127bcac7b))

# [1.41.0](https://github.com/MohammadBnei/agent-fleet/compare/1.40.3...1.41.0) (2026-08-11)


### Features

* **dashboard:** "Sync with git" button to clear stale worktrees ([bbad22d](https://github.com/MohammadBnei/agent-fleet/commit/bbad22d4bb0040aefcbda0608c33c6eacc0ab2d8)), closes [#118](https://github.com/MohammadBnei/agent-fleet/issues/118)

## [1.40.3](https://github.com/MohammadBnei/agent-fleet/compare/1.40.2...1.40.3) (2026-08-11)


### Bug Fixes

* **worker:** install procps — the agent crashed mid-session without ps ([#119](https://github.com/MohammadBnei/agent-fleet/issues/119)) ([101b585](https://github.com/MohammadBnei/agent-fleet/commit/101b585efb67498942b7c0aa2319fa54e594f9fd))

## [1.40.2](https://github.com/MohammadBnei/agent-fleet/compare/1.40.1...1.40.2) (2026-08-11)


### Bug Fixes

* **provisioner:** the branch sweep never removed a single worktree ([#118](https://github.com/MohammadBnei/agent-fleet/issues/118)) ([9a8b9fc](https://github.com/MohammadBnei/agent-fleet/commit/9a8b9fc716dfe8659abd4bbca99218589aa8e4a5))

## [1.40.1](https://github.com/MohammadBnei/agent-fleet/compare/1.40.0...1.40.1) (2026-08-11)


### Bug Fixes

* **logs:** the log viewer never returned anything — six bugs ([#117](https://github.com/MohammadBnei/agent-fleet/issues/117)) ([4a08b7a](https://github.com/MohammadBnei/agent-fleet/commit/4a08b7a9ae68db3f29674fac7120359d9db7c50d))

# [1.40.0](https://github.com/MohammadBnei/agent-fleet/compare/1.39.1...1.40.0) (2026-08-11)


### Features

* **e2e:** per-task subdomains, app served at the root path ([#116](https://github.com/MohammadBnei/agent-fleet/issues/116)) ([260c2b6](https://github.com/MohammadBnei/agent-fleet/commit/260c2b6bb350bf460873f7ba8c454ba82b109814))

## [1.39.1](https://github.com/MohammadBnei/agent-fleet/compare/1.39.0...1.39.1) (2026-08-11)

# [1.39.0](https://github.com/MohammadBnei/agent-fleet/compare/1.38.3...1.39.0) (2026-08-11)


### Features

* **thot:** task-detail UI handles thot sessions (ADR-0037 phase 4) ([#110](https://github.com/MohammadBnei/agent-fleet/issues/110)) ([27ebbe0](https://github.com/MohammadBnei/agent-fleet/commit/27ebbe0956394a80fd32ce5b05f369e4d83ef34d))

## [1.38.3](https://github.com/MohammadBnei/agent-fleet/compare/1.38.2...1.38.3) (2026-08-11)


### Bug Fixes

* **thot:** seed infra-bootstrap with an HTTPS URL, not SSH ([#113](https://github.com/MohammadBnei/agent-fleet/issues/113)) ([14f0b3b](https://github.com/MohammadBnei/agent-fleet/commit/14f0b3b69eba59cfaff1e67949a2d5a5b3c88dfb))

## [1.38.2](https://github.com/MohammadBnei/agent-fleet/compare/1.38.1...1.38.2) (2026-08-11)

## [1.38.1](https://github.com/MohammadBnei/agent-fleet/compare/1.38.0...1.38.1) (2026-08-11)


### Bug Fixes

* **ci:** build the executor image on release tags ([#112](https://github.com/MohammadBnei/agent-fleet/issues/112)) ([48cb9d9](https://github.com/MohammadBnei/agent-fleet/commit/48cb9d922f8d67d9cf96aed9a45ed3dc0129ba97))

# [1.38.0](https://github.com/MohammadBnei/agent-fleet/compare/1.37.0...1.38.0) (2026-08-11)


### Features

* **thot:** dispatch thot sessions as ordinary worker tasks (ADR-0037 phase 2) ([#108](https://github.com/MohammadBnei/agent-fleet/issues/108)) ([e753671](https://github.com/MohammadBnei/agent-fleet/commit/e75367188cb5be1ae937a4563e8636f9b9afdfb6))

# [1.37.0](https://github.com/MohammadBnei/agent-fleet/compare/1.36.3...1.37.0) (2026-08-11)


### Features

* **executor:** add the thot-executor service (ADR-0037 phase 1) ([#106](https://github.com/MohammadBnei/agent-fleet/issues/106)) ([ee30cf4](https://github.com/MohammadBnei/agent-fleet/commit/ee30cf41fa2e95804de38ec1c43f2f040502491b))

## [1.36.3](https://github.com/MohammadBnei/agent-fleet/compare/1.36.2...1.36.3) (2026-08-11)


### Bug Fixes

* **thot:** actually wire THOT_GRPC_ADDR into core and worker pods ([1c1eac8](https://github.com/MohammadBnei/agent-fleet/commit/1c1eac8c0f5378a4e7dec7619fbc5327ccbb0598)), closes [#94](https://github.com/MohammadBnei/agent-fleet/issues/94)

## [1.36.2](https://github.com/MohammadBnei/agent-fleet/compare/1.36.1...1.36.2) (2026-08-11)


### Bug Fixes

* **thot:** boot the session so readiness can ever pass ([5f2f65a](https://github.com/MohammadBnei/agent-fleet/commit/5f2f65a73344563c159beaf69b38e3bd5f3530ef))
* **thot:** make readiness free instead of paying for a boot turn ([c2c1f0c](https://github.com/MohammadBnei/agent-fleet/commit/c2c1f0c1546706643df5b82c807b039c76149be2))

## [1.36.1](https://github.com/MohammadBnei/agent-fleet/compare/1.36.0...1.36.1) (2026-08-11)


### Bug Fixes

* **thot:** split liveness from readiness so a slow session can start ([#102](https://github.com/MohammadBnei/agent-fleet/issues/102)) ([857d98d](https://github.com/MohammadBnei/agent-fleet/commit/857d98d58a1352aefa9889b6d027d75ad947051d))

# [1.36.0](https://github.com/MohammadBnei/agent-fleet/compare/1.35.0...1.36.0) (2026-08-11)


### Features

* **thot:** let humans ask thot directly from the dashboard ([#101](https://github.com/MohammadBnei/agent-fleet/issues/101)) ([5547978](https://github.com/MohammadBnei/agent-fleet/commit/55479784064c5c6f995e06894f40e2f85d20e0ef))

# [1.35.0](https://github.com/MohammadBnei/agent-fleet/compare/1.34.1...1.35.0) (2026-08-11)


### Features

* **thot:** dashboard-editable scheduled audits (ADR-0035 phase 4) ([#95](https://github.com/MohammadBnei/agent-fleet/issues/95)) ([05b2c0c](https://github.com/MohammadBnei/agent-fleet/commit/05b2c0c18cd5841abfcb481a284d6315d67ce1c1))
* **thot:** let thot read freely, keep mutation gated ([#96](https://github.com/MohammadBnei/agent-fleet/issues/96)) ([78019cf](https://github.com/MohammadBnei/agent-fleet/commit/78019cf6539d09ae7bef90901fab6fe61b871513))

## [1.34.1](https://github.com/MohammadBnei/agent-fleet/compare/1.34.0...1.34.1) (2026-08-11)


### Bug Fixes

* **e2e:** treat a terminating pod as no session, and document the shared worktree ([#100](https://github.com/MohammadBnei/agent-fleet/issues/100)) ([22837e2](https://github.com/MohammadBnei/agent-fleet/commit/22837e27a6f9d66453bedd17e4713b6b1255b772))

# [1.34.0](https://github.com/MohammadBnei/agent-fleet/compare/1.33.0...1.34.0) (2026-08-11)


### Features

* **thot:** sidecar↔thot Q&A and real cluster access (ADR-0035 phase 3) ([#94](https://github.com/MohammadBnei/agent-fleet/issues/94)) ([9594a61](https://github.com/MohammadBnei/agent-fleet/commit/9594a610ebcf3c6c5ae3709d066ffa9f2659d6d0))

# [1.33.0](https://github.com/MohammadBnei/agent-fleet/compare/1.32.0...1.33.0) (2026-08-11)


### Features

* **thot:** live permission gating + dashboard visibility (ADR-0035 phase 2) ([#99](https://github.com/MohammadBnei/agent-fleet/issues/99)) ([b7364c1](https://github.com/MohammadBnei/agent-fleet/commit/b7364c132712cf1c484056f25cb0879183c4e8f1))

# [1.32.0](https://github.com/MohammadBnei/agent-fleet/compare/1.31.3...1.32.0) (2026-08-11)


### Features

* **thot:** scaffold the cluster agent component (ADR-0035 phase 1) ([#92](https://github.com/MohammadBnei/agent-fleet/issues/92)) ([87511a2](https://github.com/MohammadBnei/agent-fleet/commit/87511a2767dc1c9d88b888d84e738b829cc176db))

## [1.31.3](https://github.com/MohammadBnei/agent-fleet/compare/1.31.2...1.31.3) (2026-08-11)


### Bug Fixes

* **e2e:** make the recipe visible, gate its override, probe the app port ([#97](https://github.com/MohammadBnei/agent-fleet/issues/97)) ([3b55fe2](https://github.com/MohammadBnei/agent-fleet/commit/3b55fe23fa20fe98209aadf492b623703f006dae))

## [1.31.2](https://github.com/MohammadBnei/agent-fleet/compare/1.31.1...1.31.2) (2026-08-10)


### Bug Fixes

* **worker:** add git safe.directory exception for the non-root worktree ([1256205](https://github.com/MohammadBnei/agent-fleet/commit/12562052b1ddbf70c176a745d2abc67e57de36d9))
* **worker:** run worker container as non-root for bypassPermissions mode ([6c1e491](https://github.com/MohammadBnei/agent-fleet/commit/6c1e491d0599782bcd4b249a35c9fd4b15482314))

## [1.31.1](https://github.com/MohammadBnei/agent-fleet/compare/1.31.0...1.31.1) (2026-08-10)


### Bug Fixes

* **worker:** log claude process stderr on crash ([1e401ec](https://github.com/MohammadBnei/agent-fleet/commit/1e401eca36b16dceb5b90d755f41f1e4f1a15a62))

# [1.31.0](https://github.com/MohammadBnei/agent-fleet/compare/1.30.1...1.31.0) (2026-08-10)


### Bug Fixes

* **fleet:** propagate the interrupt transcript type through coreserver's own copy, and treat it as an implicit permission resolution ([7de48be](https://github.com/MohammadBnei/agent-fleet/commit/7de48be0fc8d28c4001e45e517c28dc2c37442a2))


### Features

* **dashboard:** add soft interrupt, fix live permission-card bug, surface pending permissions in task list ([a3a14f9](https://github.com/MohammadBnei/agent-fleet/commit/a3a14f9c84ac0c3497caef7760770c1792746c23))

## [1.30.1](https://github.com/MohammadBnei/agent-fleet/compare/1.30.0...1.30.1) (2026-08-10)


### Bug Fixes

* **dashboard:** let the human explicitly tear down a repo's shared e2e services on kill ([5e03d4c](https://github.com/MohammadBnei/agent-fleet/commit/5e03d4c467cd0e9255bc5a53cc94f56570e65cf6))

# [1.30.0](https://github.com/MohammadBnei/agent-fleet/compare/1.29.2...1.30.0) (2026-08-10)


### Features

* **provisioner:** cache Go/Bun deps for e2e-preview pods on the shared PVC ([5384a63](https://github.com/MohammadBnei/agent-fleet/commit/5384a630206c4f50fe80e30e6fb83bdd71a60018))

## [1.29.2](https://github.com/MohammadBnei/agent-fleet/compare/1.29.1...1.29.2) (2026-08-10)


### Bug Fixes

* **provisioner:** give e2e-runner an explicit, real memory limit ([70709af](https://github.com/MohammadBnei/agent-fleet/commit/70709afac772fece3c19e28a4e0bb45658816e09))

## [1.29.1](https://github.com/MohammadBnei/agent-fleet/compare/1.29.0...1.29.1) (2026-08-10)


### Bug Fixes

* **provisioner:** postgres PGDATA subdirectory + surface pod failures in errors ([f5e2bc7](https://github.com/MohammadBnei/agent-fleet/commit/f5e2bc7a9ceaf27e6cbfef78ed24288b43916ab8))

# [1.29.0](https://github.com/MohammadBnei/agent-fleet/compare/1.28.2...1.29.0) (2026-08-10)


### Bug Fixes

* **provisioner:** satisfy errcheck on deferred conn.Close calls ([0df6ee9](https://github.com/MohammadBnei/agent-fleet/commit/0df6ee9ea4aef2e614d9fd5605de7b3236f35a55))
* real-cluster bugs found via /kind-local, then remove StartCmdFor fallback ([9f3d79b](https://github.com/MohammadBnei/agent-fleet/commit/9f3d79bb66bfc516bebb9fc9acb488912bddf0ea))


### Features

* **core:** resolve repo profiles into worker/e2e pod dispatch ([9500760](https://github.com/MohammadBnei/agent-fleet/commit/95007600500534da7d38c60071d24f7b82eccff6))
* **dashboard:** environment-recipe editor UI ([0c25690](https://github.com/MohammadBnei/agent-fleet/commit/0c256909f776eed254b56a4c33abe447283e8a07))
* **provisioner:** environment recipe foundation — schema, catalog, shared-instance lifecycle ([8d83769](https://github.com/MohammadBnei/agent-fleet/commit/8d837696aaeb9fbdfbbba0a9d23bbfabd4380485))
* **provisioner:** wire environment-recipe ingredients into pod creation ([53f8a84](https://github.com/MohammadBnei/agent-fleet/commit/53f8a84f9ea13dbb8c56e962a1b010c94e361f3d))

## [1.28.2](https://github.com/MohammadBnei/agent-fleet/compare/1.28.1...1.28.2) (2026-08-10)


### Bug Fixes

* **core:** use job label for worker logs, app label for e2e logs ([5441c16](https://github.com/MohammadBnei/agent-fleet/commit/5441c16187e25d17deb7d3847dbeeadf84e793f6))

## [1.28.1](https://github.com/MohammadBnei/agent-fleet/compare/1.28.0...1.28.1) (2026-08-10)


### Bug Fixes

* **core:** point LOKI_URL at the actual platform-loki Service ([917710f](https://github.com/MohammadBnei/agent-fleet/commit/917710f4b993fb81297d92d48286ab3f30344d89))

# [1.28.0](https://github.com/MohammadBnei/agent-fleet/compare/1.27.0...1.28.0) (2026-08-10)


### Features

* **worker:** journal search/write tools, live ponytail+caveman plugins, accurate fleet-shared context ([f7268a1](https://github.com/MohammadBnei/agent-fleet/commit/f7268a1c27c13203fb5e09665e554ba490e56979))

# [1.27.0](https://github.com/MohammadBnei/agent-fleet/compare/1.26.0...1.27.0) (2026-08-10)


### Bug Fixes

* **provisioner:** mirror fleet-shared content from the repo subdirectory, not clone root ([40e30f6](https://github.com/MohammadBnei/agent-fleet/commit/40e30f6ef171ccca0e9b7ca7d5d806d33eedaaf4))


### Features

* **fleet:** PVC-resident, provisioner-synced fleet-shared skills/context ([92e2869](https://github.com/MohammadBnei/agent-fleet/commit/92e28699da98393d413b94ac9e17a1e21dacd8c4))

# [1.26.0](https://github.com/MohammadBnei/agent-fleet/compare/1.25.4...1.26.0) (2026-08-09)


### Features

* Garage S3-backed shared file space for agents and the dashboard ([#64](https://github.com/MohammadBnei/agent-fleet/issues/64)) ([ed1ee42](https://github.com/MohammadBnei/agent-fleet/commit/ed1ee42b54a633c3f966e26fde5370c8bb2b7ac4))

## [1.25.4](https://github.com/MohammadBnei/agent-fleet/compare/1.25.3...1.25.4) (2026-08-09)


### Bug Fixes

* URL-encode DB credentials in migrate hook DSN ([34f44d0](https://github.com/MohammadBnei/agent-fleet/commit/34f44d0986f69b7bfee7dead0a4e5bffd5e56a23))
* use args not command for migrate hook flags ([67a3b7c](https://github.com/MohammadBnei/agent-fleet/commit/67a3b7c2c4e72d9e8463f899242d35f74ca097d1))

## [1.25.3](https://github.com/MohammadBnei/agent-fleet/compare/1.25.2...1.25.3) (2026-08-09)


### Bug Fixes

* replace hand-copied schema (embedded_schema.sql + test fixtures) with golang-migrate ([5ca679b](https://github.com/MohammadBnei/agent-fleet/commit/5ca679bac31c1e2d8f3b74263ea5700f9c45398a))

## [1.25.2](https://github.com/MohammadBnei/agent-fleet/compare/1.25.1...1.25.2) (2026-08-09)


### Bug Fixes

* **sidecar:** route GetTask/SetPermissionMode through CoreService, not DashboardService ([c48aa20](https://github.com/MohammadBnei/agent-fleet/commit/c48aa20654a3102ba9818adea288902304a63594))

## [1.25.1](https://github.com/MohammadBnei/agent-fleet/compare/1.25.0...1.25.1) (2026-08-09)


### Bug Fixes

* **e2e:** expose run_command's exec port on the Service and NetworkPolicy ([d91b7a9](https://github.com/MohammadBnei/agent-fleet/commit/d91b7a9d43966ea7f54dd078757ebde6f2166d5f)), closes [#65](https://github.com/MohammadBnei/agent-fleet/issues/65)

# [1.25.0](https://github.com/MohammadBnei/agent-fleet/compare/1.24.1...1.25.0) (2026-08-08)


### Bug Fixes

* add missing getTask and savePermissionMode to test mocks ([53eff64](https://github.com/MohammadBnei/agent-fleet/commit/53eff64bedb86bd50702e753c374e6c2a7dc67df))
* add suggested_permission_mode column to promptsnippets test schema ([7705002](https://github.com/MohammadBnei/agent-fleet/commit/77050020fe9ffc0e5a433aa91ff288fdacc4a37e))
* complete CreateTask test updates with model parameter ([0cdfdf9](https://github.com/MohammadBnei/agent-fleet/commit/0cdfdf95a4c35de381f93d405bd59de0f3d8b2f4))
* update CreateTask calls in tests to include model parameter ([966f059](https://github.com/MohammadBnei/agent-fleet/commit/966f05920ea9ca8b6ea63ca018f0696f782decee))


### Features

* implement model selection and auto-set permission mode from snippets ([10e0217](https://github.com/MohammadBnei/agent-fleet/commit/10e0217b1078e8af05e92036f50d74d8c020fa0d))
* use database as source of truth for task configuration ([1ed8dc9](https://github.com/MohammadBnei/agent-fleet/commit/1ed8dc92478ae1f63bf5d7cbfb278da805af5794))

## [1.24.1](https://github.com/MohammadBnei/agent-fleet/compare/1.24.0...1.24.1) (2026-08-08)


### Bug Fixes

* **dashboard:** remove EntryBubble memoization to fix message display bug ([ae2fc88](https://github.com/MohammadBnei/agent-fleet/commit/ae2fc882fc065cb6fc411c61eb6bbb1b1a576022))

# [1.24.0](https://github.com/MohammadBnei/agent-fleet/compare/1.23.5...1.24.0) (2026-08-08)


### Bug Fixes

* add LogViewer interface for testability in mcpserver ([e4c90b2](https://github.com/MohammadBnei/agent-fleet/commit/e4c90b204a4e5a77a7f646df8f3eea77f7d6305a))
* add missing loki param in server_test.go line 88 and fix TextContent type assertion ([85023c6](https://github.com/MohammadBnei/agent-fleet/commit/85023c6c71aedf5ce7a15113afe36ab7ebc40d18))
* add missing loki parameter to coreserver New() calls in integration tests ([0cbf576](https://github.com/MohammadBnei/agent-fleet/commit/0cbf576a4c9200c65a7f3138f825b3a3e46b168f))
* add missing loki parameter to dashboard NewServer test calls ([1643cec](https://github.com/MohammadBnei/agent-fleet/commit/1643ceccaca4d6266f727f20935b0a4dcbdd0493))
* address linter errors in lokiclient ([fc14fc5](https://github.com/MohammadBnei/agent-fleet/commit/fc14fc5c331ebc414fc73e9b273b79694009d270))
* correct CallToolRequest construction in view_logs_test ([0fae7bf](https://github.com/MohammadBnei/agent-fleet/commit/0fae7bfa8917f7eb920a48296e970623266d706f))
* include pod name in formatted log output ([f06753f](https://github.com/MohammadBnei/agent-fleet/commit/f06753f409f2f4f7372aa8e429a22d73315bcb14))
* update test mocks and interfaces for Loki Querier ([e6a5858](https://github.com/MohammadBnei/agent-fleet/commit/e6a5858d9a0a020f1645bb39689cec399cfab877))


### Features

* **core:** add Loki log viewer for fleet components and deployed apps ([44ce897](https://github.com/MohammadBnei/agent-fleet/commit/44ce8975f299c1fbfd0c4e1fa997717786c91e09))
* **core:** add RFC3339 timestamp support to view_logs tool ([ec2a3a9](https://github.com/MohammadBnei/agent-fleet/commit/ec2a3a91aa3dff8a2e28d73be7cef1a8e54cf8bf))

## [1.23.5](https://github.com/MohammadBnei/agent-fleet/compare/1.23.4...1.23.5) (2026-08-08)

## [1.23.4](https://github.com/MohammadBnei/agent-fleet/compare/1.23.3...1.23.4) (2026-08-08)

## [1.23.3](https://github.com/MohammadBnei/agent-fleet/compare/1.23.2...1.23.3) (2026-08-08)

## [1.23.2](https://github.com/MohammadBnei/agent-fleet/compare/1.23.1...1.23.2) (2026-08-08)

## [1.23.1](https://github.com/MohammadBnei/agent-fleet/compare/1.23.0...1.23.1) (2026-08-08)


### Bug Fixes

* **dashboard:** improve mobile UI responsiveness and auto-scroll ([#67](https://github.com/MohammadBnei/agent-fleet/issues/67)) ([3a500f1](https://github.com/MohammadBnei/agent-fleet/commit/3a500f10b1dd8988449dc3eed8b1ca611bcfc125))

# [1.23.0](https://github.com/MohammadBnei/agent-fleet/compare/1.22.1...1.23.0) (2026-08-08)


### Features

* **e2e:** agent-controlled app startup + ad-hoc run_command on the e2e pod ([#65](https://github.com/MohammadBnei/agent-fleet/issues/65)) ([11e1c4e](https://github.com/MohammadBnei/agent-fleet/commit/11e1c4e4de27bc32d9a850f33ca8e1fee07950b1))

## [1.22.1](https://github.com/MohammadBnei/agent-fleet/compare/1.22.0...1.22.1) (2026-08-08)


### Bug Fixes

* **worker,dashboard:** stop native AskUserQuestion, add cogitating indicator ([#66](https://github.com/MohammadBnei/agent-fleet/issues/66)) ([f71843c](https://github.com/MohammadBnei/agent-fleet/commit/f71843c5d88c22713d38ed703fdfd0b438f3d729))

# [1.22.0](https://github.com/MohammadBnei/agent-fleet/compare/1.21.3...1.22.0) (2026-08-08)


### Features

* **dashboard:** passage-level annotation on plan review cards ([#63](https://github.com/MohammadBnei/agent-fleet/issues/63)) ([208ce15](https://github.com/MohammadBnei/agent-fleet/commit/208ce15a097bdfe17892dd80de68412f0fdaeed3))

## [1.21.3](https://github.com/MohammadBnei/agent-fleet/compare/1.21.2...1.21.3) (2026-08-08)


### Bug Fixes

* **core:** correct CoreServiceServer e2e method casing ([27a35c5](https://github.com/MohammadBnei/agent-fleet/commit/27a35c55ba7f3e9e9c4e3560632b8663c1b79ac4))

## [1.21.2](https://github.com/MohammadBnei/agent-fleet/compare/1.21.1...1.21.2) (2026-08-08)

## [1.21.1](https://github.com/MohammadBnei/agent-fleet/compare/1.21.0...1.21.1) (2026-08-08)


### Bug Fixes

* **dashboard:** stop duplicate file-change lines and follow feed at bottom ([#62](https://github.com/MohammadBnei/agent-fleet/issues/62)) ([ae84fee](https://github.com/MohammadBnei/agent-fleet/commit/ae84feefde52f452e6bcac5423e18f6443ca7d8a))

# [1.21.0](https://github.com/MohammadBnei/agent-fleet/compare/1.20.0...1.21.0) (2026-08-08)


### Features

* session-not-task permission model, dashboard-driven guidance, human-only session ending ([#60](https://github.com/MohammadBnei/agent-fleet/issues/60)) ([5fa13a8](https://github.com/MohammadBnei/agent-fleet/commit/5fa13a8588b6d9ffc5392b688ea1ce80a3c09c39))

# [1.20.0](https://github.com/MohammadBnei/agent-fleet/compare/1.19.2...1.20.0) (2026-08-07)


### Bug Fixes

* **core,provisioner,worker:** three pod-lifecycle races caught live in kind ([a9a0ef8](https://github.com/MohammadBnei/agent-fleet/commit/a9a0ef8ed115870a52e6f6c1a9ad3286db9b096f))
* **discord:** actually deregister stale slash commands, not just stop handling them ([46ba3d6](https://github.com/MohammadBnei/agent-fleet/commit/46ba3d6fbf6a18fda540aa58282cc95abf5df0b7))
* **worker:** restore session.test.ts's sidecarClient mock after its own tests ([a87302d](https://github.com/MohammadBnei/agent-fleet/commit/a87302d8622406d26b1bfa93343ba93b51195a0e))


### Features

* **core,dashboard:** explicit warm/stop pod lifecycle ([d6590d7](https://github.com/MohammadBnei/agent-fleet/commit/d6590d79247c2ef703ad5aa02fc30fbd34fe1297))
* **core:** idle-timeout backstop for warm pods ([8fdc4ce](https://github.com/MohammadBnei/agent-fleet/commit/8fdc4ce7a33380a5df8d5f08fd44affd0ae74631))
* **db,proto:** add reply_to correlation + permission_request/response groundwork ([ac1eebf](https://github.com/MohammadBnei/agent-fleet/commit/ac1eebfa4016bc325537dbf1492e8825aaef6b6e))
* **worker,core,dashboard:** generalize canUseTool into a live permission gate, delete Approve ([0c3b1ce](https://github.com/MohammadBnei/agent-fleet/commit/0c3b1ce555b9494414d4133e8e71600430e93067))
* **worker,provisioner,core:** wire real session resume via CLAUDE_CONFIG_DIR + resume_session_id ([89596f2](https://github.com/MohammadBnei/agent-fleet/commit/89596f262ce60543a5badd012813895d9ce51e71))

## [1.19.2](https://github.com/MohammadBnei/agent-fleet/compare/1.19.1...1.19.2) (2026-08-07)

## [1.19.1](https://github.com/MohammadBnei/agent-fleet/compare/1.19.0...1.19.1) (2026-08-06)


### Bug Fixes

* **worker:** stop streamHumanMessages hanging on abort mid-onEntry ([#56](https://github.com/MohammadBnei/agent-fleet/issues/56)) ([dfba2ce](https://github.com/MohammadBnei/agent-fleet/commit/dfba2ce51a11bc9c54cd48b6e9edf5ed1ac21a51))

# [1.19.0](https://github.com/MohammadBnei/agent-fleet/compare/1.18.3...1.19.0) (2026-08-06)


### Features

* **dashboard:** resizable/collapsible sidebar panels, unified Actions modal, exchange-zone parity ([#54](https://github.com/MohammadBnei/agent-fleet/issues/54)) ([7f69043](https://github.com/MohammadBnei/agent-fleet/commit/7f690438127a94c36ba05030f331563641200fcf))

## [1.18.3](https://github.com/MohammadBnei/agent-fleet/compare/1.18.2...1.18.3) (2026-08-06)


### Bug Fixes

* **core:** actually kill worker pods on Stop ([#55](https://github.com/MohammadBnei/agent-fleet/issues/55)) ([a5dd238](https://github.com/MohammadBnei/agent-fleet/commit/a5dd2381d01c5d6a1157d9c4f06d40bd62ca04d0)), closes [#12](https://github.com/MohammadBnei/agent-fleet/issues/12)

## [1.18.2](https://github.com/MohammadBnei/agent-fleet/compare/1.18.1...1.18.2) (2026-08-06)


### Bug Fixes

* **worker,sidecar:** reconnect human-message feed instead of dying silently forever ([#53](https://github.com/MohammadBnei/agent-fleet/issues/53)) ([ccfb345](https://github.com/MohammadBnei/agent-fleet/commit/ccfb34535b0bdf16dde26a0292b8d4fa8050d61f))

## [1.18.1](https://github.com/MohammadBnei/agent-fleet/compare/1.18.0...1.18.1) (2026-08-06)


### Bug Fixes

* **worker:** make ExitPlanMode block on real approval, close Bash write-gate bypass ([#52](https://github.com/MohammadBnei/agent-fleet/issues/52)) ([686e105](https://github.com/MohammadBnei/agent-fleet/commit/686e105d57e36c025dd22ea9c4772b620bb73277))

# [1.18.0](https://github.com/MohammadBnei/agent-fleet/compare/1.17.0...1.18.0) (2026-08-06)


### Features

* **dashboard:** drag-to-reorder and resizable, persisted right-column panels ([#51](https://github.com/MohammadBnei/agent-fleet/issues/51)) ([ad7f600](https://github.com/MohammadBnei/agent-fleet/commit/ad7f600cb464c489195eab1c9d2a2a3536756b29))

# [1.17.0](https://github.com/MohammadBnei/agent-fleet/compare/1.16.0...1.17.0) (2026-08-06)


### Features

* **dashboard:** scroll to bottom on human message, pulse on AI message ([b8fd8f5](https://github.com/MohammadBnei/agent-fleet/commit/b8fd8f514716272e9f9f9105e7df3e1cd80c7176))

# [1.16.0](https://github.com/MohammadBnei/agent-fleet/compare/1.15.0...1.16.0) (2026-08-06)


### Features

* **dashboard:** permission-mode selector, slash-command palette, mobile parity ([ead4e30](https://github.com/MohammadBnei/agent-fleet/commit/ead4e30f7c8734e7a62f0119ceca2e53f589975a))

# [1.15.0](https://github.com/MohammadBnei/agent-fleet/compare/1.14.0...1.15.0) (2026-08-06)


### Features

* **dashboard:** card-style TODOS/TOOL CALLS/CHANGES, scroll-to-bottom button ([b480fec](https://github.com/MohammadBnei/agent-fleet/commit/b480fec25c03d234ef9f2d6d706229dac05ecd27))

# [1.14.0](https://github.com/MohammadBnei/agent-fleet/compare/1.13.1...1.14.0) (2026-08-06)


### Bug Fixes

* **dashboard:** optimistic echo for sent human messages ([c3adbef](https://github.com/MohammadBnei/agent-fleet/commit/c3adbef2557582c2beba7c620661a429598a4dbd))
* **dashboard:** regenerate bun.lock, drop stray npm lockfile ([016f77b](https://github.com/MohammadBnei/agent-fleet/commit/016f77b43645221868eb19b386d031a719599a07))
* **dashboard:** remove fake mock data, fix live counter, add PR link and mobile worktrees nav ([ecf3d57](https://github.com/MohammadBnei/agent-fleet/commit/ecf3d57a0d0d5bfd72bf0868b7b54b3c73d98f70))


### Features

* **dashboard:** grid layout, markdown/mermaid rendering, formatted tool JSON ([f82514a](https://github.com/MohammadBnei/agent-fleet/commit/f82514a4cf5f35e06aa8de0eaf8575c5fc1b7117))

## [1.13.1](https://github.com/MohammadBnei/agent-fleet/compare/1.13.0...1.13.1) (2026-08-06)


### Bug Fixes

* **provisioner:** forward CORE_GRPC_ADDR into every worker pod's sidecar ([14b75b8](https://github.com/MohammadBnei/agent-fleet/commit/14b75b8b2ddc2fc29f7c0e923bb59f73566b6a79))
* **sidecar:** make the access-log wrapper support SSE flushing ([358d695](https://github.com/MohammadBnei/agent-fleet/commit/358d69513b74f9697e65b94560942b3afd16d4c3))

# [1.13.0](https://github.com/MohammadBnei/agent-fleet/compare/1.12.0...1.13.0) (2026-08-06)


### Features

* **provisioner,dashboard:** surface the precise provisioning step ([8caeb29](https://github.com/MohammadBnei/agent-fleet/commit/8caeb29c65053d02163af66355e3f477a854f207)), closes [#41](https://github.com/MohammadBnei/agent-fleet/issues/41)

# [1.12.0](https://github.com/MohammadBnei/agent-fleet/compare/1.11.1...1.12.0) (2026-08-06)


### Features

* **local:** add kind-based local testing ground, fix worker Claude auth ([#42](https://github.com/MohammadBnei/agent-fleet/issues/42)) ([e47dc75](https://github.com/MohammadBnei/agent-fleet/commit/e47dc759fb64917037f79ef1bc8bba6290465764))

## [1.11.1](https://github.com/MohammadBnei/agent-fleet/compare/1.11.0...1.11.1) (2026-08-06)


### Bug Fixes

* **core-grpc:** correct core's Service hostname for sidecar/provisioner ([#43](https://github.com/MohammadBnei/agent-fleet/issues/43)) ([61578d0](https://github.com/MohammadBnei/agent-fleet/commit/61578d007b2b3bd06a4981ddf4893e32781f7495))

# [1.11.0](https://github.com/MohammadBnei/agent-fleet/compare/1.10.4...1.11.0) (2026-08-06)


### Features

* **dashboard:** surface task staleness, retry count, and last error ([b030378](https://github.com/MohammadBnei/agent-fleet/commit/b030378b06212d8aaf6072c6cbc1ad2a21bf41a4))

## [1.10.4](https://github.com/MohammadBnei/agent-fleet/compare/1.10.3...1.10.4) (2026-08-06)


### Bug Fixes

* **core:** bound provisioner CreateWorkerPod/TearDownSession with a deadline ([8357d30](https://github.com/MohammadBnei/agent-fleet/commit/8357d308d13ab5603bb35b7e5d71eea973b55e4e))

## [1.10.3](https://github.com/MohammadBnei/agent-fleet/compare/1.10.2...1.10.3) (2026-08-06)


### Bug Fixes

* **core:** close dispatch-loop nudge gaps, widen poll to a true fallback ([3a7ef44](https://github.com/MohammadBnei/agent-fleet/commit/3a7ef44c289bdbae75d18d4952da723c33b08912))

## [1.10.2](https://github.com/MohammadBnei/agent-fleet/compare/1.10.1...1.10.2) (2026-08-05)


### Bug Fixes

* **core:** drop debug log on every empty ClaimNextTask poll ([5d5405d](https://github.com/MohammadBnei/agent-fleet/commit/5d5405d6d529ccf441d896ad8bf2f12e780d1d57))

## [1.10.1](https://github.com/MohammadBnei/agent-fleet/compare/1.10.0...1.10.1) (2026-08-05)


### Bug Fixes

* **sidecar,provisioner:** correct gRPC retry status code spelling, crash-looping every pod on startup ([815a1d3](https://github.com/MohammadBnei/agent-fleet/commit/815a1d354a90299a98561af1fb4747a730ecade5))

# [1.10.0](https://github.com/MohammadBnei/agent-fleet/compare/1.9.0...1.10.0) (2026-08-05)


### Bug Fixes

* **core:** add pod_phase/pod_message to integration tests' inline schema ([e5a7a5e](https://github.com/MohammadBnei/agent-fleet/commit/e5a7a5ecb1d6eae4013d696260f728d005866b3e))
* **worker,sidecar,provisioner:** stop crashing workers on transient core outages ([1a5272c](https://github.com/MohammadBnei/agent-fleet/commit/1a5272c441aa9ca6d2e5885e18ea0bb807630782))


### Features

* **dashboard:** surface worker-pod lifecycle state in the UI ([baa4e51](https://github.com/MohammadBnei/agent-fleet/commit/baa4e51f054bfcb8dcdb2249f4d0cb0307ba1377))

# [1.9.0](https://github.com/MohammadBnei/agent-fleet/compare/1.8.0...1.9.0) (2026-08-05)


### Bug Fixes

* **provisioner:** forward LOG_LEVEL to spawned worker/sidecar pods ([9ad9330](https://github.com/MohammadBnei/agent-fleet/commit/9ad9330e6885cd3abb5af7a8d05bd9503d11dcda))


### Features

* **observability:** LOG_LEVEL-configurable logging fleet-wide ([7bbef08](https://github.com/MohammadBnei/agent-fleet/commit/7bbef08b143e46ed51492bec7b4d3519a9d3fcce))

# [1.8.0](https://github.com/MohammadBnei/agent-fleet/compare/1.7.0...1.8.0) (2026-08-05)


### Features

* **dashboard:** seamless chat, real todos/tool-calls, fix Discord type-leak ([606c769](https://github.com/MohammadBnei/agent-fleet/commit/606c7695d8796507d0b8c1cd147bd6b2b830a83c))

# [1.7.0](https://github.com/MohammadBnei/agent-fleet/compare/1.6.8...1.7.0) (2026-08-05)


### Features

* **dashboard:** add mobile view, real branch/changes/needs-you, session delete ([6d784ac](https://github.com/MohammadBnei/agent-fleet/commit/6d784ac034b10642f5cc3e76c364f9ce5f0b3ec1))

## [1.6.8](https://github.com/MohammadBnei/agent-fleet/compare/1.6.7...1.6.8) (2026-08-05)


### Bug Fixes

* bound-retry sidecar/provisioner->core gRPC calls, surface real task failures via exit code ([a8a0aec](https://github.com/MohammadBnei/agent-fleet/commit/a8a0aec2ba5c3a4afb8ae44098cd23868012355d))

## [1.6.7](https://github.com/MohammadBnei/agent-fleet/compare/1.6.6...1.6.7) (2026-08-05)


### Bug Fixes

* **provisioner:** stop mounting worker/sidecar PVC via per-task subPath ([8e6626f](https://github.com/MohammadBnei/agent-fleet/commit/8e6626feeebc90f8ab151b92ab3b01d86acb4560))

## [1.6.6](https://github.com/MohammadBnei/agent-fleet/compare/1.6.5...1.6.6) (2026-08-05)


### Bug Fixes

* **reliability:** Phase 4 — worker session = plain continuous session ([#0](https://github.com/MohammadBnei/agent-fleet/issues/0)) ([0901f64](https://github.com/MohammadBnei/agent-fleet/commit/0901f649706ffe31907c181046e27c057792e38f)), closes [#8](https://github.com/MohammadBnei/agent-fleet/issues/8) [3/#4](https://github.com/MohammadBnei/agent-fleet/issues/4) [#3](https://github.com/MohammadBnei/agent-fleet/issues/3) [#8](https://github.com/MohammadBnei/agent-fleet/issues/8)

## [1.6.5](https://github.com/MohammadBnei/agent-fleet/compare/1.6.4...1.6.5) (2026-08-05)


### Bug Fixes

* **reliability:** Phase 3 — crash reporting + journal read path ([#1](https://github.com/MohammadBnei/agent-fleet/issues/1)) ([646bb06](https://github.com/MohammadBnei/agent-fleet/commit/646bb06be5ff69a3b1df3fc659d351fd2bd19fd5)), closes [#7](https://github.com/MohammadBnei/agent-fleet/issues/7)

## [1.6.4](https://github.com/MohammadBnei/agent-fleet/compare/1.6.3...1.6.4) (2026-08-05)


### Bug Fixes

* **reliability:** Phase 2 — dashboard Worktrees view + tests ([#2](https://github.com/MohammadBnei/agent-fleet/issues/2)) ([b6dda72](https://github.com/MohammadBnei/agent-fleet/commit/b6dda72845d72c3389d797c5030a37c3b9185d23))

## [1.6.3](https://github.com/MohammadBnei/agent-fleet/compare/1.6.2...1.6.3) (2026-08-05)


### Bug Fixes

* **reliability:** Phase 1 — worker pods become batch/v1.Job ([#11](https://github.com/MohammadBnei/agent-fleet/issues/11)) ([ba71c00](https://github.com/MohammadBnei/agent-fleet/commit/ba71c00a55eeffb5110a728dc37685ed5ecc6c92))

## [1.6.2](https://github.com/MohammadBnei/agent-fleet/compare/1.6.1...1.6.2) (2026-08-05)


### Bug Fixes

* **reliability:** Phase 0 quick wins from reliability-findings.md ([6cb4846](https://github.com/MohammadBnei/agent-fleet/commit/6cb484648bb4f19a867e4aebfeea2694d7db914c)), closes [#5](https://github.com/MohammadBnei/agent-fleet/issues/5) [#6](https://github.com/MohammadBnei/agent-fleet/issues/6) [#7-partial](https://github.com/MohammadBnei/agent-fleet/issues/7-partial) [#9](https://github.com/MohammadBnei/agent-fleet/issues/9) [#10](https://github.com/MohammadBnei/agent-fleet/issues/10) [#7](https://github.com/MohammadBnei/agent-fleet/issues/7)

## [1.6.1](https://github.com/MohammadBnei/agent-fleet/compare/1.6.0...1.6.1) (2026-08-05)


### Bug Fixes

* **deploy:** expose core's gRPC port on its Service ([dbfe71a](https://github.com/MohammadBnei/agent-fleet/commit/dbfe71aeb9ca0619fd7782e5cbe7f664301c39e1))
* **provisioner:** make sidecar a native K8s sidecar to fix worker startup race ([b393a28](https://github.com/MohammadBnei/agent-fleet/commit/b393a284f3c8276f706fb363a3eb722b7f054cae))

# [1.6.0](https://github.com/MohammadBnei/agent-fleet/compare/1.5.1...1.6.0) (2026-08-05)


### Features

* **dashboard:** create tasks from the dashboard, not just Discord ([a87089e](https://github.com/MohammadBnei/agent-fleet/commit/a87089e8cb00820377ce76237a1d3dbe06686c81))

## [1.5.1](https://github.com/MohammadBnei/agent-fleet/compare/1.5.0...1.5.1) (2026-08-05)


### Bug Fixes

* **core:** log discord thread-open failures instead of swallowing them ([efe3cc7](https://github.com/MohammadBnei/agent-fleet/commit/efe3cc75c584c0b9ea4e32098c52cd84231dae19))
* **core:** truncate task description in Discord thread names ([566bb93](https://github.com/MohammadBnei/agent-fleet/commit/566bb935a19bbbae2872385ff206e9e3ac32ca4e))

# [1.5.0](https://github.com/MohammadBnei/agent-fleet/compare/1.4.0...1.5.0) (2026-08-05)


### Bug Fixes

* **core:** retry Discord channel lookup in onReady instead of silently giving up ([8cbfaf1](https://github.com/MohammadBnei/agent-fleet/commit/8cbfaf1f5d2d01108e367e923f769f37dcf4a108))


### Features

* **dashboard:** reskin to the "herd" design ([43e7367](https://github.com/MohammadBnei/agent-fleet/commit/43e73678ecd88176817135320c80ba97a71e2135))

# [1.4.0](https://github.com/MohammadBnei/agent-fleet/compare/1.3.0...1.4.0) (2026-08-04)


### Bug Fixes

* **ci:** avoid shadowing actions/github-script's injected `core` global ([8138675](https://github.com/MohammadBnei/agent-fleet/commit/81386754f7114663b7c273ff18fa268c2881d51c))
* **sidecar:** declare direct dependencies in go.mod ([323551d](https://github.com/MohammadBnei/agent-fleet/commit/323551de95fe6897e843bf628ba4af7fada70df9))


### Features

* **core,provisioner,sidecar,worker,k8s:** shared-PVC provisioner, hub-and-spoke gRPC, continuous streaming sessions ([02a5579](https://github.com/MohammadBnei/agent-fleet/commit/02a55795ed4f721b389b61c4582845201c12f43a))

# [1.3.0](https://github.com/MohammadBnei/agent-fleet/compare/1.2.0...1.3.0) (2026-08-04)


### Features

* **worker,fleet-core,dashboard:** single-session planner + dashboard-answered AskUserQuestion ([d59a96f](https://github.com/MohammadBnei/agent-fleet/commit/d59a96f8f711cb7f83e4a69ffcf03b828605d231))

# [1.2.0](https://github.com/MohammadBnei/agent-fleet/compare/1.1.1...1.2.0) (2026-08-04)


### Features

* **fleet-core,dashboard:** replace REST+SSE dashboard API with ConnectRPC ([d9da126](https://github.com/MohammadBnei/agent-fleet/commit/d9da1267e79a4e7c1472a576fa41ae985081422f))

## [1.1.1](https://github.com/MohammadBnei/agent-fleet/compare/1.1.0...1.1.1) (2026-08-04)


### Bug Fixes

* **ci:** deploy job was silently skipped despite its needs succeeding ([89a2955](https://github.com/MohammadBnei/agent-fleet/commit/89a29552e8d3ac6b11994971e4c185c760bda397))

# [1.1.0](https://github.com/MohammadBnei/agent-fleet/compare/1.0.2...1.1.0) (2026-08-04)


### Bug Fixes

* **ci:** track a dist/.gitkeep placeholder for fleet-core's webui embed ([f40ad31](https://github.com/MohammadBnei/agent-fleet/commit/f40ad3194cc24d63856866ec5249a8bb616ef26b))


### Features

* add web dashboard backend + SPA to fleet-core ([6d2c920](https://github.com/MohammadBnei/agent-fleet/commit/6d2c920ffd640aa458bc93309b74a4ce2b64a98f))

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
