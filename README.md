# liftbay-runner

[github.com/liftbay/runner](https://github.com/liftbay/runner) · Apache-2.0

Runs one Liftbay build on the machine it's started on. On the GitHub Actions engine that is your
own GitHub runner: Linux for Android, macOS for iOS. Apache-2.0; it's the code that sees your
source and secrets, so it is open.

## What it does

1. Gets a one-time job token: exchanges the run's GitHub OIDC token (audience `liftbay`) at
   `POST /v1/runner/github/exchange`, or uses `LIFTBAY_JOB_TOKEN` when run by hand.
2. Fetches the job: build settings, the environment's variables and signing material.
3. Runs the recipe, streaming phases and logs:

| | Android | iOS |
|---|---|---|
| Expo | install → `expo prebuild` → Gradle `bundleRelease` / `assembleRelease` / `assembleDebug` | install → `expo prebuild` → `pod install` → `xcodebuild archive` → export `.ipa` |
| React Native | install → Gradle | install → `pod install` → xcodebuild |

   Without iOS signing credentials it builds for the iOS simulator (a zipped `.app`). Without an
   Android upload key it generates one with `keytool` and stores it, encrypted, with the project.
4. Uploads artifacts (`.aab`, `.apk`, `.ipa`, dSYMs, mapping) and finishes the build.

Secret values are replaced by `***` in every log line (and masked in the GitHub log). Android
signing goes through Gradle's injected signing properties, so your Gradle files aren't edited.
iOS signing uses a temporary keychain that is deleted when the build ends.

## iOS signing

Upload one distribution certificate and **one provisioning profile per bundle id**: the app and
each extension (widget, notification service, share extension, App Clip…). The runner:

- installs every profile and reads its bundle id from the profile itself,
- switches each app and extension target whose `PRODUCT_BUNDLE_IDENTIFIER` matches a profile to
  manual signing with that profile (simple `$(VAR)` references in the same configuration are
  resolved; Pods targets are never touched),
- stops before archiving if any app or extension target has no profile, listing the bundle ids
  to upload a profile for,
- exports the `.ipa` with every bundle id → profile pair in `ExportOptions.plist`.

Wildcard profiles (`*`) and profiles from another team are rejected.

## Network destinations

Only these, so you can enforce them with an egress allowlist (e.g. StepSecurity Harden-Runner):

- the Liftbay API (`api.liftbay.dev`, or `LIFTBAY_API_URL`)
- signed upload URLs on Liftbay's artifact storage, handed out by the API per file
- GitHub's OIDC token endpoint (`ACTIONS_ID_TOKEN_REQUEST_URL`)

Your build tools (npm, Gradle, CocoaPods) make their own connections. Source code is never
uploaded; only build artifacts and logs are.

## Verify a release

Every build log starts with the runner's version, commit and SHA-256. The workflow pins a version
and the archive's SHA-256 and refuses to run anything else.

Releases are built by `.github/workflows/release.yml` in this public repo, with SLSA build
provenance signed through Sigstore:

```sh
shasum -a 256 -c SHA256SUMS --ignore-missing
gh attestation verify liftbay-runner_0.1.0_linux_amd64.tar.gz --repo liftbay/runner
```

They are also reproducible. On Linux (GNU tar) with the Go version in `go.mod`:

```sh
git checkout v0.1.0
scripts/release.sh 0.1.0      # same bytes as the release: compare dist/SHA256SUMS
```

## Bring your own runner

Build it yourself (`make release VERSION=…`), host the archive anywhere, and set the repository
variables `LIFTBAY_RUNNER_URL` and `LIFTBAY_RUNNER_SHA256` (`…_MACOS` for iOS). The workflow
uses those instead of our release.

## Run locally

```sh
make build
LIFTBAY_API_URL=http://localhost:8787 LIFTBAY_BUILD_ID=bld_… LIFTBAY_JOB_TOKEN=lbj_… \
  bin/liftbay-runner run --workdir ~/code/my-app
```

`LIFTBAY_JOB_TOKEN` comes from the dev claim endpoint
(`POST /v1/orgs/{org}/projects/{project}/builds/{number}/dev-claim`).

## Develop

```sh
make test                     # go vet + go test -race
```

| Path | What |
|---|---|
| `cmd/liftbay-runner` | Entry point: auth, job fetch, signals |
| `internal/api` | Runner protocol client, GitHub OIDC |
| `internal/logship` | Redaction and ordered, batched log shipping |
| `internal/proc` | Commands in their own process group, output by line |
| `internal/recipe` | Steps per framework and platform |
| `internal/signing` | Upload keystore, keychain, provisioning profiles, project.pbxproj patching, ExportOptions |
| `workflow/liftbay.yml` | The GitHub Actions workflow Liftbay adds to your repo |
