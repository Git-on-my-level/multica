# macOS Desktop release (fork policy)

Public Desktop releases for `Git-on-my-level/multica` are made manually on
David's arm64 Mac. The public deliverable is **macOS arm64 DMG only**. Mobile
may be considered later, but it is out of scope. Do not publish or wait for a
ZIP, updater metadata, CLI, containers, Windows, Linux, or x64 artifacts unless
explicitly requested.

The DMG must be signed with the locally discovered `Developer ID Application:
DAZHENG ZHANG (JVMXE5G542)` identity, notarized, and stapled before publication.
Never use an unsigned/ad-hoc build as a public-release substitute.

## Credentials and signing prerequisites

On the signing Mac, keep the notarization credential in the macOS login
Keychain as the `multica-notary` profile. Confirm the profile can authenticate
without exposing a secret:

```bash
xcrun notarytool history --keychain-profile multica-notary --output-format json > /dev/null
```

If the profile must be renewed, run `xcrun notarytool store-credentials`
interactively. Never put an app-specific password in documentation, shell
history, Git, chat, or logs. The package script recognizes this profile with:

```bash
export APPLE_KEYCHAIN_PROFILE=multica-notary
```

The legacy Apple-ID environment path remains supported for exceptional cases,
but it is not the default. Do not put its values in release instructions or
logs.

Verify that the signing identity is available, then let electron-builder find
it automatically:

```bash
security find-identity -v -p codesigning | rg 'Developer ID Application: DAZHENG ZHANG \(JVMXE5G542\)'
```

Leave `CSC_NAME` unset. In electron-builder 26.8.1, setting `CSC_NAME` to a
value prefixed with `Developer ID Application:` was rejected during v0.4.11.
Do not set `CSC_IDENTITY_AUTO_DISCOVERY=false` for a distributable build.

## Build and local verification

1. Start from the exact reviewed commit on `main`; record it before building:

   ```bash
   RELEASE_SHA="$(git rev-parse HEAD)"
   git status --short
   ```

   Stop if the worktree is dirty or `RELEASE_SHA` is not the reviewed commit.

2. Build locally, never publish from the package command:

   ```bash
   cd apps/desktop
   APPLE_KEYCHAIN_PROFILE=multica-notary \
     node scripts/package.mjs --mac --arm64 --publish never
   ```

   `electron-builder` can transiently emit a ZIP because of its configured
   macOS targets. That does not expand this release: DMG-only scope means do
   not publish or wait for the ZIP or any updater metadata.

3. Verify the fresh DMG, then mount it. Replace `<version>` with the built
   version and keep the attach output so it can be detached reliably:

   ```bash
   DMG="dist/multica-desktop-<version>-mac-arm64.dmg"
   test -f "$DMG"
   xattr -w com.apple.quarantine '0081;00000000;MulticaRelease;' "$DMG"
   MOUNT_POINT="$(hdiutil attach -nobrowse -readonly "$DMG" | awk '/\/Volumes\// {print $3; exit}')"
   APP="$MOUNT_POINT/Multica.app"
   test -d "$APP"
   codesign --verify --deep --strict --verbose=4 "$APP"
   codesign -dv --verbose=4 "$APP"
   spctl -a -vv -t install "$APP"
   xcrun stapler validate "$APP"
   test "$(defaults read "$APP/Contents/Info" CFBundleShortVersionString)" = "<version>"
   file "$APP/Contents/MacOS/Multica" | rg 'arm64'
   file "$APP/Contents/Resources/bin/multica" | rg 'arm64'
   RELEASE_SHORT_SHA="$(git rev-parse --short "$RELEASE_SHA")"
   CLI_INFO="$("$APP/Contents/Resources/bin/multica" version --output json)"
   printf '%s\n' "$CLI_INFO" | rg '"commit": "'"$RELEASE_SHORT_SHA"'"'
   printf '%s\n' "$CLI_INFO" | rg '"arch": "arm64"'
   ```

   Stop unless Gatekeeper reports `accepted` with a Notarized Developer ID
   source, stapler succeeds, the bundle version is the intended release, and
   both app and bundled CLI are arm64. Compare the bundled CLI version and
   provenance to `RELEASE_SHA`; stop on any mismatch.

4. Detach the image even after a failed check, then remove only the temporary
   mounted image state:

   ```bash
   hdiutil detach "$MOUNT_POINT" || hdiutil detach -force "$MOUNT_POINT"
   unset MOUNT_POINT APP
   ```

   Do not upload, tag, or release after a signing, notarization, staple,
   Gatekeeper, version, architecture, or bundled-CLI failure.

## Publish the verified DMG

The repository's `.github/workflows/release.yml` triggers its generic Release
workflow whenever a `v*.*.*` tag is pushed. On this fork it can start unrelated
CLI and container work; it is not a DMG release gate. Do not wait for those jobs
before creating the manually verified DMG release.

After all local checks pass, create an annotated tag at the exact recorded
commit, push only that tag, and prove the remote annotated tag resolves to the
same commit:

```bash
git tag -a "v<version>" "$RELEASE_SHA" -m "Multica Desktop v<version>"
git push origin "v<version>"
test "$(git ls-remote origin "refs/tags/v<version>^{}" | awk '{print $1}')" = "$RELEASE_SHA"
```

Create the GitHub Release with the pre-existing tag and upload only the DMG:

```bash
gh release create "v<version>" "$DMG" \
  --repo Git-on-my-level/multica \
  --verify-tag \
  --title "Multica Desktop v<version>" \
  --notes-from-tag
```

`--verify-tag` prevents `gh release create` from creating a tag at the default
branch by implication. Stop if the remote tag does not resolve to
`RELEASE_SHA`, the release command tries to include another asset, or the
release would be built from a different commit.

## Verify the public artifact

Download the public DMG afresh from the GitHub Release, apply quarantine, and
repeat the mount and checks above (`codesign`, `spctl`, `stapler`, bundle
version, arm64 architecture, and bundled-CLI provenance):

```bash
RELEASE_DOWNLOAD_DIR="$(mktemp -d)"
gh release download "v<version>" \
  --repo Git-on-my-level/multica \
  --pattern "multica-desktop-<version>-mac-arm64.dmg" \
  --dir "$RELEASE_DOWNLOAD_DIR"
DMG="$RELEASE_DOWNLOAD_DIR/multica-desktop-<version>-mac-arm64.dmg"
# Apply quarantine, mount, verify, and detach as in the local verification step.
```

After detaching, remove the temporary download directory with
`rm -rf "$RELEASE_DOWNLOAD_DIR"`. Stop and investigate if the downloaded
artifact differs from the locally verified DMG; do not replace it with a ZIP,
bypass Gatekeeper, or declare unrelated workflow completion as release
verification.
