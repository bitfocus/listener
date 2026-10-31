# Changelog

## [1.2.0](https://github.com/bitfocus/listener/compare/v1.1.0...v1.2.0) (2026-10-31)


### Features

* add windows arm64 build ([#25](https://github.com/bitfocus/listener/issues/25)) ([9638fdb](https://github.com/bitfocus/listener/commit/9638fdbf169c45b9a520cd27126b7828c4010e70))

## [1.1.0](https://github.com/bitfocus/listener/compare/v1.0.13...v1.1.0) (2026-10-27)


### Features

* add fileWrite command for creating and appending to files ([#21](https://github.com/bitfocus/listener/issues/21)) ([636c0ea](https://github.com/bitfocus/listener/commit/636c0eadef286b6fc42a5e8d6c643c7db05ec6b7))
* group tray menu items and show the app name ([#23](https://github.com/bitfocus/listener/issues/23)) ([f9f94cc](https://github.com/bitfocus/listener/commit/f9f94cce84f7029f0cb7e8d78318de985826dbe6))
* modernize settings UI ([#19](https://github.com/bitfocus/listener/issues/19)) ([6edd83b](https://github.com/bitfocus/listener/commit/6edd83bbb8d047559ebec99694b7eeee98e70866))


### Bug Fixes

* add F13–F24, Caps Lock, Menu and Numpad keys to key access control ([#18](https://github.com/bitfocus/listener/issues/18)) ([5a77dc7](https://github.com/bitfocus/listener/commit/5a77dc710a2ea6428cbf8858d7359764f4b4a402))
* harden auth ordering, rebinds, and GUI races ([bff705a](https://github.com/bitfocus/listener/commit/bff705a21382e290f8c83d879143482780cdd343))
* off-by-default exec commands and fileWrite folder checks ([#22](https://github.com/bitfocus/listener/issues/22)) ([5998f4b](https://github.com/bitfocus/listener/commit/5998f4bc7598ef04299e5c8b2aaca9d3b8ca72ab))
* polish error messages, hardened activity log, shared sysInfo sampling ([#20](https://github.com/bitfocus/listener/issues/20)) ([22a4947](https://github.com/bitfocus/listener/commit/22a4947b632df93593993dd24c3eeba56817eace))
* reject cross-origin websockets and cap message size ([bff705a](https://github.com/bitfocus/listener/commit/bff705a21382e290f8c83d879143482780cdd343))
* report unsupported keys and add Pause/Scroll Lock support ([bff705a](https://github.com/bitfocus/listener/commit/bff705a21382e290f8c83d879143482780cdd343))
* stop shared-IP lockout, DNS rebinding, and stale auth state ([bff705a](https://github.com/bitfocus/listener/commit/bff705a21382e290f8c83d879143482780cdd343))
* use an SVG tray glyph and require macOS 14 ([bff705a](https://github.com/bitfocus/listener/commit/bff705a21382e290f8c83d879143482780cdd343))

## [1.0.13](https://github.com/bitfocus/listener/compare/v1.0.12...v1.0.13) (2026-09-30)


### Bug Fixes

* windows release builds ([#13](https://github.com/bitfocus/listener/issues/13)) ([fc7b307](https://github.com/bitfocus/listener/commit/fc7b3075e004b0ba84bc9dbc6345d0765dd5f771))

## [1.0.12](https://github.com/bitfocus/listener/compare/v1.0.11...v1.0.12) (2026-09-03)


### Bug Fixes

* macOS tray icon handling, move logs to proper location ([#1](https://github.com/bitfocus/listener/issues/1)) ([0a5b287](https://github.com/bitfocus/listener/commit/0a5b287a671705db1afe88e3a1b5981d3bdef168))
* normalize modifier aliases ([#9](https://github.com/bitfocus/listener/issues/9)) ([e39a6e5](https://github.com/bitfocus/listener/commit/e39a6e5a0f01b361db33672a6cdb8ad809b9b5e4))
