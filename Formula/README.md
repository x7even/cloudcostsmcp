# opencloudcosts Homebrew Tap

## Installation

```sh
brew tap x7even/opencloudcosts https://github.com/x7even/homebrew-opencloudcosts
brew install opencloudcosts
```

## go install

If you have Go installed, you can also install directly:

```sh
go install github.com/x7even/cloudcostsmcp/opencloudcosts-go/cmd/opencloudcosts@latest
```

## About the formula

`opencloudcosts.rb` is a template-style formula for the
[x7even/homebrew-opencloudcosts](https://github.com/x7even/homebrew-opencloudcosts)
tap. It uses `on_macos`/`on_linux` + `on_arm`/`on_intel` blocks so Homebrew
fetches only the archive for the current machine's platform.

## Updating for a new release

At release time, replace the four `REPLACE_SHA256_*` placeholders in
`opencloudcosts.rb` with the actual SHA-256 checksums from
`checksums.txt` (produced by GoReleaser) and update the `version` field.

The SHA-256 values for each platform are listed in
`dist/checksums.txt` after running `goreleaser release --clean`.
Example workflow:

```sh
grep darwin_arm64 dist/checksums.txt   # copy the hash
# edit Formula/opencloudcosts.rb: replace REPLACE_SHA256_DARWIN_ARM64
```

Ideally a release workflow step automates this substitution before pushing
to the tap repo. For the first release (1.0.0) the update is manual.
