# ah-scanner (`ahc`)

WoW Forever auction house collector: one Go binary, one SQLite file, one Docker container on
the NUC. See README.md for what it does and how it's deployed.

## Commit messages: Conventional Commits

Every commit on a branch that will merge to `main` uses
[Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/). semantic-release reads
them on each push to `main` (go-semantic-release, `.github/workflows/build.yml`) to pick the
next version, build the image with it, then tag the commit and publish the GitHub release:

| Prefix | Use for | Release |
|---|---|---|
| `feat:` | a new feature or user-visible behaviour | minor (v1.**2**.0) |
| `fix:` | a bug fix | patch (v1.2.**1**) |
| `feat!:` / `fix!:` or a `BREAKING CHANGE:` footer | breaks config, data or the API | major (v**2**.0.0) |
| `perf:` `refactor:` `test:` `docs:` `build:` `ci:` `chore:` `style:` | everything else | none |

A change that should ship as a release (a speed-up users notice, a CI fix that must reach the
image) is `fix:` or `feat:`; only those two types release.

- Format: `type(optional scope): summary`, e.g. `feat(supply): chart gold per Favor over time`.
  Summary in the imperative, lower case after the colon, no trailing period.
- The body explains why. Breaking changes need a `BREAKING CHANGE: <what to do>` footer.
- One logical change per commit, so the type is honest; split a feature and an unrelated fix.
- Only the first line is parsed. Merge commits ("Merge pull request #n") and any other
  non-conventional subject are ignored, so the branch's own commits decide the release: fix a
  wrong message before merging (reword the commit).

## Versions

- Each image is stamped with its version (`internal/version`): the release (`v1.2.3`), or
  `git describe` output for builds without a release (`v1.2.3-4-gabc1234`). Local `go build`
  reports `dev-<commit>`.
- `ahc collect`, `once` and `serve` log `{"msg":"ahc starting","version":…}` first thing,
  `ahc version` prints it, and the Collector page shows the running collector's version. After deploying, check it matches the release
  or commit you expect.
- Stacked PRs: merge the base PR, wait for GitHub to retarget the next one to `main`, then merge
  it. A PR merged into a branch that already merged doesn't reach `main` or the image.

## Before pushing

- `gofmt -l .` prints nothing, `go vet ./...` and `go test ./...` pass.
- `node --check internal/web/static/app.js` for UI changes.
