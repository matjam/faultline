# Changelog

## [1.5.0](https://github.com/matjam/faultline/compare/v1.4.0...v1.5.0) (2026-05-01)


### Features

* Agent Skills support ([#24](https://github.com/matjam/faultline/issues/24)) ([eaad5a0](https://github.com/matjam/faultline/commit/eaad5a0eb8d43f80c709b97e474f10fe3def3fad))
* **skills:** security audit subagent + sandbox hardening ([#27](https://github.com/matjam/faultline/issues/27)) ([93515d5](https://github.com/matjam/faultline/commit/93515d58b3dc86f5666f8c9bea6033956704fd7a))
* subagent delegation ([#26](https://github.com/matjam/faultline/issues/26)) ([c42b698](https://github.com/matjam/faultline/commit/c42b6988b0e537d6e647467acdefef81d9e21b53))

## [1.4.0](https://github.com/matjam/faultline/compare/v1.3.0...v1.4.0) (2026-05-01)


### Features

* custom Arch-based multi-runtime sandbox image ([#23](https://github.com/matjam/faultline/issues/23)) ([41bee48](https://github.com/matjam/faultline/commit/41bee48f1bd97c14e76ea5acdc3a97bc3186667d))


### Bug Fixes

* shutdown handling and reconcile observability ([#21](https://github.com/matjam/faultline/issues/21)) ([81cc3b6](https://github.com/matjam/faultline/commit/81cc3b6e3d147c122cd06f5072e310a561406ea7))

## [1.3.0](https://github.com/matjam/faultline/compare/v1.2.0...v1.3.0) (2026-05-01)


### Features

* paragraph-aligned semantic indexing with adaptive batching ([#19](https://github.com/matjam/faultline/issues/19)) ([e5d7918](https://github.com/matjam/faultline/commit/e5d7918539d43b80f499101538c171c237e1bcc5))

## [1.2.0](https://github.com/matjam/faultline/compare/v1.1.0...v1.2.0) (2026-05-01)


### Features

* add rebuild_indexes tool and refactor reconcile/rebuild helpers ([27aea57](https://github.com/matjam/faultline/commit/27aea575d51f7509af3697e878a81b75f40f03e9))
* add semantic memory search with persisted vector index ([ff012dc](https://github.com/matjam/faultline/commit/ff012dc0f08ffe7eb62afbf4b2e6bec5d1898fba))
* add semantic memory search with persisted vector index ([6265089](https://github.com/matjam/faultline/commit/62650891ae24f9c555af3a3f129ea94bdbb21225))

## [1.1.0](https://github.com/matjam/faultline/compare/v1.0.0...v1.1.0) (2026-05-01)


### Features

* add self-update support ([cc7090e](https://github.com/matjam/faultline/commit/cc7090eb6c9b6da4601bb3a5c5be132519fcc5d6))
* add self-update support ([1739d61](https://github.com/matjam/faultline/commit/1739d61bab2a551858a099442343315c04e4437f))

## 1.0.0 (2026-05-01)


### ⚠ BREAKING CHANGES

* footer, which is too sharp a knife for a project at this stage. Add 'Ask the maintainer first.' to the table row and a paragraph spelling out the policy: contributors don't unilaterally mark breaking changes; the maintainer accumulates them and decides when to cut a major. Also documents the Release-As: footer for forcing a specific version when needed.

### Features

* add email_fetch tool ([f1b4ab6](https://github.com/matjam/faultline/commit/f1b4ab6f6cde2349247841b29f6845cf33767b30))
* add release pipeline (release-please + goreleaser) ([3075763](https://github.com/matjam/faultline/commit/3075763c74284f2fe9f2447a2f2b5724003e8c89))
* add release pipeline (release-please + goreleaser) ([0a1b86d](https://github.com/matjam/faultline/commit/0a1b86d82e82af0b822eadbb971cddd383ae3217))
* add sandbox_shell tool for arbitrary shell command execution ([40c0922](https://github.com/matjam/faultline/commit/40c0922d017847e2fb8d11a23754c17d84430b15))
* add sandbox_shell tool for arbitrary shell command execution ([efe41cb](https://github.com/matjam/faultline/commit/efe41cb2d7d4f58c13f762437e8b83df9943f964))


### Miscellaneous Chores

* tidy release-please config and document major-bump policy ([793f6ee](https://github.com/matjam/faultline/commit/793f6ee50a7e21a78fe7c32db6537b82ebc5c276))
