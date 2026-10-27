# Changelog

## [0.12.0](https://github.com/sserkanml/backflow/compare/v0.11.0...v0.12.0) (2026-10-27)


### Features

* **policy:** a policy whose Argo CD Secret cannot be read is not Ready (SecretForbidden) ([3db26b1](https://github.com/sserkanml/backflow/commit/3db26b1af5b10d31ac3ce77088cdf54cea785164))
* **scm:** get-only access to Secrets, cluster-wide or per namespace with secretNamespaces ([747ce03](https://github.com/sserkanml/backflow/commit/747ce032569e81d748963c829efd077cc57b46b4))
* **scm:** read Secrets by name without the cache and drop the Secret watches ([d7366bc](https://github.com/sserkanml/backflow/commit/d7366bc97131751a0d28688610565bc6069f08eb))


### Documentation

* **scm:** Permissions section, Secret rotation delay and the upgrade note ([a5aa5ca](https://github.com/sserkanml/backflow/commit/a5aa5caa0e8394539f20fe88f95998c1f6684d35))

## [0.11.0](https://github.com/sserkanml/backflow/compare/v0.10.0...v0.11.0) (2026-10-27)


### Features

* **drift:** an in-place JSON editor ([caf23b6](https://github.com/sserkanml/backflow/commit/caf23b6a22a6a248e06b329d044b867133685297))
* **drift:** edit JSON files of Kustomize and Helm sources ([9a8deab](https://github.com/sserkanml/backflow/commit/9a8deab6ce7196daec34766e0ef07586a529e2bb))
* **drift:** edit JSON manifests of a Directory source ([8444940](https://github.com/sserkanml/backflow/commit/84449408b243d2f36d2d3d6277ee94d0d2a2be04))


### Documentation

* **drift:** JSON files ([8aa8ac1](https://github.com/sserkanml/backflow/commit/8aa8ac18af32be5049f1bee7488dad674516850f))

## [0.10.0](https://github.com/sserkanml/backflow/compare/v0.9.1...v0.10.0) (2026-10-27)


### Features

* **api:** status.mapping.application, where an Application option sets a value ([4ed92be](https://github.com/sserkanml/backflow/commit/4ed92bef5b9e362dbb8bf97c7cbea6267bbf63f4))
* **drift:** diagnose values set in the Application ([5eeac8d](https://github.com/sserkanml/backflow/commit/5eeac8d148676031f0e9e4aebd8e92eb547dc819))
* **drift:** find the Git file of an Application option through its parent ([cf3cd0a](https://github.com/sserkanml/backflow/commit/cf3cd0a21fe8f06530f05502098eb1299b6246ea))
* **drift:** name the Helm Application option that sets a value ([d93bb79](https://github.com/sserkanml/backflow/commit/d93bb79302f8d46be69474b6ea027724d8b45699))
* **drift:** name the Kustomize Application option that sets a value ([d315f05](https://github.com/sserkanml/backflow/commit/d315f05a0daf24db800e27d13abe944fa0682430))
* **drift:** shorter diagnosis messages ([28fb480](https://github.com/sserkanml/backflow/commit/28fb480c0f04d883b512e4d5efd85255be0c645d))


### Documentation

* **drift:** application values as implemented ([1fbc0cb](https://github.com/sserkanml/backflow/commit/1fbc0cb9f885d041f2bdbb1fa5257d85dbdf9553))
* **drift:** application values design ([6a13c88](https://github.com/sserkanml/backflow/commit/6a13c883cd7c23c564a6f962e57d8f73c30c2ccc))
* **drift:** noCache is adopted ([e88b22b](https://github.com/sserkanml/backflow/commit/e88b22bad59cc830b8b4b7d2aec9e9568142f917))
* **drift:** values set in the Application ([70f2643](https://github.com/sserkanml/backflow/commit/70f2643adb59cf95d96fc1fc38e42dd706b8df7f))

## [0.9.1](https://github.com/sserkanml/backflow/compare/v0.9.0...v0.9.1) (2026-10-25)


### Performance

* **drift:** fresh base renders with noCache instead of marker commits ([f4e6bb1](https://github.com/sserkanml/backflow/commit/f4e6bb168199570c7be9fe12ae29294b128e5b11))


### Documentation

* **drift:** noCache for the base renders ([a6015c3](https://github.com/sserkanml/backflow/commit/a6015c3f90c580b27ce30dc1ad54c4059436c20d))

## [0.9.0](https://github.com/sserkanml/backflow/compare/v0.8.0...v0.9.0) (2026-10-25)


### Features

* **api:** spec.mapping.scratchBranches and per-source status of Applications and proposals ([e968e69](https://github.com/sserkanml/backflow/commit/e968e699e8d902ac2731c56cfbe0335a725d8d81))
* **drift:** chart defaults from Argo CD for charts that are not in Git ([e968e69](https://github.com/sserkanml/backflow/commit/e968e699e8d902ac2731c56cfbe0335a725d8d81))
* **drift:** git-ahead per source; a chart version change is reported as a change of the Application ([e968e69](https://github.com/sserkanml/backflow/commit/e968e699e8d902ac2731c56cfbe0335a725d8d81))
* **drift:** leave Helm and Argo CD hook resources out of verification ([e968e69](https://github.com/sserkanml/backflow/commit/e968e699e8d902ac2731c56cfbe0335a725d8d81))
* **drift:** map multi-source Helm Applications whose values are in a Git ref source, verified through scratch branches ([e968e69](https://github.com/sserkanml/backflow/commit/e968e699e8d902ac2731c56cfbe0335a725d8d81))


### Documentation

* **drift:** multi-source mapping and the Argo CD permission it needs ([e968e69](https://github.com/sserkanml/backflow/commit/e968e699e8d902ac2731c56cfbe0335a725d8d81))

## [0.8.0](https://github.com/sserkanml/backflow/compare/v0.7.0...v0.8.0) (2026-10-24)


### Features

* **api:** spec.kustomize editBases and addPatches ([0b3b3dd](https://github.com/sserkanml/backflow/commit/0b3b3dde55d69d2cfab4d97b1a98e9b868110c74))
* **drift:** gitrepo creates the files a commit lists as new ([92f8436](https://github.com/sserkanml/backflow/commit/92f843637785b1017926054f337e68dc671bb351))
* **drift:** map Kustomize proposals ([837188e](https://github.com/sserkanml/backflow/commit/837188e95e78f0b1c610a5fb652c2b6130c792f3))
* **kustomize:** describe edits per tier for status and merge requests ([69abe61](https://github.com/sserkanml/backflow/commit/69abe61fb0e097f8a0d025d458399c17ed9f3095))
* **kustomize:** locate resources by origin annotations ([c8593c6](https://github.com/sserkanml/backflow/commit/c8593c6a86a813c13fd8fbfa1db364b4d8921313))
* **kustomize:** map to settings, patches, resource files, bases and a new patch ([8668c0b](https://github.com/sserkanml/backflow/commit/8668c0b58b3293c6d5107cbf5d73b57b50cb0d61))
* **kustomize:** try a new patch before a base other Applications use ([d3b9def](https://github.com/sserkanml/backflow/commit/d3b9defaf74dcb4d416c640f06dc56e21ea16ff1))


### Refactoring

* **drift:** context writer for edits that list a new file in another ([b66f0b2](https://github.com/sserkanml/backflow/commit/b66f0b28dbecebb5c245ff61e8175f46ae9f1f29))
* **drift:** source-neutral render error and engine hooks for tiered sources ([d0fe21a](https://github.com/sserkanml/backflow/commit/d0fe21abb882163fe3d243e341b949b34c8127ca))


### Documentation

* **kustomize:** README section and design notes ([2873dbc](https://github.com/sserkanml/backflow/commit/2873dbc0961e9ce98705ed1af7d79ad664f17395))

## [0.7.0](https://github.com/sserkanml/backflow/compare/v0.6.0...v0.7.0) (2026-10-23)


### Features

* **drift:** git-ahead check for kustomize sources ([b78a8ae](https://github.com/sserkanml/backflow/commit/b78a8aeaab7a878c85058453eba4bd80dfce00a3))


### Refactoring

* **drift:** move KeyPath and RenderValue into internal/rendermap ([539c016](https://github.com/sserkanml/backflow/commit/539c0169f68db989b4bf448e8c4f9f27442984f2))
* **drift:** move the Argo CD renderer into internal/rendermap ([004151b](https://github.com/sserkanml/backflow/commit/004151b7ee6979e7e057d410459eb8570eeaec65))
* **drift:** move the verification engine into internal/rendermap ([57d6a2a](https://github.com/sserkanml/backflow/commit/57d6a2ad47fe9ac9b997faf772a84ea9df64f30b))
* **drift:** put the engine behind a candidate provider and a file writer ([f5c74ac](https://github.com/sserkanml/backflow/commit/f5c74acf756710c2100651b079576e9ad85c071a))


### Documentation

* **drift:** kustomize mapping design ([f553c44](https://github.com/sserkanml/backflow/commit/f553c447e6d7b6fc70426a42fd46d1dcfd9d323b))

## [0.6.0](https://github.com/sserkanml/backflow/compare/v0.5.1...v0.6.0) (2026-10-23)


### Features

* **helmmap:** accept structured field changes ([8a46541](https://github.com/sserkanml/backflow/commit/8a465411ea8f9dec59a258557ad80d1ba06038fa))
* **helmmap:** report values that affect other fields ([9c3c08d](https://github.com/sserkanml/backflow/commit/9c3c08d8bc7cf5f32086be3fd6430fb1f1affe7c))
* **helmmap:** subtree candidates for maps and lists ([76ba1ed](https://github.com/sserkanml/backflow/commit/76ba1eddf8e51a84ac6ccca2ddfd03c03b23074d))
* **helmmap:** write maps and lists to values files ([b094118](https://github.com/sserkanml/backflow/commit/b094118f26344f9e25b4d31497a7c5a03720169c))


### Documentation

* **helmmap:** live-only additions are not reported by Argo CD ([b48b6f2](https://github.com/sserkanml/backflow/commit/b48b6f26ea2642c6322db47b1e02ee9768e62792))
* **helmmap:** structured values mapping ([0cb21a6](https://github.com/sserkanml/backflow/commit/0cb21a6f82d2019d6ef90eb647d4692bab31409f))

## [0.5.1](https://github.com/sserkanml/backflow/compare/v0.5.0...v0.5.1) (2026-10-22)


### Bug Fixes

* changelog.md ([3e562e4](https://github.com/sserkanml/backflow/commit/3e562e436db339598e08349ad6c9b09cc151cda6))

## [0.5.0](https://github.com/sserkanml/backflow/compare/v0.4.0...v0.5.0) (2026-10-22)


### Features

* **api:** spec.helm.addOverrides ([04c8cf0](https://github.com/sserkanml/backflow/commit/04c8cf09d68638ac5398e24b8273c30e24a7ec66))
* **drift:** map a default only the chart defines to an override, and say so in the merge request ([f706e4e](https://github.com/sserkanml/backflow/commit/f706e4eaf9c2d5997ad4461227f232980f9d2ea9))
* **drift:** reuse the mapping at the head of the target branch when nothing it depends on changed ([4f33b33](https://github.com/sserkanml/backflow/commit/4f33b33c343b58cc1a32f2da63fa2b409c81f92f))
* **helmmap:** add overrides to spec.helm.targetValuesFile when it is set ([baa6809](https://github.com/sserkanml/backflow/commit/baa68097058b394208935f7047477359c7e51050))
* **helmmap:** AddValue adds a key to a value file, keeping comments and style ([468f37b](https://github.com/sserkanml/backflow/commit/468f37b612e37f87b3373da85df656a82e7f2126))
* **helmmap:** override candidates for defaults only the chart defines ([8a05354](https://github.com/sserkanml/backflow/commit/8a053547ec242367d18b0c9c51f60d9cddbdff19))
* **helmmap:** read the defaults of packaged subcharts, and say when a dependency is not in the repository ([728c89a](https://github.com/sserkanml/backflow/commit/728c89af3a22812453193d856a6be634af9ddd91))
* **helmmap:** verify override edits as a last resort, after edits of existing lines ([5bfabf6](https://github.com/sserkanml/backflow/commit/5bfabf6c169e4fc86818309eefda092ee5025aa9))
* **helm:** override chart defaults in the Application's value file ([d7d20cc](https://github.com/sserkanml/backflow/commit/d7d20ccfec68d89008b0ef639507b8566e3caded))


### Bug Fixes

* **helmmap:** share the renders between the attempts of a partial mapping; log the renders ([edd9db4](https://github.com/sserkanml/backflow/commit/edd9db41a4e34febd74a9bdb72af9f5e8c4ef06f))


### Refactoring

* **helmmap:** build the Fields of a proposal in helmmap, so tests can run the same path ([d0dbb97](https://github.com/sserkanml/backflow/commit/d0dbb97df26c8a46f327715ad30f8815c59f1cd3))


### Documentation

* overrides of chart defaults, and the e2e for them ([62f7e16](https://github.com/sserkanml/backflow/commit/62f7e16a24d9287eec1415a568f6c0b491bf6f7f))

## [0.4.0](https://github.com/sserkanml/backflow/compare/v0.3.1...v0.4.0) (2026-10-18)


### Features

* **drift:** optionally propose the part of a drift that can be mapped and list the rest (spec.mapping.allowPartial) ([abe32a0](https://github.com/sserkanml/backflow/commit/abe32a06e6ab452d02202b7a8d7e83c6c6bd10fc))

## [0.3.1](https://github.com/sserkanml/backflow/compare/v0.3.0...v0.3.1) (2026-10-18)


### Bug Fixes

* **drift:** keep the Proposed condition in step with the phase ([#9](https://github.com/sserkanml/backflow/issues/9)) ([4707fe1](https://github.com/sserkanml/backflow/commit/4707fe1c74f43695dbce545b976b44e4f14a1668))

## [0.3.0](https://github.com/sserkanml/backflow/compare/v0.2.0...v0.3.0) (2026-10-17)


### Features

* **helm:** map a change inside a composed value, such as an image tag, to its values key ([38bde51](https://github.com/sserkanml/backflow/commit/38bde5147e26d08eafafb5547945a54d6ab8b1d5))
* **helm:** optionally accept label and annotation changes that come with an edit ([38bde51](https://github.com/sserkanml/backflow/commit/38bde5147e26d08eafafb5547945a54d6ab8b1d5))

## [0.2.0](https://github.com/sserkanml/backflow/compare/v0.1.2...v0.2.0) (2026-10-16)


### Features

* **helm:** map drift on Helm Applications to values files ([ccd144d](https://github.com/sserkanml/backflow/commit/ccd144d17786e4236a8cc03d53521599afb5534a))


### Bug Fixes

* **drift:** keep asking for the recheck of a drift that is waiting to become stable ([ccd144d](https://github.com/sserkanml/backflow/commit/ccd144d17786e4236a8cc03d53521599afb5534a))
* **drift:** retry a push the Git server declines before failing the proposal ([ccd144d](https://github.com/sserkanml/backflow/commit/ccd144d17786e4236a8cc03d53521599afb5534a))

## [0.1.2](https://github.com/sserkanml/backflow/compare/v0.1.1...v0.1.2) (2026-10-16)


### Bug Fixes

* **drift:** do not propose a change again while its merge is not synced yet ([7ce197c](https://github.com/sserkanml/backflow/commit/7ce197cb82fe9b4552f3041b4002f588e0515e36))

## [0.1.1](https://github.com/sserkanml/backflow/compare/v0.1.0...v0.1.1) (2026-10-15)


### Bug Fixes

* **drift:** propose ReportOnly proposals when the policy mode changes ([2b4e89b](https://github.com/sserkanml/backflow/commit/2b4e89b898f59db10bde1bea3adbe1ab2b292f05))

## 0.1.0 (2026-10-11)


### Features

* add .gitignore file ([a793220](https://github.com/sserkanml/backflow/commit/a79322094e125296fe5a4a7c415748428b8860c7))
* add backflowpolicy controller ([07b1a40](https://github.com/sserkanml/backflow/commit/07b1a40690b04913b482ad18a74e6e116094058f))
* add backflowpolicy type ([34fbeca](https://github.com/sserkanml/backflow/commit/34fbeca6631556ebd1da47a7d3a39dd994dd505c))
* add base crds ([ec39a90](https://github.com/sserkanml/backflow/commit/ec39a9061f02328e5e76ace2daecba12de5c4b39))
* add driftproposal type ([e821e8b](https://github.com/sserkanml/backflow/commit/e821e8ba0c072fae828b3c9f8789bb087c84116a))
* add folder structure ([defaec5](https://github.com/sserkanml/backflow/commit/defaec550dc61c566b8cfadde448f83fa729c341))
* add license file ([c0a1064](https://github.com/sserkanml/backflow/commit/c0a10642e22349efc7778366ee6e624098fdeb08))
* add scmconnection controller strcuture ([d31ac81](https://github.com/sserkanml/backflow/commit/d31ac81b8dcbe40f322600a9bae7a0e90a6d6f45))
* add scmconnection type ([42db2ce](https://github.com/sserkanml/backflow/commit/42db2ce40b8de580bcbd764cfbdb2d2c14f299da))
* add test script ([72785c6](https://github.com/sserkanml/backflow/commit/72785c6f13d510e15f2b90b2ae4848326166b367))
* **api:** add argoCD connection to BackflowPolicy and dev token setup ([c01ea05](https://github.com/sserkanml/backflow/commit/c01ea055bddef22009a603c2b75d7cf515131559))
* **api:** add validation flag for DriftProposal type ([790a7d4](https://github.com/sserkanml/backflow/commit/790a7d4f2d49fcfa0cf4a32b13b4950887c150a8))
* **api:** make DriftProposal spec immutable ([92f347d](https://github.com/sserkanml/backflow/commit/92f347d81b38d546746fb7c0ac02c36089aac207))
* **api:** record mapping diff and reason in DriftProposal status ([14e25d0](https://github.com/sserkanml/backflow/commit/14e25d0924703066f73092b728bd7875a18a3c1a))
* change project architecture structure ([7efd900](https://github.com/sserkanml/backflow/commit/7efd900483016649e54630e2cffc79d636529ce3))
* **chart:** Helm chart for the operator ([8c922b2](https://github.com/sserkanml/backflow/commit/8c922b2ee3dfac7b223cfec810f6ae3741bf8db7))
* **drift:** add Argo CD client, field diff and kind filtering ([2beeec2](https://github.com/sserkanml/backflow/commit/2beeec2869fd699dcf0913b86e7f481b93fb386a))
* **drift:** add drift controller and open-proposal counting ([a6957e0](https://github.com/sserkanml/backflow/commit/a6957e06672dc935fb341407003c2974a8e83b5b))
* **drift:** build commit messages and merge request bodies ([f1514fa](https://github.com/sserkanml/backflow/commit/f1514fa4a626ce7458523476cce43fb8107b48e7))
* **drift:** follow merge requests and retire them with their proposals ([a11f7bb](https://github.com/sserkanml/backflow/commit/a11f7bb17a599a44c51d310e5429c468af757e01))
* **drift:** keep a change in its merge request when a sync of a new revision resets the cluster ([3245cc8](https://github.com/sserkanml/backflow/commit/3245cc874fdfa60e453c90682eaef98ae56df547))
* **drift:** make the merge request polling interval configurable ([dd1ccbe](https://github.com/sserkanml/backflow/commit/dd1ccbe31a3fdde4ea032648da25e3dfcf5e388e))
* **drift:** map Directory-source proposals to the defining file ([0c24d2e](https://github.com/sserkanml/backflow/commit/0c24d2e2c2bfff99bd69019c9d69e894ffddb2b8))
* **drift:** propose a drift only after it has been stable for the batch window ([241af47](https://github.com/sserkanml/backflow/commit/241af472f9fb15e43975c6585f8bc5ec98bf4f7a))
* **drift:** propose mapped DriftProposals as merge requests or direct commits ([2ea0b66](https://github.com/sserkanml/backflow/commit/2ea0b6662faf6339a68a22fb3679a91f61b64b01))
* **drift:** reuse the branch an earlier run pushed after the target moved ([efc8d60](https://github.com/sserkanml/backflow/commit/efc8d60bdffe6499ed342b77662b15481d0c4d1a))
* **drift:** tell which side of recorded changes a resource is on ([1aa2661](https://github.com/sserkanml/backflow/commit/1aa2661fbf354f340de190c17de5708135f108d6))
* **gitrepo:** check that the repository cache is writable at startup ([eb2ab3c](https://github.com/sserkanml/backflow/commit/eb2ab3cfa95217f3b26d1b1ade9204344ccc75c2))
* **gitrepo:** create commits and push branches for merge requests ([ae698eb](https://github.com/sserkanml/backflow/commit/ae698ebacdf050751f140668ab3a316a7902d05c))
* **gitrepo:** honour ScmConnection.caSecretRef when reading repositories ([1d7d3f5](https://github.com/sserkanml/backflow/commit/1d7d3f5ce74c3ebbeed8a206b02fe286b22bc340))
* **gitrepo:** read repositories at an exact commit with go-git ([0609c18](https://github.com/sserkanml/backflow/commit/0609c187f1b4aa882210e290d350d5315cd092b7))
* **gitrepo:** read repositories at an exact commit with go-git ([4c47039](https://github.com/sserkanml/backflow/commit/4c47039a2d4b13a8fda75f663ccfc974ce14c6be))
* **gitrepo:** read the default branch and commit info ([bfbbbf7](https://github.com/sserkanml/backflow/commit/bfbbbf7f1b6aa8cf10fa8dea4263623286f5df3f))
* **gitrepo:** tell whether a commit is an ancestor and which files differ ([8a7cf0c](https://github.com/sserkanml/backflow/commit/8a7cf0c3f9b779c15dc44c512143649458dfbac4))
* **gitrepo:** tell whether a directory holds a symbolic link ([c9ac9cf](https://github.com/sserkanml/backflow/commit/c9ac9cf14c1a6e054c66a19bcbb66cf2692d2fe9))
* **mapping:** map Directory-source drift to the defining file ([711a3fe](https://github.com/sserkanml/backflow/commit/711a3fe8136d34c9ddec0a5d3a7f93b088631964))
* **scm:** add provider interface and GitLab/GitHub merge request clients ([cb5ad44](https://github.com/sserkanml/backflow/commit/cb5ad448fb86bbe529dadfbd3cbc4d87006eb3f8))


### Bug Fixes

* **api:** let the batch window default apply to policies built in Go ([8a29520](https://github.com/sserkanml/backflow/commit/8a29520572ae6ea0642bfa9dea6f5f235c2830cb))
* **deploy:** give the manager a writable repo cache and realistic resources ([d388ab5](https://github.com/sserkanml/backflow/commit/d388ab5de649110b1d690dcef40f08764dc2dfdc))
* **drift:** a recorded branch is never renamed ([fb14cf1](https://github.com/sserkanml/backflow/commit/fb14cf1bc4dd129daf0868d739ced0dc00110d55))
* **drift:** classify transient errors correctly and keep messages fresh ([231ec2a](https://github.com/sserkanml/backflow/commit/231ec2a8ec0ace7c0c32489bde7635615f702b64))
* **drift:** close an MR found by branch only if the branch is ours ([0420dbc](https://github.com/sserkanml/backflow/commit/0420dbc98ddcb516db94bc6a7387cacef7449817))
* **drift:** decide "caught up" per resource, not per Application ([aad6156](https://github.com/sserkanml/backflow/commit/aad61560083269dc080554a7ad58aa72fe6b26b0))
* **drift:** do not end a DirectCommit proposal on a failed history walk ([7fe110e](https://github.com/sserkanml/backflow/commit/7fe110eb40c88c41dcdac99d0c2772056b80c856))
* **drift:** do not mistake a pending Git commit for cluster drift ([82d5f3b](https://github.com/sserkanml/backflow/commit/82d5f3b12b60b03f98ad7d0fe11a23f0202af326))
* **drift:** do not reopen a drift whose proposal ended without a fix ([f651025](https://github.com/sserkanml/backflow/commit/f6510258a3bf3a2feed0fa39e7c39c05ae1b885e))
* **drift:** keep in-memory state across short Ready flaps ([09fc963](https://github.com/sserkanml/backflow/commit/09fc963f5f87fee3752b64f74044cfe747335cf1))
* **drift:** keep repo URLs with credentials out of everything ([e22fc95](https://github.com/sserkanml/backflow/commit/e22fc952383923ae210181d610fb6293bda50c24))
* **drift:** match DirectCommit restarts by UID as well as name ([f05ae3d](https://github.com/sserkanml/backflow/commit/f05ae3dcfbd8f06ab746960f1c8d5333a69f2e8b))
* **drift:** never adopt a merge request from an earlier proposal with the same name ([17bac38](https://github.com/sserkanml/backflow/commit/17bac3863f93aa46a16fb41633171975fb5db232))
* **drift:** never treat a failed or partial sync as caught up ([fd8ab69](https://github.com/sserkanml/backflow/commit/fd8ab697fed99e8c776056c2fcd7332c39682856))
* **drift:** only delete branches we own ([24ee83b](https://github.com/sserkanml/backflow/commit/24ee83b6135d1fbcb84252847d2a5a78bdc84ff0))
* **drift:** pause when the Application source changed without a new revision ([4540ab9](https://github.com/sserkanml/backflow/commit/4540ab92b83c4d8b81ca6cebb12a72a1392bc102))
* **drift:** recover proposals orphaned before their MR was recorded ([b1779ae](https://github.com/sserkanml/backflow/commit/b1779ae9de8dd6e7bed663961d2ee90b255fbf95))
* **drift:** reject multi-source Applications early ([0bb603d](https://github.com/sserkanml/backflow/commit/0bb603df90cdba695daccc40302494b80fa80ab4))
* **drift:** retry a repository that is not found instead of giving up ([e0b405d](https://github.com/sserkanml/backflow/commit/e0b405dbaae5fc942a1fd081dcae0c6da7b8eca5))
* **drift:** scope proposals of shared resources per Application ([e3564fd](https://github.com/sserkanml/backflow/commit/e3564fd04c41137f2c9875ad7893c5e524cca68c))
* **drift:** stamp proposals with the fresh compared revision ([10c372f](https://github.com/sserkanml/backflow/commit/10c372faaa1abf06d6401674e9f4533ed8a16b3d))
* **drift:** stop leaking connections and memory ([17a43ad](https://github.com/sserkanml/backflow/commit/17a43ad4f55d9ffa23e52017989b70cdb904fd95))
* **drift:** treat jsonnet Directory sources as unchecked ([1ede70b](https://github.com/sserkanml/backflow/commit/1ede70b985f04b04cb7ea9e4d7d1180c2669947d))
* **gitrepo:** make commits deterministic and adopt a branch that holds the same change ([018d237](https://github.com/sserkanml/backflow/commit/018d237d4b48e1ce68b274ea5560f73d158d26dc))
* **gitrepo:** report network errors on fetch by SHA as unavailable ([0ce0f68](https://github.com/sserkanml/backflow/commit/0ce0f68d2a6035d4e09f939b4adae907e170d3b9))
* **hack:** send JSON content type when creating the Argo CD token ([70d403c](https://github.com/sserkanml/backflow/commit/70d403c722c59a96badeb747a86edfeb68ca8f5f))
* **mapping:** block scalar edge cases ([356fd63](https://github.com/sserkanml/backflow/commit/356fd63748155183f6825c220e0aeed733d3f2ef))
* **mapping:** do not trim block scalar content ([fe7504e](https://github.com/sserkanml/backflow/commit/fe7504eb4a83e53fae48c7a1ffe83672d5d465e7))
* **mapping:** reject repository URLs that contain credentials ([d7e5241](https://github.com/sserkanml/backflow/commit/d7e52412481b7882b694c8179c12c2b5c9e3a799))
* **scm:** edge cases ([1cc996b](https://github.com/sserkanml/backflow/commit/1cc996b5634300b83230f3d7e88c4f80a31b5ec8))
* **scm:** treat a GitLab merge request that is merging as open ([9b2ae8d](https://github.com/sserkanml/backflow/commit/9b2ae8d50920388324bc59a988327128312291bc))


### Performance

* **manager:** serve unstructured Application reads from the cache ([fdc9096](https://github.com/sserkanml/backflow/commit/fdc9096c1c9af6b5c3dd1cba679519902aaa3897))


### Refactoring

* **drift:** keep the new code within the lint limits ([3380c81](https://github.com/sserkanml/backflow/commit/3380c81eef95f1998defff20cf8722851f2316a5))
* **drift:** make the DriftProposal controller the only status writer ([c23b36b](https://github.com/sserkanml/backflow/commit/c23b36b749dde9cb6a1890b7d223224065330771))


### Documentation

* describe running in-cluster ([9bf5118](https://github.com/sserkanml/backflow/commit/9bf5118c7b8a37f0a79f77343363e4a89e2dfebc))
* explain the stability rule ([9bd0928](https://github.com/sserkanml/backflow/commit/9bd092809d2ecbcd4f256ca0caca056211f9decc))
* explain when drift detection is paused ([bc1de71](https://github.com/sserkanml/backflow/commit/bc1de71ed9dd89614aed8031e1c05295b74ec158))
* list the limitations that apply ([4e1fcdd](https://github.com/sserkanml/backflow/commit/4e1fcdd362f1a1e671fb072205f22ddddbd0b3b1))
* README ([2fc20e4](https://github.com/sserkanml/backflow/commit/2fc20e409982fcfb9f487b17d6f25ea8bceac4e2))
* README for the Helm install ([a1e402f](https://github.com/sserkanml/backflow/commit/a1e402f1945cf9a9bc53ff797b4b63e62af26258))
* **samples:** replace the scaffolded samples with working examples ([1e28074](https://github.com/sserkanml/backflow/commit/1e28074b29e2b176d91250e025f4667ad2a3a301))
