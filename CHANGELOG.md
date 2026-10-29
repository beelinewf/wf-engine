# Changelog

All notable changes to this project are documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).


## [1.2.0] - 2026-10-29


### Features

- Add local users with email verify (#75) ([f6571ce](https://github.com/beelinewf/wf-engine/commit/f6571ce531536274b4310a43726db4741b267c4f))
- Add public reads for anon callers (#76) ([dd1d735](https://github.com/beelinewf/wf-engine/commit/dd1d735c2168afa8ebad74df0a8bab4b5b140b13))
- Add workflow mail send (#77) ([372a6d8](https://github.com/beelinewf/wf-engine/commit/372a6d82d8970231de0c6a3e09c5c719f316b75f))

## [1.1.1] - 2026-10-28


### Bug Fixes

- Ship tzdata in runtime image (#73) ([e264cf6](https://github.com/beelinewf/wf-engine/commit/e264cf6ba4fb026c9f5edf0ba6d031273a3b0078))

### Other

- Release v1.1.0 (#69) ([e52ad45](https://github.com/beelinewf/wf-engine/commit/e52ad451647f2b16a716018a3971bd35e44d2343))
- Add banner image to top (#72) ([3be18a0](https://github.com/beelinewf/wf-engine/commit/3be18a0e7813c003c42cf9a69139d39b79da7c3e))

## [1.1.0] - 2026-10-28


### Bug Fixes

- Add missing tables to test schema ([2cdb72a](https://github.com/beelinewf/wf-engine/commit/2cdb72a5e4bb474166c22aa5f9d5cdfd8fd08b84))

### Features

- Manage roles over the API ([3e1a835](https://github.com/beelinewf/wf-engine/commit/3e1a835dcc9c708f674843ae2f80866550479f99))
- Manage roles over the API (#68) ([a0971ee](https://github.com/beelinewf/wf-engine/commit/a0971eec57c79636461365c4901ec26d267e1a60))

### Other

- Bump go-jose/v4 to v4.1.6 ([8649773](https://github.com/beelinewf/wf-engine/commit/86497739e2bf44e8f5bc40664df234e04cbd9260))

## [1.0.0] - 2026-10-27


### Bug Fixes

- Use postgres URL for dev_url ([22d8aba](https://github.com/beelinewf/wf-engine/commit/22d8abad811d2d6fd8da4f8efb9228667bd826d1))
- Blank output defaults to node id (#8) ([732f0c6](https://github.com/beelinewf/wf-engine/commit/732f0c66c21876581209cbccd13fb9781698dd47))
- Input nodes use output_property (#13) ([e048774](https://github.com/beelinewf/wf-engine/commit/e048774e8b8e9abd429553e7f5adcc3a1924ee7e))
- Require absolute allowlist paths (#22) ([aa7427c](https://github.com/beelinewf/wf-engine/commit/aa7427cb49945d584e9e96d7f1fbbecb5c7a21a4))
- Reject non-serializable values (#23) ([8f41e38](https://github.com/beelinewf/wf-engine/commit/8f41e38cc32ffcca288378e82ee4921365be617f))
- Retry checkpoint on conflict (#24) ([5c77327](https://github.com/beelinewf/wf-engine/commit/5c77327a657e1036921114a5d137c7197092cf39))
- Use literal in upsert predicate (#37) ([5a42051](https://github.com/beelinewf/wf-engine/commit/5a42051f9e22f5c0eb43243bb3b74e96fc6363fc))
- Wire dispatcher options from config (#40) ([715d52f](https://github.com/beelinewf/wf-engine/commit/715d52f4a1da424c1304ce3787029ea76898aca7))
- Map unknown id to ErrNotFound (#53) ([785d399](https://github.com/beelinewf/wf-engine/commit/785d3993724aae9c4ae04d6e5c8ec43e2d93f5ac))
- Union allowed_users with allowed_roles (#60) ([e2a8b7d](https://github.com/beelinewf/wf-engine/commit/e2a8b7d5b9ee267aca1e159a82e672aa948e986b))
- Add boot and shutdown timeouts (#63) ([a2adf3c](https://github.com/beelinewf/wf-engine/commit/a2adf3caa5e42f5aacbb7d22dde43a243c80c6f2))
- Fix/security (#64) ([a2d384a](https://github.com/beelinewf/wf-engine/commit/a2d384ac7dc0ef6cd7a6ae8b4933ecbc0a0a0f9d))
- Skip commit when changelog is unchanged (#66) ([be0eb97](https://github.com/beelinewf/wf-engine/commit/be0eb97530013e8ac2a821189fc9c4c054ffe7ac))
- Update git-cliff-action pin to working v4 SHA ([711ba93](https://github.com/beelinewf/wf-engine/commit/711ba9344f440d14c9bc4d132234fb9164a1b9f9))

### Features

- Bootstrap simpwf workflow engine ([ce65980](https://github.com/beelinewf/wf-engine/commit/ce659805fcb63b9627bae5a2dcc7db13589b35d6))
- Feat/rollback (#1) ([fc26899](https://github.com/beelinewf/wf-engine/commit/fc268997beb31cbaadb21a6f0fbf4329167cd343))
- Add lean context mode (#5) ([d78c6b4](https://github.com/beelinewf/wf-engine/commit/d78c6b4b997c5e07be92d893e2248ab6f2f77f7c))
- Add input form validation (#9) ([7888318](https://github.com/beelinewf/wf-engine/commit/7888318fb4de39e4e078668facebf08dbd85437c))
- Add run statistics endpoint (#10) ([8854f9b](https://github.com/beelinewf/wf-engine/commit/8854f9b1633f0865fc8c43d5564a7bdbbf390ed0))
- Add debug step-through mode (#12) ([52dc373](https://github.com/beelinewf/wf-engine/commit/52dc37382863302dc44a96b12457118ba9ef88f2))
- Feat/custom node (#14) ([4fd37e3](https://github.com/beelinewf/wf-engine/commit/4fd37e3003bba38dac63d5db26b4e4a9659dabf5))
- Add instance secret snapshots (#15) ([89e6f3b](https://github.com/beelinewf/wf-engine/commit/89e6f3b69d97d526fb8ab523291f1701305a170e))
- Serve node JSON Schemas on definition reads (#16) ([134375b](https://github.com/beelinewf/wf-engine/commit/134375bf82b4d53b750de4bd93350e25d646c788))
- Add OIDC bearer auth, RBAC, and instance ownership (#17) ([2b99575](https://github.com/beelinewf/wf-engine/commit/2b995751883db39f5e6d0725048ce940b10a683e))
- Anonymous delivery for public nodes (#18) ([9c51f26](https://github.com/beelinewf/wf-engine/commit/9c51f261ba7ec4c45a4fab68ce135223ef228ee3))
- Add TypeScript context endpoint (#19) ([6dad110](https://github.com/beelinewf/wf-engine/commit/6dad110f3840ed00e56040cde9a1063468b07a9e))
- Feat/parallel (#20) ([753c83c](https://github.com/beelinewf/wf-engine/commit/753c83c3cc5429e1a253da7a67f35b9a05f789eb))
- Add cron-based triggers (#21) ([352a3f4](https://github.com/beelinewf/wf-engine/commit/352a3f4bfb957b98d51eebf96e5e8c24a7404514))
- Env wins per key, tune dispatchers (#27) ([e22237a](https://github.com/beelinewf/wf-engine/commit/e22237afd7b628d53d2724824109fbb86573b2ff))
- Supervise broker inputs with retry (#28) ([2951407](https://github.com/beelinewf/wf-engine/commit/29514077fcf07d9e86680ef6a4c0fd514ca2533d))
- Add postgres pool limits (#29) ([fe8e729](https://github.com/beelinewf/wf-engine/commit/fe8e72910065658f8b3b1cb5272182834d62d916))
- Bound outbound HTTP concurrency (#33) ([54b4cd2](https://github.com/beelinewf/wf-engine/commit/54b4cd245ea6ac536796d4e756b4de8ca08a92e5))
- Scope statistics to owner (#35) ([caf0577](https://github.com/beelinewf/wf-engine/commit/caf0577a36f8d332a7a2839350a36468c03defd9))
- Add UI and proxy to stack (#38) ([838abb0](https://github.com/beelinewf/wf-engine/commit/838abb00ccab09e25f7645487c38edfef2444cad))
- Add mock provider container (#39) ([4753be7](https://github.com/beelinewf/wf-engine/commit/4753be7ea1770b1f5d5bbe67d89b611b5bd66443))
- Add definition migration endpoint (#41) ([929632f](https://github.com/beelinewf/wf-engine/commit/929632ffd30e155a2c51de2ebf494764283be42a))
- Add debug filter to list (#42) ([95dba71](https://github.com/beelinewf/wf-engine/commit/95dba71e54a9f7c356858503d2eb87fe2a819f73))
- Feat/delay (#43) ([e90b306](https://github.com/beelinewf/wf-engine/commit/e90b30669ae9fee609d53ceb2d44b60806d80a8b))
- Add delay node type (#44) ([20b0587](https://github.com/beelinewf/wf-engine/commit/20b0587ba57ba02d872ee0bc57e06629d6591232))
- Add status SSE stream (#48) ([96c8c10](https://github.com/beelinewf/wf-engine/commit/96c8c1039d02fee5aff4b59603382e7029d1a17f))
- Add forms library CRUD API (#49) ([6694395](https://github.com/beelinewf/wf-engine/commit/6694395579367b7ef4a79ff15eb6ca0a1e33cee6))
- Add node type catalog endpoint (#45) (#47) ([13d374b](https://github.com/beelinewf/wf-engine/commit/13d374b47da98581def2ae83c5f3810ca36c3137))
- Add users list and get endpoints (#54) ([0359297](https://github.com/beelinewf/wf-engine/commit/03592974216fc2c23a21856b45868f5594db387c))
- Add configurable CORS middleware (#55) ([62ae305](https://github.com/beelinewf/wf-engine/commit/62ae305ea075d66d9ed13043c9e3352d4ecd4f9b))
- Add multipart upload support (#56) ([d0b9077](https://github.com/beelinewf/wf-engine/commit/d0b9077ef506c10f822b7a0e8d01684e7d0b3322))
- Add Prometheus metrics (#59) ([e8dfb62](https://github.com/beelinewf/wf-engine/commit/e8dfb628cc5ded5c78748a25885d508c0c617d28))
- Add branch identity context (#58) ([7cdb999](https://github.com/beelinewf/wf-engine/commit/7cdb999ffc3db978c6b9f239c0ff7517413418b8))
- Add allowed_users gate (#57) ([c70c357](https://github.com/beelinewf/wf-engine/commit/c70c357c639e615f4fce52c1c1984dc9b895c25a))
- Add body cap and timeouts (#61) ([175fd0f](https://github.com/beelinewf/wf-engine/commit/175fd0f73e4dafd3dffca6004f432980ab5985cd))

### Other

- Remove phantom materialize endpoint ([2d35558](https://github.com/beelinewf/wf-engine/commit/2d355581333ec8917c6fffd65c765c9838058af9))
- Use setup-atlas action for migrations ([20dfb42](https://github.com/beelinewf/wf-engine/commit/20dfb426b686b9133afbeae1b3baa2846c72bb91))
- Fix flaky fanout order ([86e6538](https://github.com/beelinewf/wf-engine/commit/86e6538411937f74694a33c34dd5c927b43ee70b))
- Bump golangci-lint-action to v7 ([c0f769a](https://github.com/beelinewf/wf-engine/commit/c0f769a3c06bebcd1b011e6e758d4bdf0bfe8590))
- Add badge workflow and sync docs ([c6c5586](https://github.com/beelinewf/wf-engine/commit/c6c5586f96985bd07e3a4134921d6e6e748bbce7))
- Update coverage badge ([8fa4955](https://github.com/beelinewf/wf-engine/commit/8fa4955c4e10b75858b553b6b0654791ed963559))
- Add coverage guard workflow ([7f45204](https://github.com/beelinewf/wf-engine/commit/7f452049fe3bc5705fd79cdce177187e6a308a67))
- Rewrite structure and content (#3) ([218436a](https://github.com/beelinewf/wf-engine/commit/218436acd4dbb42e522a87a06552a101898bbc71))
- Use PAT for badge push (#6) ([49305a3](https://github.com/beelinewf/wf-engine/commit/49305a32300ea7bb6b4c76e4e9c8bd93c466cd17))
- Use PAT for checkout step (#7) ([e51224f](https://github.com/beelinewf/wf-engine/commit/e51224f12bf7fdefdf1de0ab5014ffa44df26aef))
- Update coverage badge ([47f9050](https://github.com/beelinewf/wf-engine/commit/47f9050373b4defca3af401e046cf549b8d26677))
- Update coverage badge ([bce1c83](https://github.com/beelinewf/wf-engine/commit/bce1c83c7a47b316717b6bdf29a502405e432db7))
- Update coverage badge ([2147c4e](https://github.com/beelinewf/wf-engine/commit/2147c4e19dbb410e85a0fd4e0646e3d0e554f3e0))
- Update coverage badge ([7b06d82](https://github.com/beelinewf/wf-engine/commit/7b06d820236e76fba500086111d8daed56a6d279))
- Update coverage badge ([a42e927](https://github.com/beelinewf/wf-engine/commit/a42e927a1fe7f5ba032c2b8fddc6ba6d65fe1989))
- Update coverage badge ([4672f41](https://github.com/beelinewf/wf-engine/commit/4672f4181a9e24a2136dea116e8b9606dc8a2d63))
- Update coverage badge ([f51249a](https://github.com/beelinewf/wf-engine/commit/f51249a0deece90bd3bc01e3e5a2a8991396391c))
- Update coverage badge ([6ed110e](https://github.com/beelinewf/wf-engine/commit/6ed110e6d710d2623d43dca17e91639991cdc8e6))
- Update coverage badge ([9d1899d](https://github.com/beelinewf/wf-engine/commit/9d1899dffb1b8f3001843c24d7c785294b3d6e9f))
- Update coverage badge ([7af3250](https://github.com/beelinewf/wf-engine/commit/7af3250c8080727d6433bd84404e95c6093c810b))
- Update coverage badge ([96fdd1b](https://github.com/beelinewf/wf-engine/commit/96fdd1bf903398ca9baec0d987f36f7808085a1f))
- Update coverage badge ([df5b42a](https://github.com/beelinewf/wf-engine/commit/df5b42a4b75b472041646e4e9166ba788931153f))
- Update coverage badge ([e3bb4c8](https://github.com/beelinewf/wf-engine/commit/e3bb4c83e0610402d2acf1a56d0ca63fb87dd00a))
- Update coverage badge ([93ca085](https://github.com/beelinewf/wf-engine/commit/93ca0852725c018f1de934afd906e6abffcbaace))
- Update coverage badge ([e64803e](https://github.com/beelinewf/wf-engine/commit/e64803e425d1e68bd4a9ebe0368d181266d20453))
- Update coverage badge ([d77649b](https://github.com/beelinewf/wf-engine/commit/d77649b6d75d2990b86a0d86e9aea2415cddc454))
- Update coverage badge ([47c7d5c](https://github.com/beelinewf/wf-engine/commit/47c7d5c5a9c19428992cdc5b8b38ade73479ec0d))
- Update coverage badge ([3ee404c](https://github.com/beelinewf/wf-engine/commit/3ee404c5491336921e72cd5e6895eca20c70324b))
- Update coverage badge ([3f386ba](https://github.com/beelinewf/wf-engine/commit/3f386ba98f9d5377c59ba2ba97e990c09a4cd0c8))
- Update coverage badge ([624d8ed](https://github.com/beelinewf/wf-engine/commit/624d8ed0fd1cc60e66d2a631e8f6e4def11c038d))
- Update coverage badge ([688d3ce](https://github.com/beelinewf/wf-engine/commit/688d3ce37caa84c317062e2a126a06dbf449245f))
- Update coverage badge ([c837d3f](https://github.com/beelinewf/wf-engine/commit/c837d3f33073683dd4ae064a8f628f50d405f13a))
- Update coverage badge ([eab319b](https://github.com/beelinewf/wf-engine/commit/eab319b8c125d954504020dab92a47962b811952))
- Update coverage badge ([6c80a78](https://github.com/beelinewf/wf-engine/commit/6c80a78f6e23be5e6ee06dc3f16c4ba52735ab26))
- Update coverage badge ([8b9dc7c](https://github.com/beelinewf/wf-engine/commit/8b9dc7c210b4e505d1ae50078f21238effb868c5))
- Update coverage badge ([e29980f](https://github.com/beelinewf/wf-engine/commit/e29980faaa0b136a1d4c2b02a7fb90305214bb6a))
- Update coverage badge ([f6b0982](https://github.com/beelinewf/wf-engine/commit/f6b0982057b4367e46bb23d40103be79f6f80ac7))
- Update coverage badge ([f4adce6](https://github.com/beelinewf/wf-engine/commit/f4adce644c3099cc7ff27cf35f0423e4beb64be5))
- Update coverage badge ([8da9435](https://github.com/beelinewf/wf-engine/commit/8da94353ce4d5da20fc9eb6670ea6db0a7b72333))
- Centralize test DB setup (#34) ([70546dc](https://github.com/beelinewf/wf-engine/commit/70546dcfc4abcbce5e6c96cc82e802e9f92b69f9))
- Update coverage badge ([0e5ca91](https://github.com/beelinewf/wf-engine/commit/0e5ca91d3f1cca444e777ba9c703de8cc68466bd))
- Pin deps to digests (#36) ([70d6f16](https://github.com/beelinewf/wf-engine/commit/70d6f16399feed77727487693af91c0e6a77ef9e))
- Update coverage badge ([545de15](https://github.com/beelinewf/wf-engine/commit/545de1585a75363f11cad18303efe5ad3ddb7e19))
- Update coverage badge ([1fc75e5](https://github.com/beelinewf/wf-engine/commit/1fc75e5af2db6817ecbc4864a290842b4b0c526b))
- Update coverage badge ([1418935](https://github.com/beelinewf/wf-engine/commit/14189352d7ae55ac3cccf47a0a98ca34a6c5bfa1))
- Update coverage badge ([3316c26](https://github.com/beelinewf/wf-engine/commit/3316c261ebeeb32ba48b0c42c7da275b50d8178c))
- Update coverage badge ([67af2a4](https://github.com/beelinewf/wf-engine/commit/67af2a4546a1aa39b7156600684e568d1a788b58))
- Update coverage badge ([34b2036](https://github.com/beelinewf/wf-engine/commit/34b203600aa24ee1abb9283c2ce79fd07ed68a8b))
- Update coverage badge ([1ca93b5](https://github.com/beelinewf/wf-engine/commit/1ca93b5559136f51186829dfc0528538c678ec6c))
- Update coverage badge ([5667472](https://github.com/beelinewf/wf-engine/commit/5667472787e81b85ed1288332cf907912ea62561))
- Update coverage badge ([25c92a0](https://github.com/beelinewf/wf-engine/commit/25c92a08327a096aa43308611cb81a1feb466bd3))
- Update coverage badge ([381ace2](https://github.com/beelinewf/wf-engine/commit/381ace2fd344402aa7868b7b840e8e326d3a2519))
- Update coverage badge ([3ad1e90](https://github.com/beelinewf/wf-engine/commit/3ad1e90d338dbd5910d6f90f2cbee3eab123a3ab))
- Update coverage badge ([a4a354c](https://github.com/beelinewf/wf-engine/commit/a4a354c27c81527e8f330bf4d628c94a0d22b4bf))
- Update coverage badge ([670ec80](https://github.com/beelinewf/wf-engine/commit/670ec8031c8f50e0449b44a7ec9c271cb4d91a83))
- Update coverage badge ([7f5c165](https://github.com/beelinewf/wf-engine/commit/7f5c165ee80be15335409fe837f7d3c0fcc0bf54))
- Update coverage badge ([82a3a05](https://github.com/beelinewf/wf-engine/commit/82a3a05c8091a67ed7d4c176c7649da7de3ffc3a))
- Update coverage badge ([b72007c](https://github.com/beelinewf/wf-engine/commit/b72007caddb6eeb8838549495dc2bb4879dedde5))
- Update coverage badge ([5d8ea67](https://github.com/beelinewf/wf-engine/commit/5d8ea6762e1c33d3fc5dca2425babb2fb7fe1d43))
- Rebrand simpwf to beelinewf (#50) ([a7de926](https://github.com/beelinewf/wf-engine/commit/a7de92675053e85b5c907201979f87c67bf662fc))
- Add CLA workflow and docs (#51) ([5664e7e](https://github.com/beelinewf/wf-engine/commit/5664e7e558231a5ea752e08e1dc3345479a75213))
- Add tag-triggered release workflow (#52) ([0c61a7c](https://github.com/beelinewf/wf-engine/commit/0c61a7ce2b85be2d2802c5447856e09219634224))

### Performance

- Cache immutable definitions (#30) ([d8f5f94](https://github.com/beelinewf/wf-engine/commit/d8f5f94962a45e46f5af80660c821bf0b29d7874))
- Add claim poll list indexes (#31) ([1c152a9](https://github.com/beelinewf/wf-engine/commit/1c152a91b8684d3f98de9edaf936367c29c8298b))
- Batch status detail queries (#32) ([cf373d7](https://github.com/beelinewf/wf-engine/commit/cf373d7e0e63dd89b7f2d52e2b74d3c5a09a41ef))

### Refactors

- Extract shared mechanics (#26) ([79cd975](https://github.com/beelinewf/wf-engine/commit/79cd975c2845d969716d50119f65d50306e7634a))
- Extract pump and helpers (#62) ([22e9ff7](https://github.com/beelinewf/wf-engine/commit/22e9ff7653830b35dbd2880b205412396ebae98c))

