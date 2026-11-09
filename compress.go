// Package compress compresses HTTP responses with zstd, brotli or gzip, as
// the client accepts (gtr package fullstack-compress). It is plain net/http
// middleware:
//
//	app.Router.Use(compress.New(compress.Options{}))
//
// Two kinds of responses get two trade-offs:
//
//   - dynamic ones (pages, API JSON) are compressed while they are written,
//     at fast levels: zstd 3, brotli 4, gzip 5;
//   - responses that stay the same for a while, those with an ETag or with
//     Cache-Control immutable (hashed build files, the template pack,
//     cached pages), get the maximal levels (brotli 11, zstd 19, gzip 9),
//     and the compressed bytes are kept in memory, so each is compressed
//     once.
//
// Small bodies (under MinSize), formats that are compressed already (images,
// woff2, archives), event streams, ranges and responses that set
// Content-Encoding themselves pass through.
package compress

import (
	"bytes"
	"compress/gzip"
	"container/list"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// Options configure New.
type Options struct {
	// MinSize is the smallest body compressed (default 1024 bytes).
	MinSize int
	// CacheSize bounds the memory for kept compressed responses (default
	// 64 MB; negative: keep none).
	CacheSize int64
	// Encodings are the ones offered, preferred first for dynamic responses
	// (default zstd, br, gzip); kept responses prefer br.
	Encodings []string
	// Compressible reports whether a content type is worth compressing
	// (default: text, JSON, JavaScript, XML, SVG, manifests, wasm, fonts
	// other than woff/woff2).
	Compressible func(contentType string) bool
}

const (
	zstdEnc = "zstd"
	brEnc   = "br"
	gzipEnc = "gzip"
)

// New returns the middleware.
func New(o Options) func(http.Handler) http.Handler {
	if o.MinSize <= 0 {
		o.MinSize = 1024
	}
	if o.CacheSize == 0 {
		o.CacheSize = 64 << 20
	}
	if len(o.Encodings) == 0 {
		o.Encodings = []string{zstdEnc, brEnc, gzipEnc}
	}
	if o.Compressible == nil {
		o.Compressible = Compressible
	}
	c := &compressor{o: o, cache: newCache(o.CacheSize)}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead || r.Header.Get("Range") != "" {
				next.ServeHTTP(w, r)
				return
			}
			accepted := acceptedEncodings(r.Header.Get("Accept-Encoding"))
			cw := &writer{ResponseWriter: w, c: c, r: r, accepted: accepted}
			defer cw.finish()
			next.ServeHTTP(cw, r)
		})
	}
}

// Compressible is the default Options.Compressible.
func Compressible(contentType string) bool {
	t, _, _ := mime.ParseMediaType(contentType)
	switch {
	case t == "text/event-stream":
		return false
	case strings.HasPrefix(t, "text/"):
		return true
	case strings.HasSuffix(t, "+json"), strings.HasSuffix(t, "+xml"), strings.HasSuffix(t, "/json"), strings.HasSuffix(t, "/xml"):
		return true
	}
	switch t {
	case "application/javascript", "application/x-javascript", "application/ecmascript", "image/svg+xml",
		"application/wasm", "font/ttf", "font/otf", "application/vnd.ms-fontobject", "image/x-icon", "image/vnd.microsoft.icon":
		return true
	}
	return false
}

// acceptedEncodings parses Accept-Encoding: encoding → acceptable (q > 0).
func acceptedEncodings(h string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(h, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		q := 1.0
		if k, v, ok := strings.Cut(strings.TrimSpace(params), "="); ok && strings.TrimSpace(k) == "q" {
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				q = f
			}
		}
		out[name] = q > 0
	}
	if out["*"] {
		for _, e := range []string{zstdEnc, brEnc, gzipEnc} {
			if _, set := out[e]; !set {
				out[e] = true
			}
		}
	}
	return out
}

type compressor struct {
	o     Options
	cache *cache
	zfast sync.Pool
	gfast sync.Pool
}

// pick chooses an encoding: for kept responses brotli first.
func (c *compressor) pick(accepted map[string]bool, keep bool) string {
	order := c.o.Encodings
	if keep {
		order = append([]string{brEnc}, order...)
	}
	for _, e := range order {
		if accepted[e] {
			for _, offered := range c.o.Encodings {
				if offered == e {
					return e
				}
			}
		}
	}
	return ""
}

// encoder returns a writer compressing into w and a function that releases
// it; max picks the maximal level.
func (c *compressor) encoder(enc string, w io.Writer, max bool) (io.WriteCloser, func()) {
	switch enc {
	case zstdEnc:
		if max {
			e, _ := zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.SpeedBestCompression), zstd.WithEncoderConcurrency(1))
			return e, func() {}
		}
		e, _ := c.zfast.Get().(*zstd.Encoder)
		if e == nil {
			e, _ = zstd.NewWriter(w, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1), zstd.WithLowerEncoderMem(true))
		} else {
			e.Reset(w)
		}
		return e, func() { c.zfast.Put(e) }
	case brEnc:
		level := 4
		if max {
			level = brotli.BestCompression
		}
		return brotli.NewWriterLevel(w, level), func() {}
	default:
		if max {
			g, _ := gzip.NewWriterLevel(w, gzip.BestCompression)
			return g, func() {}
		}
		g, _ := c.gfast.Get().(*gzip.Writer)
		if g == nil {
			g, _ = gzip.NewWriterLevel(w, 5)
		} else {
			g.Reset(w)
		}
		return g, func() { c.gfast.Put(g) }
	}
}

// writer decides once the headers are known (and MinSize bytes are
// written, or the handler ends or flushes) whether and how to compress.
type writer struct {
	http.ResponseWriter
	c        *compressor
	r        *http.Request
	accepted map[string]bool

	status  int
	decided bool
	pass    bool // write through, uncompressed
	keep    bool // compress at the end at the maximal level and keep it
	enc     string
	buf     bytes.Buffer
	out     io.WriteCloser
	release func()
}

func (w *writer) WriteHeader(code int) {
	if code < 200 { // 1xx: informational, sent as they come
		w.ResponseWriter.WriteHeader(code)
		return
	}
	if w.status == 0 {
		w.status = code
	}
}

func (w *writer) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if !w.decided {
		w.buf.Write(p)
		if w.buf.Len() >= w.c.o.MinSize {
			w.decide(false)
		}
		return len(p), nil
	}
	switch {
	case w.pass:
		return w.ResponseWriter.Write(p)
	case w.keep:
		return w.buf.Write(p)
	default:
		return w.out.Write(p)
	}
}

// decide is called with the first MinSize bytes buffered, or at the end
// (final) with the whole body.
func (w *writer) decide(final bool) {
	w.decided = true
	h := w.Header()
	ct := h.Get("Content-Type")
	if ct == "" {
		ct = http.DetectContentType(w.buf.Bytes())
		h.Set("Content-Type", ct)
	}
	compressible := w.c.o.Compressible(ct)
	if compressible && h.Get("Content-Encoding") == "" {
		addVary(h, "Accept-Encoding")
	}
	cc := h.Get("Cache-Control")
	w.keep = h.Get("ETag") != "" || strings.Contains(cc, "immutable")
	w.enc = w.c.pick(w.accepted, w.keep)
	switch {
	case !compressible, w.enc == "", h.Get("Content-Encoding") != "", h.Get("Content-Range") != "",
		w.status == http.StatusNoContent, w.status == http.StatusNotModified, w.status == http.StatusPartialContent,
		strings.Contains(cc, "no-transform"),
		final && w.buf.Len() < w.c.o.MinSize:
		w.pass = true
		w.ResponseWriter.WriteHeader(w.status)
		w.ResponseWriter.Write(w.buf.Bytes())
		w.buf.Reset()
		return
	}
	h.Del("Content-Length")
	h.Del("Accept-Ranges")
	h.Set("Content-Encoding", w.enc)
	if etag := h.Get("ETag"); etag != "" && !strings.HasPrefix(etag, "W/") {
		h.Set("ETag", "W/"+etag) // another representation of the same content
	}
	if w.keep {
		return // the whole body first
	}
	w.ResponseWriter.WriteHeader(w.status)
	w.out, w.release = w.c.encoder(w.enc, w.ResponseWriter, false)
	w.out.Write(w.buf.Bytes())
	w.buf.Reset()
}

// Flush sends what is written so far (streaming responses).
func (w *writer) Flush() {
	if !w.decided {
		if w.status == 0 {
			w.status = http.StatusOK
		}
		w.decide(false)
	}
	if w.keep && !w.pass {
		return // compressed whole at the end
	}
	if w.out != nil {
		if f, ok := w.out.(interface{ Flush() error }); ok {
			f.Flush()
		}
	}
	http.NewResponseController(w.ResponseWriter).Flush()
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *writer) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *writer) finish() {
	if w.status == 0 {
		return // nothing written: the server answers 200 with no body
	}
	if !w.decided {
		w.decide(true)
	}
	switch {
	case w.pass:
	case w.keep:
		w.writeKept()
	default:
		w.out.Close()
		w.release()
	}
}

// writeKept compresses the whole body at the maximal level, once per
// content: later responses with the same body take the kept bytes.
func (w *writer) writeKept() {
	body := w.buf.Bytes()
	key := w.enc + "\x00" + w.r.URL.RequestURI() + "\x00" + w.Header().Get("ETag") + "\x00" + strconv.Itoa(len(body)) + "\x00" + checksum(body)
	data, ok := w.c.cache.get(key)
	if !ok {
		var b bytes.Buffer
		enc, release := w.c.encoder(w.enc, &b, true)
		enc.Write(body)
		enc.Close()
		release()
		data = b.Bytes()
		w.c.cache.put(key, data)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.ResponseWriter.WriteHeader(w.status)
	w.ResponseWriter.Write(data)
}

func addVary(h http.Header, v string) {
	for _, have := range h.Values("Vary") {
		for _, x := range strings.Split(have, ",") {
			if strings.EqualFold(strings.TrimSpace(x), v) || strings.TrimSpace(x) == "*" {
				return
			}
		}
	}
	h.Add("Vary", v)
}

// checksum is FNV-1a: the kept bytes must belong to the same body.
func checksum(b []byte) string {
	var h uint64 = 14695981039346656037
	for _, c := range b {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return strconv.FormatUint(h, 36)
}

// cache keeps compressed bodies, least recently used out first.
type cache struct {
	mu    sync.Mutex
	limit int64
	size  int64
	ll    *list.List
	items map[string]*list.Element
}

type entry struct {
	key  string
	data []byte
}

func newCache(limit int64) *cache {
	return &cache{limit: limit, ll: list.New(), items: map[string]*list.Element{}}
}

func (c *cache) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.ll.MoveToFront(e)
		return e.Value.(*entry).data, true
	}
	return nil, false
}

func (c *cache) put(key string, data []byte) {
	if c.limit <= 0 || int64(len(data)) > c.limit {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[key]; ok {
		return
	}
	c.items[key] = c.ll.PushFront(&entry{key, data})
	c.size += int64(len(data))
	for c.size > c.limit {
		e := c.ll.Back()
		c.ll.Remove(e)
		delete(c.items, e.Value.(*entry).key)
		c.size -= int64(len(e.Value.(*entry).data))
	}
}
