# Contributing to BeelineWF

Thank you for contributing. Please read this document before opening a
pull request. For setup, quickstart, and validation commands, see
[README.md](README.md).

## Contributor License Agreement (CLA) — required for every pull request

Every pull request requires a signed [CLA](CLA.md). No exceptions.

A pull request cannot be merged, even if CI passes, until the CLA check is
green for every human author on the pull request.

### How to sign

You sign by commenting the phrase the CLA bot shows:

1. Open your pull request.
2. Wait for the CLA bot to comment on the pull request. If you have not
   signed before, it will post the exact signing phrase and a link to
   [CLA.md](CLA.md). If you have already signed, the CLA check passes
   automatically and no action is needed.
3. Read [CLA.md](CLA.md) in full.
4. Post the exact phrase the bot shows as a new comment on your pull
   request. At the time of writing, that phrase is:

   > I have read the CLA Document and I hereby sign the CLA

   Copy it verbatim — exact wording, punctuation, and capitalization. Do
   not paraphrase it and do not put it in the pull request description,
   a commit message, or a review comment.
5. The bot verifies your comment, records your signature, and updates the
   CLA check. Merging is unblocked once the check is green.

Good to know:

- **One signature per person, one time.** Signing covers your future pull
  requests from the same GitHub account. If the CLA changes materially,
  the bot will ask you to sign again.
- **Each author signs for themselves.** If a pull request has multiple
  human authors, each author must post the phrase from their own GitHub
  account. You cannot sign on behalf of someone else.
- **Unsigned pull requests stay blocked.** Maintainers will not merge a
  pull request whose CLA check has not passed, whatever the CI result.
- **Signing as an employee.** If you contribute on behalf of an employer
  or another entity, make sure you are authorized to sign on its behalf
  before posting the phrase (see section 4 of [CLA.md](CLA.md)).
- **Problems signing?** Check that the comment is a plain new comment on
  the pull request from the account that authored the commits, with the
  phrase exactly as the bot showed it. If it still does not verify,
  mention the maintainers on the pull request.

## Before you start

- Fork the repo and create a feature branch from `main`.
- Keep changes surgical: touch only what the task needs, match existing
  style, and do not refactor unrelated code.
- Add tests for new behavior.

## Validation

Run the validation block from [README.md](README.md) before opening a pull
request:

```bash
gofmt -l .
go vet ./...
go test ./... -count=1
go test -race ./... -count=1
golangci-lint run ./...
atlas migrate validate --config file://migrations/atlas.hcl --env gorm \
  --var dev_url="postgres://gorm:gorm@localhost:9921/gorm?sslmode=disable"
```

`gofmt -l .` must print nothing and `go vet ./...` must be clean. Run the
Atlas command only if migrations changed, and `task swagger` only if API
annotations changed.

## Opening a pull request

1. Open the pull request against `main`.
2. Describe what changed and how it was verified.
3. Sign the CLA as described above when the bot prompts you.
4. Wait for CI and the CLA check to pass.

Good first checks on any pull request: `gofmt -l .` prints nothing,
`go vet ./...` is clean, and `task test` passes.

## Security

Found a vulnerability? Please use GitHub's private vulnerability
reporting on this repository instead of opening a public issue, so a fix
can land before disclosure. See the Security section in
[README.md](README.md).
