package compress

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

func decode(t *testing.T, enc string, b []byte) string {
	t.Helper()
	var r io.Reader
	switch enc {
	case "zstd":
		d, err := zstd.NewReader(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		r = d
	case "br":
		r = brotli.NewReader(bytes.NewReader(b))
	case "gzip":
		g, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		r = g
	default:
		return string(b)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(enc, err)
	}
	return string(out)
}

// big is JSON-like text, repetitive as real responses are, but not trivially.
var big = func() string {
	words := strings.Fields("note text title user email created updated draft public private tag list item order")
	var b strings.Builder
	x := uint32(7)
	for i := 0; i < 600; i++ {
		x = x*1664525 + 1013904223
		b.WriteString(`{"id":` + strings.Repeat("1", int(x%5)+1) + `,"` + words[x%uint32(len(words))] + `":"` + words[(x>>8)%uint32(len(words))] + `"},`)
	}
	return b.String()
}()

func serve(h http.Handler, method, accept string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/x?a=1", nil)
	if accept != "" {
		r.Header.Set("Accept-Encoding", accept)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestDynamic(t *testing.T) {
	mw := New(Options{})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "999")
		w.WriteHeader(201)
		io.WriteString(w, big[:500])
		io.WriteString(w, big[500:])
	}))
	for accept, want := range map[string]string{
		"gzip, deflate, br, zstd": "zstd",
		"br;q=1, gzip;q=0.5":      "br",
		"gzip":                    "gzip",
		"zstd;q=0, gzip":          "gzip",
		"*":                       "zstd",
		"":                        "",
		"identity":                "",
	} {
		w := serve(h, "GET", accept)
		if w.Code != 201 || w.Header().Get("Content-Encoding") != want || w.Header().Get("Vary") != "Accept-Encoding" {
			t.Fatalf("%q: %d %v", accept, w.Code, w.Header())
		}
		if want != "" && (w.Header().Get("Content-Length") != "" || w.Body.Len() >= len(big)) {
			t.Fatalf("%q: %v %d", accept, w.Header(), w.Body.Len())
		}
		if got := decode(t, want, w.Body.Bytes()); got != big {
			t.Fatalf("%q: body %q", accept, got[:40])
		}
	}
}

func TestPassThrough(t *testing.T) {
	mw := New(Options{})
	cases := map[string]http.HandlerFunc{
		"small": func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Content-Type", "text/html"); io.WriteString(w, "<p>hi</p>") },
		"image": func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Content-Type", "image/png"); io.WriteString(w, big) },
		"encoded": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Encoding", "gzip")
			io.WriteString(w, big)
		},
		"no-transform": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-transform")
			w.Header().Set("Content-Type", "text/plain")
			io.WriteString(w, big)
		},
		"304":   func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(304) },
		"empty": func(w http.ResponseWriter, r *http.Request) {},
		"redirect": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/y", http.StatusSeeOther)
		},
	}
	for name, fn := range cases {
		w := serve(mw(fn), "GET", "zstd, br, gzip")
		if w.Header().Get("Content-Encoding") != "" && name != "encoded" {
			t.Fatalf("%s: %v", name, w.Header())
		}
	}
	// HEAD and ranges are never compressed
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Content-Type", "text/plain"); io.WriteString(w, big) }))
	if w := serve(h, "HEAD", "gzip"); w.Header().Get("Content-Encoding") != "" {
		t.Fatal("HEAD")
	}
	if w := serve(h, "GET", "gzip", "Range", "bytes=0-10"); w.Header().Get("Content-Encoding") != "" {
		t.Fatal("Range")
	}
	// a body without a type is sniffed
	h = mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "<!doctype html>"+big) }))
	if w := serve(h, "GET", "gzip"); w.Header().Get("Content-Type") != "text/html; charset=utf-8" || w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal(w.Header())
	}
}

func TestKept(t *testing.T) {
	calls := 0
	h := New(Options{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/javascript")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("ETag", `"v1"`)
		io.WriteString(w, big)
	}))
	first := serve(h, "GET", "gzip, br, zstd")
	if first.Header().Get("Content-Encoding") != "br" || first.Header().Get("ETag") != `W/"v1"` || first.Header().Get("Content-Length") == "" {
		t.Fatal(first.Header())
	}
	if decode(t, "br", first.Body.Bytes()) != big {
		t.Fatal("body")
	}
	// maximal level: smaller than the fast one
	fast := serve(New(Options{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		io.WriteString(w, big)
	})), "GET", "br")
	if first.Body.Len() > fast.Body.Len() {
		t.Fatal(first.Body.Len(), fast.Body.Len())
	}
	start := time.Now()
	second := serve(h, "GET", "gzip, br, zstd")
	if !bytes.Equal(second.Body.Bytes(), first.Body.Bytes()) || calls != 2 || time.Since(start) > time.Second {
		t.Fatal("kept")
	}
	// another encoding is kept separately; no cache keeps nothing
	if g := serve(h, "GET", "gzip"); decode(t, "gzip", g.Body.Bytes()) != big {
		t.Fatal("gzip")
	}
	none := New(Options{CacheSize: -1})(h)
	if w := serve(none, "GET", "br"); decode(t, "br", w.Body.Bytes()) != big {
		t.Fatal("no cache")
	}
	c := newCache(10)
	c.put("a", []byte("123456"))
	c.put("b", []byte("123456")) // a goes
	c.put("b", []byte("x"))
	c.put("huge", make([]byte, 11))
	if _, ok := c.get("a"); ok {
		t.Fatal("not evicted")
	}
	if b, ok := c.get("b"); !ok || string(b) != "123456" {
		t.Fatal("b")
	}
}

func TestStreaming(t *testing.T) {
	srv := httptest.NewServer(New(Options{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "part one ")
		http.NewResponseController(w).Flush() // decides with less than MinSize: compressed
		time.Sleep(50 * time.Millisecond)
		io.WriteString(w, big)
	})))
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.Header.Get("Content-Encoding") != "gzip" || decode(t, "gzip", body) != "part one "+big {
		t.Fatal(res.Header)
	}
	// event streams pass, flushed as they come
	srv2 := httptest.NewServer(New(Options{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: 1\n\n")
		http.NewResponseController(w).Flush()
		<-r.Context().Done()
	})))
	defer srv2.Close()
	req, _ = http.NewRequest("GET", srv2.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	res, err = http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 9)
	io.ReadFull(res.Body, buf)
	res.Body.Close()
	if string(buf) != "data: 1\n\n" || res.Header.Get("Content-Encoding") != "" {
		t.Fatal(string(buf), res.Header)
	}
}

func TestCompressible(t *testing.T) {
	for ct, want := range map[string]bool{
		"text/html; charset=utf-8": true, "application/json": true, "application/vnd.fullstack.page+json": true,
		"application/manifest+json": true, "image/svg+xml": true, "text/javascript": true, "application/wasm": true,
		"text/event-stream": false, "image/png": false, "font/woff2": false, "application/zip": false, "": false,
	} {
		if Compressible(ct) != want {
			t.Error(ct)
		}
	}
}
