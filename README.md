# fullstack-compress

Response compression for [fullstack](https://github.com/shibisty/fullstack.go) (and any `net/http`
handler): zstd, brotli or gzip, as the client accepts, with two trade-offs:

| Response | Levels | |
|---|---|---|
| dynamic: pages, API JSON, page data | zstd 3, brotli 4, gzip 5 | compressed while written, streaming works |
| versioned: an `ETag`, or `Cache-Control: immutable` (hashed build files, the template pack, cached pages) | brotli 11, zstd 19, gzip 9 | compressed once, the result kept in memory |

## Installation

```bash
gtr add github:shibisty/fullstack.go-compress
```

## Use

```go
app.Router.Use(compress.New(compress.Options{}))
```

On the root router it runs for every request. For dynamic responses zstd comes first (fastest for
its ratio; Chrome, Edge and Firefox accept it), then brotli, then gzip; versioned ones prefer
brotli. In the example a 7 KB minified script is sent as 2.7 KB of brotli 11, compressed on the
first request (29 ms) and taken from memory afterwards; a page is about half its size.

Passed through untouched: bodies under `MinSize` (1 KB), types already compressed (images, woff2,
archives, video), `text/event-stream` (live reload, realtime), `HEAD`, ranges, `204`/`304`,
`Cache-Control: no-transform` and responses with their own `Content-Encoding`. `Vary:
Accept-Encoding` is set on everything compressible; the ETag of a compressed response becomes weak
(`W/"..."`), which `If-None-Match` matches.

| Option | Default | |
|---|---|---|
| `MinSize` | 1024 | smallest body compressed |
| `CacheSize` | 64 MB | memory for kept responses (least recently used go first); negative: none |
| `Encodings` | zstd, br, gzip | offered, in order of preference |
| `Compressible` | `compress.Compressible` | which content types are compressed |

**Secrets in compressed pages.** A page that holds a secret next to text an attacker can choose may
leak the secret by its compressed size (BREACH). fullstack-passport's CSRF tokens are masked anew on
every page, so they never repeat; do the same for other secrets in HTML, or send such pages with
`Cache-Control: no-transform`.

## License

MIT
