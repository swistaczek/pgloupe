# Releasing

How to cut a new pgloupe release. Set up once, then it's a single `git tag`.

## One-time setup

### 1. Personal Access Token for the Homebrew tap

GoReleaser pushes the formula to a separate repository (`swistaczek/homebrew-tap`). The default `GITHUB_TOKEN` GitHub Actions provides is scoped to the source repo only, so we need a PAT with cross-repo write access.

**Recommended: fine-grained PAT.**

1. Visit https://github.com/settings/personal-access-tokens/new
2. Token name: `pgloupe-homebrew-tap-write`
3. Resource owner: `swistaczek`
4. Repository access: **Only select repositories** → pick `swistaczek/homebrew-tap` (NOT pgloupe itself)
5. Repository permissions:
   - **Contents: Read and write** (push commits to Formula/)
   - **Metadata: Read-only** (auto-granted)
6. Expiration: 1 year
7. **Generate** → copy the `github_pat_…` token

### 2. Add the secret

1. Visit https://github.com/swistaczek/pgloupe/settings/secrets/actions
2. **New repository secret**
3. Name: `HOMEBREW_TAP_GITHUB_TOKEN`
4. Value: paste the PAT
5. **Add secret**

That's it.

## Cutting a release

Make sure `main` is green and the tree is clean:

```bash
git status                # nothing to commit, working tree clean
gh run list --limit 1     # most recent CI run is success
```

Tag and push:

```bash
git tag -a v0.1.0 -m "Initial release"
git push origin v0.1.0
```

GitHub Actions runs the `release` workflow, which takes ~3 minutes and produces:

- A GitHub Release at `https://github.com/swistaczek/pgloupe/releases/tag/v0.1.0` with four tarballs (`pgloupe_0.1.0_{Darwin,Linux}_{amd64,arm64}.tar.gz`) plus `checksums.txt`.
- A new commit on `swistaczek/homebrew-tap`'s `main` branch creating `Formula/pgloupe.rb` with the right version, URLs, and SHA256s. Author: `goreleaserbot`.

After ~3 minutes:

```bash
brew install swistaczek/tap/pgloupe
pgloupe --version           # → pgloupe v0.1.0 (commit ..., built ...)
```

`brew tap swistaczek/tap` is implicit — Homebrew taps automatically when you reference a formula by `<owner>/<tap>/<formula>`.

## Versioning

Follows [SemVer](https://semver.org/):

- `vX.0.0` — breaking changes (CLI flag rename, removed feature)
- `vX.Y.0` — new features (new flags, new modes)
- `vX.Y.Z` — bug fixes only

Pre-releases use `vX.Y.Z-rcN`. GoReleaser sets `prerelease: auto`, which means rc tags create a draft GitHub Release but **do not** push to the Homebrew tap. That's intentional — testers install from the GitHub Release tarball directly.

## Recovering from a failed release

If the workflow runs partially (release created, formula push failed, etc.):

1. Inspect the failure: `gh run view --log-failed` on the run.
2. Common causes:
   - **Token expired or revoked** — regenerate the PAT and update the secret.
   - **Tap default branch isn't `main`** — adjust `.goreleaser.yaml` `brews[].repository.branch`.
   - **Formula audit failed** — usually a `description`/`homepage`/`license` field went missing. Fix in `.goreleaser.yaml` and retag.
3. Delete the bad tag locally and remotely:
   ```bash
   git tag -d v0.1.0
   git push origin :refs/tags/v0.1.0
   ```
4. Delete the GitHub Release if one was created (Settings → Releases → Delete).
5. Fix the issue, push the fix to `main`, then retag. **Do not reuse the same version number for a different artifact** — bump the patch version (`v0.1.1`) instead. Force-pushing a tag means users with cached formulas get SHA256 mismatches.

## Updating dependencies

```bash
go get -u ./...
go mod tidy
go test -race ./...
```

If anything in the wire-protocol parser changes (`jackc/pgx`), re-read [`docs/DESIGN.md`](docs/DESIGN.md) §"Wire-protocol observation" before merging — the API has historically been stable but assumptions like `Receive()` aliasing are worth re-checking on major bumps.
