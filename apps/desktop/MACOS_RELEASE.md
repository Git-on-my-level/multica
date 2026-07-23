# macOS Desktop release (fork policy)

Public Desktop releases for `Git-on-my-level/multica` are made manually on
David's arm64 Mac. They are always signed with the local Keychain identity
`Developer ID Application: DAZHENG ZHANG (JVMXE5G542)`, notarized by Apple,
and stapled before publication. Do not publish ad-hoc, unsigned, x64, Linux,
or Windows Desktop artifacts unless David explicitly asks.

Fork tag automation publishes CLI archives and GHCR images only. It never
publishes Desktop artifacts. `CSC_IDENTITY_AUTO_DISCOVERY=false` is permitted
only for clearly non-distributed smoke builds with `--publish never`.

## Prerequisites

On David's signing Mac, confirm the Keychain identity and export only the
notarization credentials required by electron-builder:

```bash
security find-identity -v -p codesigning | rg 'Developer ID Application: DAZHENG ZHANG \(JVMXE5G542\)'
export CSC_NAME='Developer ID Application: DAZHENG ZHANG (JVMXE5G542)'
export APPLE_ID='your-apple-id@example.com'
export APPLE_APP_SPECIFIC_PASSWORD='app-specific-password'
export APPLE_TEAM_ID='JVMXE5G542'
```

Use an app-specific password, never a primary Apple Account password. Do not
set `CSC_IDENTITY_AUTO_DISCOVERY=false` for this release path.

## Manual arm64 release checklist

1. Start from the intended, already-pushed fork tag. Never rewrite a failed
   tag or release; create a corrective version instead.
2. Build locally without publication. This is the verification candidate:

   ```bash
   cd apps/desktop
   node scripts/package.mjs --mac --arm64 --publish never \
     --config.publish.owner=Git-on-my-level \
     --config.publish.repo=multica
   ```

3. Verify the locally produced DMG and ZIP. Expected assets are
   `multica-desktop-<version>-mac-arm64.dmg`,
   `multica-desktop-<version>-mac-arm64.zip`, and `latest-mac.yml`.
   There must be no x64 Desktop asset.
4. Mount the DMG and validate its application bundle:

   ```bash
   hdiutil attach "dist/multica-desktop-<version>-mac-arm64.dmg"
   APP='/Volumes/Multica <version>/Multica.app'
   codesign --verify --deep --strict --verbose=4 "$APP"
   codesign -dv --verbose=4 "$APP"
   spctl -a -vv -t install "$APP"
   xcrun stapler validate "$APP"
   hdiutil detach "$(dirname "$APP")"
   ```

   `spctl` must report `accepted` and `source=Notarized Developer ID`.
5. Download the candidate ZIP/DMG through the same release path users will
   use, then repeat the mounted-DMG and installed-app checks. This catches a
   bad upload, missing staple, and browser-download differences. Quarantine is
   diagnostic evidence only; do not make `xattr -cr` or Gatekeeper bypasses a
   release solution.
6. Only after these checks pass, publish the same arm64 build using the
   approved manual release process. Confirm the release contains exactly the
   DMG, ZIP, and `latest-mac.yml` metadata expected by electron-updater.

If signing or notarization is unavailable, stop before upload. A public
Desktop release must be postponed rather than replaced with an ad-hoc build.
