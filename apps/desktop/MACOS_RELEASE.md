# macOS desktop release signing and notarization

Gatekeeper blocks apps that are unsigned, ad-hoc signed, or signed with a Developer ID
certificate that has not been notarized and stapled. Multica's `electron-builder.yml`
enables `mac.notarize: true`, but notarization only runs when these env vars are set at
package time:

| Variable | Purpose |
|---|---|
| `CSC_LINK` | Base64 `.p12` export of **Developer ID Application** cert (or use a macOS keychain cert via `CSC_NAME`) |
| `CSC_KEY_PASSWORD` | Password for the `.p12` |
| `APPLE_ID` | Apple ID email for the developer team |
| `APPLE_APP_SPECIFIC_PASSWORD` | App-specific password from [appleid.apple.com](https://appleid.apple.com) |
| `APPLE_TEAM_ID` | Team ID (e.g. `JVMXE5G542`) |

`scripts/package.mjs` skips notarization when `APPLE_TEAM_ID` is unset and logs a warning.

## Fork CI (`release.yml` → `desktop-mac`)

Wire the variables above as GitHub Actions **repository secrets** on the fork
(`Git-on-my-level/multica`). The fork-only `desktop-mac` job in
[`.github/workflows/release.yml`](../../.github/workflows/release.yml) already
forwards them to `package.mjs`. Without all five, CI produces a Developer
ID-signed (or ad-hoc) build that Gatekeeper still rejects with **"Apple could
not verify Multica"**.

CI publishes **arm64 only** so a single `latest-mac.yml` is written per tag
(publishing x64 + arm64 with `--publish always` overwrites that feed and breaks
auto-update). After secrets are configured, tag a new release (e.g. `v0.3.46`)
and confirm the mac job uploads `multica-desktop-*-mac-arm64.dmg`, `.zip`, and
`latest-mac.yml`.

## Verify a release artifact

```bash
# Mount DMG or point APP at an installed copy
APP="/Volumes/Multica 0.3.45-arm64/Multica.app"

codesign -dv --verbose=4 "$APP"
spctl -a -vv -t install "$APP"
stapler validate "$APP"
xattr -l multica-desktop-0.3.45-mac-arm64.dmg   # browser downloads add com.apple.quarantine
```

| `spctl` result | Meaning |
|---|---|
| `accepted` + `source=Notarized Developer ID` | Gatekeeper-clean |
| `rejected` + `source=Unnotarized Developer ID` | Signed but not notarized — user must Right-click → Open once, or staple after notarization |
| `rejected` + no Developer ID in origin | Ad-hoc or unsigned — replace with a Developer ID build |

## Local notarized release (maintainers)

On a Mac with the Developer ID cert in Keychain:

```bash
export APPLE_ID="you@example.com"
export APPLE_APP_SPECIFIC_PASSWORD="xxxx-xxxx-xxxx-xxxx"
export APPLE_TEAM_ID="JVMXE5G542"
# Optional if cert is in keychain instead of CSC_LINK:
# export CSC_NAME="Developer ID Application: YOUR NAME (JVMXE5G542)"

cd apps/desktop
rm -rf dist out
node scripts/package.mjs --mac --arm64 --publish always \
  --config.publish.owner=Git-on-my-level \
  --config.publish.repo=multica
```

`electron-builder` submits to Apple's notary service and staples the ticket into the
DMG/ZIP before upload.

## User workaround (unnotarized but Developer ID-signed)

Valid for [v0.3.45](https://github.com/Git-on-my-level/multica/releases/tag/v0.3.45) until a
notarized build is published:

1. **Quit Multica** if it is running.
2. Remove stale updater copies (optional but recommended):
   `bash scripts/cleanup-macos-desktop-updater.sh`
3. Download `multica-desktop-*-mac-arm64.dmg` from GitHub Releases.
4. Open the DMG, drag **Multica** to **Applications**.
5. **First launch only:** in Finder, Right-click **Multica** → **Open** → confirm **Open**
   in the dialog. Double-click works on subsequent launches.
6. If Gatekeeper still blocks after a browser download, clear quarantine on the installed app:
   `xattr -cr /Applications/Multica.app`

Replacing an older **ad-hoc** install: delete `/Applications/Multica.app` first, then install
from the current DMG. Run `bash scripts/cleanup-macos-desktop-updater.sh --inspect` to see
whether the installed copy is ad-hoc, unnotarized Developer ID, or notarized.
