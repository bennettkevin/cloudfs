# Spike 11: encrypted names and blind index

Prototype of encrypted file names plus a blind index for case-insensitive uniqueness, with correctness tests and benchmarks. Findings: [docs/research/11-name-encryption.md](../../docs/research/11-name-encryption.md).

This is spike code, not part of CloudFS.

```sh
go test -v ./...                      # correctness + size report + 10k/100k timing
go test -run '^$' -bench . -benchmem  # benchmarks
```

- `TestIndexFormMatchesAPFS`: our "same name" rule against a table of pairs measured on APFS.
- `TestAPFSLive`: re-measures that table on this machine if the temp dir is case-insensitive (skipped otherwise).
- `TestFoldMatchesUnicode`: our case folding against every code point in `testdata/CaseFolding-17.0.0.txt` (Unicode 17.0.0, matching the pinned `golang.org/x/text`).
- `TestRoundTripAndBinding`: names decrypt only for the node and parent they were encrypted for.
