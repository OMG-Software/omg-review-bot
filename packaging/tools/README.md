# Packaging tools

A separate Go module that pins the tools the release workflow builds, so their
versions and checksums are locked in `go.mod` and `go.sum` instead of resolved
at build time. It is not part of the prbot module: `go build ./...` and
`go test ./...` at the repository root skip it.

To update nfpm:

```bash
go -C packaging/tools get -tool github.com/goreleaser/nfpm/v2/cmd/nfpm@latest
go -C packaging/tools mod tidy
```
