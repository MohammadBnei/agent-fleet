# Changelog

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
