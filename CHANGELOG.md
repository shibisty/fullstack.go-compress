# Changelog

## Unreleased

### Added
- `compress.New`: zstd, brotli and gzip by `Accept-Encoding` (q-values, `*`); fast levels while
  writing dynamic responses, maximal levels for responses with an ETag or `immutable`, kept in a
  bounded LRU; `Vary: Accept-Encoding`, weak ETags; streaming with `Flush`; event streams, small
  bodies, compressed types, `HEAD`, ranges and `no-transform` passed through.
