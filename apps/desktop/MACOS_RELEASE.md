# macOS arm64 Desktop release (fork)

Desktop signing is human-gated on the designated arm64 Mac. It never runs from
GitHub Actions, never creates a release tag, and uses the existing local
Keychain profile; no Apple credential belongs in CI, Git, shell history, or
logs.

After an approved stable `vX.Y.Z` tag is already at the freshly fetched
`origin/main`, invoke this single command from a clean checkout:

```bash
env -u GOROOT -u GOPATH -u GOENV_ROOT -u GOENV_SHELL -u GOENV_PATH_ORDER \
  PATH="/opt/homebrew/bin:/usr/bin:/bin" \
  APPLE_KEYCHAIN_PROFILE="multica-notary" \
  APPLE_TEAM_ID="JVMXE5G542" \
  pnpm --dir apps/desktop release:macos-arm64
```

The command fails closed before building unless it finds an arm64 Mac, Go 1.26+,
the Developer ID identity, and a directly usable `multica-notary` Keychain
profile. It then builds unpublished first and requires a signed, notarized,
stapled, Gatekeeper-accepted arm64 app/DMG plus an internally consistent
`latest-mac.yml`, referenced ZIP, and ZIP blockmap. The single build is
fork-configured with `--publish never`, embedding the fork's updater feed before
validation. It then uses the existing GitHub Release as an
idempotent publisher for those exact verified DMG, manifest, ZIP, and blockmap
files. It never asks electron-builder to rebuild with `--publish always`, so a
second, unvalidated artifact set cannot become public. Existing Desktop assets
are never replaced: a retry accepts one only when its downloaded bytes match
the already verified local file, otherwise it fails.

Finally it downloads the public DMG, manifest, ZIP, and blockmap fresh from the
existing GitHub Release, verifies the manifest path/version/SHA-512 and ZIP,
and repeats the DMG signature, notarization, Gatekeeper, version, and arm64
checks. A DMG-only result is a terminal failure.

Do not work around a missing profile with Apple-ID environment credentials. An
operator must repair the existing Keychain profile interactively before retrying.
