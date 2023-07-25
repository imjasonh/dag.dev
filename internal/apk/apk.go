package apk

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/digitorus/timestamp"
	"github.com/fxamacker/cbor/v2"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/logs"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/types"
	httpserve "github.com/jonjohnsonjr/dag.dev/internal/forks/http"
	"github.com/jonjohnsonjr/dag.dev/internal/soci"
	"github.com/jonjohnsonjr/dag.dev/internal/xxd"
	hgzip "github.com/nanmu42/gzip"
	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"
)

// We should not buffer blobs greater than 4MB
const tooBig = 1 << 22
const respTooBig = 1 << 25

type handler struct {
	mux       http.Handler
	keychain  authn.Keychain
	userAgent string

	// digest -> remote.desc
	manifests map[string]*remote.Descriptor

	// reg.String() -> ping resp
	pings map[string]*transport.PingResp

	tocCache   cache
	indexCache cache

	sync.Mutex
	sawTags map[string][]string

	oauth *oauth2.Config
}

type Option func(h *handler)

func WithUserAgent(ua string) Option {
	return func(h *handler) {
		h.userAgent = ua
	}
}

func New(opts ...Option) http.Handler {
	h := handler{
		manifests:  map[string]*remote.Descriptor{},
		pings:      map[string]*transport.PingResp{},
		sawTags:    map[string][]string{},
		tocCache:   buildTocCache(),
		indexCache: buildIndexCache(),
	}

	for _, opt := range opts {
		opt(&h)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/", h.errHandler(h.renderResponse))

	mux.HandleFunc("/fs/", h.errHandler(h.renderFS))

	// Janky workaround for downloading via the "urls" field.
	mux.HandleFunc("/http/", h.errHandler(h.renderFS))
	mux.HandleFunc("/https/", h.errHandler(h.renderFS))

	mux.HandleFunc("/layers/", h.errHandler(h.renderLayers))
	mux.HandleFunc("/cache/", h.errHandler(h.renderIndex))

	// Try to detect mediaType.
	mux.HandleFunc("/blob/", h.errHandler(h.renderFS))

	h.mux = hgzip.DefaultHandler().WrapHandler(mux)

	return &h
}

func splitFsURL(p string) (string, string, error) {
	for _, prefix := range []string{"/fs/", "/layers/", "/https/", "/http/", "/blob/", "/cache/"} {
		if strings.HasPrefix(p, prefix) {
			return strings.TrimPrefix(p, prefix), prefix, nil
		}
	}

	return "", "", fmt.Errorf("unexpected path: %v", p)
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.oauth == nil {
		// Cloud run already logs this stuff, don't log extra.
		log.Printf("%v", r.URL)

		start := time.Now()
		defer func() {
			log.Printf("%v (%s)", r.URL, time.Since(start))
		}()
	}

	if r.URL.Path == "/favicon.svg" || r.URL.Path == "/favicon.ico" {
		w.Header().Set("Cache-Control", "max-age=3600")
		http.ServeFile(w, r, filepath.Join(os.Getenv("KO_DATA_PATH"), "favicon.svg"))
		return
	}
	if r.URL.Path == "/robots.txt" {
		w.Header().Set("Cache-Control", "max-age=3600")
		http.ServeFile(w, r, filepath.Join(os.Getenv("KO_DATA_PATH"), "robots.txt"))
		return
	}

	h.mux.ServeHTTP(w, r)
}

type HandleFuncE func(http.ResponseWriter, *http.Request) error

func (h *handler) errHandler(hfe HandleFuncE) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := hfe(w, r); err != nil {
			log.Printf("%s: %v", r.URL.Path, err)
			fmt.Fprintf(w, "failed: %s", html.EscapeString(err.Error()))
		}
	}
}

func (h *handler) renderResponse(w http.ResponseWriter, r *http.Request) error {
	qs := r.URL.Query()

	if image := qs.Get("image"); image != "" {
		return h.renderManifest(w, r, strings.TrimPrefix(strings.TrimSpace(image), "https://"))
	}
	if blob := qs.Get("blob"); blob != "" {
		return h.renderBlobJSON(w, r, strings.TrimPrefix(strings.TrimSpace(blob), "https://"))
	}
	if image := qs.Get("referrers"); image != "" {
		return h.renderReferrers(w, r, image)
	}

	// Cache landing page for 5 minutes.
	w.Header().Set("Cache-Control", "max-age=300")
	w.Write([]byte(landingPage))

	return nil
}

// Render manifests with links to blobs, manifests, etc.
func (h *handler) renderManifest(w http.ResponseWriter, r *http.Request, image string) error {
	ref, err := name.ParseReference(image, name.WeakValidation)
	if err != nil {
		return err
	}

	desc, err := h.fetchManifest(w, r, ref)
	if err != nil {
		return fmt.Errorf("fetchManifest: %w", err)
	}

	header := h.manifestHeader(ref, desc.Descriptor)

	u := *r.URL
	if _, ok := ref.(name.Digest); ok {
		// Allow this to be cached for an hour.
		w.Header().Set("Cache-Control", "max-age=3600, immutable")
	} else {
		// Rewrite links to include digest (not tag) for better caching.
		newImage := image + "@" + desc.Digest.String()
		qs := u.Query()
		qs.Set("image", newImage)
		u.RawQuery = qs.Encode()
	}

	if err := headerTmpl.Execute(w, TitleData{image}); err != nil {
		return fmt.Errorf("headerTmpl: %w", err)
	}

	output := &jsonOutputter{
		w:     w,
		u:     &u,
		fresh: []bool{},
		repo:  ref.Context().String(),
		mt:    string(desc.MediaType),
	}

	// Mutates header for bodyTmpl.
	b, err := h.jq(output, desc.Manifest, r, header)
	if err != nil {
		return fmt.Errorf("h.jq: %w", err)
	}

	if r.URL.Query().Get("render") == "x509" {
		if bytes.Count(b, []byte("-----BEGIN CERTIFICATE-----")) > 1 {
			header.JQ += " | while openssl x509 -text -noout 2>/dev/null; do :; done"
		} else {
			header.JQ += " | openssl x509 -text -noout"
		}
	} else if r.URL.Query().Get("render") == "history" {
		header.JQ = strings.TrimSuffix(header.JQ, " | jq .")
		header.JQ += ` | jq '.history[] | .v1Compatibility' -r | jq '.container_config.Cmd | join(" ")' -r | tac`
	}

	header.SizeLink = fmt.Sprintf("/sizes/%s?mt=%s&size=%d", ref.Context().Digest(desc.Digest.String()).String(), desc.MediaType, desc.Size)

	if err := bodyTmpl.Execute(w, header); err != nil {
		return fmt.Errorf("bodyTmpl: %w", err)
	}

	if err := h.renderContent(w, r, ref, b, output, u); err != nil {
		return err
	}

	fmt.Fprintf(w, footer)

	return nil
}

func (h *handler) renderReferrers(w http.ResponseWriter, r *http.Request, src string) error {
	ref, err := name.NewDigest(src)
	if err != nil {
		return err
	}

	opts := h.remoteOptions(w, r, ref.Context().Name())

	idx, err := remote.Referrers(ref, opts...)
	if err != nil {
		return err
	}

	desc, err := partial.Descriptor(idx)
	if err != nil {
		return err
	}

	header := h.manifestHeader(ref.Digest(desc.Digest.String()), *desc)
	header.Referrers = false
	header.Subject = ref.Identifier()

	if err := headerTmpl.Execute(w, TitleData{src}); err != nil {
		return fmt.Errorf("headerTmpl: %w", err)
	}

	u := *r.URL

	output := &jsonOutputter{
		w:     w,
		u:     &u,
		fresh: []bool{},
		repo:  ref.Context().String(),
		mt:    string(types.OCIImageIndex),
	}

	b, err := idx.RawManifest()
	if err != nil {
		return err
	}

	b, err = h.jq(output, b, r, header)
	if err != nil {
		return fmt.Errorf("h.jq: %w", err)
	}

	if err := bodyTmpl.Execute(w, header); err != nil {
		return fmt.Errorf("bodyTmpl: %w", err)
	}

	if err := h.renderContent(w, r, ref, b, output, u); err != nil {
		return err
	}

	fmt.Fprintf(w, footer)

	return nil
}

// Render blob as JSON, possibly containing refs to images.
func (h *handler) renderBlobJSON(w http.ResponseWriter, r *http.Request, blobRef string) error {
	ref, err := name.NewDigest(blobRef)
	if err != nil {
		return fmt.Errorf("NewDigest: %w", err)
	}

	opts := h.remoteOptions(w, r, ref.Context().Name())
	opts = append(opts, remote.WithMaxSize(tooBig))

	l, err := remote.Layer(ref, opts...)
	if err != nil {
		return fmt.Errorf("remote.Layer: %w", err)
	}
	blob, err := l.Compressed()
	if err != nil {
		return fmt.Errorf("Compressed: %w", err)
	}
	defer blob.Close()

	size, err := l.Size()
	if err != nil {
		log.Printf("layer %s Size(): %v", ref, err)
		return fmt.Errorf("cannot check blob size: %w", err)
	}
	// Allow this to be cached for an hour.
	w.Header().Set("Cache-Control", "max-age=3600, immutable")

	if err := headerTmpl.Execute(w, TitleData{blobRef}); err != nil {
		return fmt.Errorf("headerTmpl: %w", err)
	}

	digest := ref.Identifier()
	hash, err := v1.NewHash(digest)
	if err != nil {
		return fmt.Errorf("NewHash: %w", err)
	}

	output := &jsonOutputter{
		w:     w,
		u:     r.URL,
		fresh: []bool{},
		repo:  ref.Context().String(),
		mt:    r.URL.Query().Get("mt"),
	}

	mediaType := types.MediaType("application/octet-stream")
	if output.mt != "" {
		mediaType = types.MediaType(output.mt)
	}

	desc := v1.Descriptor{
		Size:      size,
		Digest:    hash,
		MediaType: mediaType,
	}
	header := headerData(ref, desc)
	header.Up = &RepoParent{
		Parent:    ref.Context().String(),
		Separator: "@",
		Child:     ref.Identifier(),
	}
	header.JQ = crane("blob") + " " + ref.String()

	if size > tooBig {
		header.JQ += fmt.Sprintf(" | head -c %d", httpserve.TooBig)
		if err := bodyTmpl.Execute(w, header); err != nil {
			return fmt.Errorf("bodyTmpl: %w", err)
		}
		dumb := &dumbEscaper{buf: bufio.NewWriter(w)}
		if _, err := io.CopyN(dumb, blob, httpserve.TooBig); err != nil {
			return err
		}
		fmt.Fprintf(w, footer)

		return nil
	}

	// TODO: Can we do this in a streaming way?
	input, err := ioutil.ReadAll(io.LimitReader(blob, tooBig))
	if err != nil {
		return err
	}

	if string(mediaType) == "application/cose" {
		var v interface{}
		if err := cbor.Unmarshal(input, &v); err != nil {
			return err
		}
		j, err := jsonify(v)
		if err != nil {
			return fmt.Errorf("jsonify: %w", err)
		}
		b, err := json.Marshal(j)
		if err != nil {
			return fmt.Errorf("json.Marshal: %w", err)
		}

		input = b
	}

	// Mutates header for bodyTmpl.
	b, err := h.jq(output, input, r, header)
	if err != nil {
		return fmt.Errorf("h.jq: %w", err)
	}

	if r.URL.Query().Get("render") == "history" {
		header.JQ = strings.TrimSuffix(header.JQ, " | jq .")
		header.JQ += " | jq '.history[] | .created_by' -r"
	} else if r.URL.Query().Get("render") == "der" {
		header.JQ += " | openssl x509 -inform der -text -noout"
	} else if r.URL.Query().Get("render") == "xxd" {
		header.JQ += " | xxd"
	}

	if err := bodyTmpl.Execute(w, header); err != nil {
		return fmt.Errorf("bodyTmpl: %w", err)
	}

	if err := h.renderContent(w, r, ref, b, output, *r.URL); err != nil {
		if r.URL.Query().Get("render") == "xxd" {
			return fmt.Errorf("renderContent: %w", err)
		}

		r.URL.Query().Set("render", "xxd")
		fmt.Fprintf(w, "NOTE: failed to render: %v\n", err)
		if err := renderOctets(w, r, b); err != nil {
			return fmt.Errorf("renderContent fallback: %w", err)
		}
	}

	fmt.Fprintf(w, footer)

	return nil
}

func renderOctets(w http.ResponseWriter, r *http.Request, b []byte) error {
	fmt.Fprintf(w, "<pre>")
	if _, err := io.Copy(xxd.NewWriter(w, int64(len(b))), bytes.NewReader(b)); err != nil {
		return err
	}
	fmt.Fprintf(w, "</pre>")

	return nil
}

func (h *handler) renderContent(w http.ResponseWriter, r *http.Request, ref name.Reference, b []byte, output *jsonOutputter, u url.URL) error {
	switch r.URL.Query().Get("render") {
	case "raw":
		fmt.Fprintf(w, "<pre>")
		if _, err := w.Write(b); err != nil {
			return err
		}
		fmt.Fprintf(w, "</pre>")
	case "x509":
		return renderx509(w, b)
	case "cert":
		return renderCert(w, b, u)
	case "der":
		return renderDer(w, b)
	case "xxd":
		return renderOctets(w, r, b)
	case "timestamp":
		ts, err := timestamp.Parse(b)
		if err != nil {
			return err
		}
		j, err := json.Marshal(ts)
		if err != nil {
			return err
		}
		return renderJSON(output, j)
	default:
		return renderJSON(output, b)
	}

	return nil

}

func (h *handler) renderFile(w http.ResponseWriter, r *http.Request, ref name.Digest, kind string, blob *sizeSeeker) error {
	mt := r.URL.Query().Get("mt")

	// Allow this to be cached for an hour.
	w.Header().Set("Cache-Control", "max-age=3600, immutable")

	httpserve.ServeContent(w, r, "", time.Time{}, blob, func(w http.ResponseWriter, ctype string) error {
		// Kind at this poin can be "gzip", "zstd" or ""
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := headerTmpl.Execute(w, TitleData{ref.String()}); err != nil {
			return err
		}
		hash, err := v1.NewHash(ref.Identifier())
		if err != nil {
			return err
		}
		desc := v1.Descriptor{
			Digest:    hash,
			MediaType: types.MediaType(mt),
		}
		if size := r.URL.Query().Get("size"); size != "" {
			if parsed, err := strconv.ParseInt(size, 10, 64); err == nil {
				desc.Size = parsed
			}
		}
		header := headerData(ref, desc)
		header.Up = &RepoParent{
			Parent:    ref.Context().String(),
			Separator: "@",
			Child:     ref.Identifier(),
		}
		header.JQ = crane("blob") + " " + ref.String()
		if kind == "zstd" {
			header.JQ += " | zstd -d"
		} else if kind == "gzip" {
			header.JQ += " | gunzip"
		}
		if blob.size < 0 || blob.size > httpserve.TooBig {
			header.JQ += fmt.Sprintf(" | head -c %d", httpserve.TooBig)
		}
		if !strings.HasPrefix(ctype, "text/") && !strings.Contains(ctype, "json") {
			header.JQ += " | xxd"
		}

		return bodyTmpl.Execute(w, header)
	})

	return nil
}

func (h *handler) renderFS(w http.ResponseWriter, r *http.Request) error {
	mt := r.URL.Query().Get("mt")

	dig, ref, err := h.getDigest(w, r)
	if err != nil {
		return fmt.Errorf("getDigest: %w", err)
	}

	isImage := strings.HasPrefix(mt, "image/")
	if isImage {
		return h.renderImage(w, r, dig, mt)
	}

	index, err := h.getIndex(r.Context(), dig.Identifier())
	if err != nil {
		return fmt.Errorf("indexCache.Index(%q) = %w", dig.Identifier(), err)
	}
	if index != nil {
		fs, err := h.indexedFS(w, r, dig, ref, index)
		if err != nil {
			return err
		}
		httpserve.FileServer(httpserve.FS(fs)).ServeHTTP(w, r)
		return nil
	}

	// Determine if this is actually a filesystem thing.
	blob, ref, err := h.fetchBlob(w, r)
	if err != nil {
		return fmt.Errorf("fetchBlob: %w", err)
	}

	kind, original, unwrapped, err := h.tryNewIndex(w, r, dig, ref, blob)
	if err != nil {
		return fmt.Errorf("failed to index blob %q: %w", dig.String(), err)
	}
	if unwrapped != nil {
		logs.Debug.Printf("unwrapped, kind = %q", kind)
		seek := &sizeSeeker{unwrapped, -1}
		return h.renderFile(w, r, dig, kind, seek)
	}
	if original != nil {
		logs.Debug.Printf("original")
		seek := &sizeSeeker{original, blob.size}
		return h.renderFile(w, r, dig, kind, seek)
	}

	return nil
}

func (h *handler) renderImage(w http.ResponseWriter, r *http.Request, ref name.Digest, mt string) error {
	url, err := h.resolveUrl(w, r)
	if err := headerTmpl.Execute(w, TitleData{ref.String()}); err != nil {
		return err
	}
	hash, err := v1.NewHash(ref.Identifier())
	if err != nil {
		return err
	}
	desc := v1.Descriptor{
		Digest:    hash,
		MediaType: types.MediaType(mt),
	}
	if size := r.URL.Query().Get("size"); size != "" {
		if parsed, err := strconv.ParseInt(size, 10, 64); err == nil {
			desc.Size = parsed
		}
	}
	header := headerData(ref, desc)
	header.Up = &RepoParent{
		Parent:    ref.Context().String(),
		Separator: "@",
		Child:     ref.Identifier(),
	}
	header.JQ = "curl " + url

	if err := bodyTmpl.Execute(w, header); err != nil {
		return err
	}

	fmt.Fprintf(w, "<img src=%q></img>", url)
	fmt.Fprintf(w, "</body></html>")

	return nil
}

func (h *handler) indexedFS(w http.ResponseWriter, r *http.Request, dig name.Digest, ref string, index soci.Index) (*soci.SociFS, error) {
	toc := index.TOC()
	if toc == nil {
		return nil, fmt.Errorf("this should not happen")
	}
	mt := r.URL.Query().Get("mt")
	if mt == "" {
		mt = toc.MediaType
	}

	opts := []remote.Option{}
	foreign := strings.HasPrefix(r.URL.Path, "/http/") || strings.HasPrefix(r.URL.Path, "/https/")
	if !foreign {
		// Skip the ping for foreign layers.
		opts = h.remoteOptions(w, r, dig.Context().Name())
	}

	opts = append(opts, remote.WithSize(toc.Csize))

	cachedUrl := ""
	// For foreign layers, we aren't hitting the registry. We want
	// to reuse some code from remote.BlobSeeker but without the
	// ping/token stuff, so we transport.Wrap a plain transport.
	if foreign {
		p := r.URL.Path
		scheme := "https://"
		if strings.HasPrefix(r.URL.Path, "/http/") {
			p = strings.TrimPrefix(p, "/http/")
			scheme = "http://"
		} else {
			p = strings.TrimPrefix(p, "/https/")
		}
		if before, _, ok := strings.Cut(p, "@"); ok {
			u, err := url.PathUnescape(before)
			if err != nil {
				return nil, err
			}
			u = scheme + u
			cachedUrl = u

			t := remote.DefaultTransport
			t = transport.NewRetry(t)
			t = transport.NewUserAgent(t, h.userAgent)
			if r.URL.Query().Get("trace") != "" {
				t = transport.NewTracer(t)
			}
			t = transport.Wrap(t)
			opts = append(opts, remote.WithTransport(t))
		}
	}

	// We can't set the cookie after calling soci.FS because we will
	// have already sent the body. We use remote.LazyBlob because we
	// don't actually need to send any HTTP requests if they're just
	// browsing directories and not loading file content.
	setCookie := func(blob *remote.BlobSeeker) error {
		return nil
	}

	blob := remote.LazyBlob(dig, cachedUrl, setCookie, opts...)
	prefix := strings.TrimPrefix(ref, "/")
	fs := soci.FS(index, blob, prefix, dig.String(), respTooBig, types.MediaType(mt), renderHeader)

	return fs, nil
}

func (h *handler) multiFS(w http.ResponseWriter, r *http.Request, dig name.Digest, desc *remote.Descriptor, ref string) (*soci.MultiFS, error) {
	m, err := v1.ParseManifest(bytes.NewReader(desc.Manifest))
	if err != nil {
		return nil, err
	}

	opts := h.remoteOptions(w, r, dig.Context().Name())
	opts = append(opts, remote.WithMaxSize(tooBig))

	fss := make([]*soci.SociFS, len(m.Layers))
	var g errgroup.Group
	for i, layer := range m.Layers {
		i := i
		size := layer.Size
		digest := layer.Digest
		urls := layer.URLs
		layerRef := dig.Context().Digest(layer.Digest.String())
		mediaType := layer.MediaType

		if digest.String() == emptyDigest {
			continue
		}

		g.Go(func() error {
			index, err := h.getIndex(r.Context(), digest.String())
			if err != nil {
				return fmt.Errorf("indexCache.Index(%q) = %w", dig.Identifier(), err)
			}
			if index == nil {
				l, err := remote.Layer(layerRef)
				if err != nil {
					return err
				}
				rc, err := l.Compressed()
				if err != nil {
					return err
				}

				index, err = h.createIndex(r.Context(), rc, size, digest.String(), 0, string(mediaType))
				if err != nil {
					return fmt.Errorf("createIndex: %w", err)
				}
				if index == nil {
					// Non-indexable blobs are filtered later.
					return nil
				}
			}

			fs, err := h.createFs(w, r, ref, layerRef, index, size, mediaType, urls, opts)
			if err != nil {
				return err
			}
			// NOTE: reverses order
			fss[(len(m.Layers)-1)-i] = fs
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	prefix := strings.TrimPrefix(ref, "/")
	return soci.NewMultiFS(fss, prefix, dig, desc.Size, desc.MediaType, renderDir), nil
}

// Flatten layers of an image and serve as a filesystem.
func (h *handler) renderLayers(w http.ResponseWriter, r *http.Request) error {
	dig, ref, err := h.getDigest(w, r)
	if err != nil {
		return err
	}

	desc, err := h.fetchManifest(w, r, dig)
	if err != nil {
		return err
	}

	mfs, err := h.multiFS(w, r, dig, desc, ref)
	if err != nil {
		return err
	}

	// Allow this to be cached for an hour.
	w.Header().Set("Cache-Control", "max-age=3600, immutable")

	httpserve.FileServer(httpserve.FS(mfs)).ServeHTTP(w, r)

	return nil
}

func (h *handler) renderIndex(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	idx := 0
	dig, idxs, err := h.getDigest(w, r)
	if err != nil {
		return fmt.Errorf("getDigest: %w", err)
	}
	prefix := fmt.Sprintf("/cache/%s/%s", idxs, dig.String())
	if parsed, err := strconv.ParseInt(idxs, 10, 64); err != nil {
		logs.Debug.Printf("ParseInt(%q)", idxs)
	} else {
		idx = int(parsed)
	}
	key := indexKey(dig.Identifier(), idx)
	size, err := h.indexCache.Size(ctx, key)
	if err != nil {
		return fmt.Errorf("indexCache.Size: %w", err)
	}
	rc, err := h.indexCache.Reader(ctx, key)
	if err != nil {
		return fmt.Errorf("indexCache.Reader: %w", err)
	}
	defer rc.Close()
	zr, err := gzip.NewReader(rc)
	if err != nil {
		return fmt.Errorf("gzip.NewReader: %w", err)
	}
	tr := tar.NewReader(zr)
	fs := h.newLayerFS(tr, size, prefix, dig.String(), "tar+gzip", types.MediaType("application/tar+gzip"))

	// Allow this to be cached for an hour.
	w.Header().Set("Cache-Control", "max-age=3600, immutable")

	httpserve.FileServer(fs).ServeHTTP(w, r)
	return nil
}

func (h *handler) jq(output *jsonOutputter, b []byte, r *http.Request, header *HeaderData) ([]byte, error) {
	jq, ok := r.URL.Query()["jq"]
	if !ok {
		header.JQ += " | jq ."
		return b, nil
	}

	var (
		err error
		exp string
	)

	exps := []string{header.JQ}

	for _, j := range jq {
		if debug {
			log.Printf("j = %s", j)
		}
		b, exp, err = evalBytes(j, b)
		if err != nil {
			return nil, err
		}
		exps = append(exps, exp)
	}

	header.JQ = strings.Join(exps, " | ")
	return b, nil
}

func (h *handler) getTags(repo name.Repository) ([]string, bool) {
	h.Lock()
	tags, ok := h.sawTags[repo.String()]
	h.Unlock()
	return tags, ok
}

func (h *handler) manifestHeader(ref name.Reference, desc v1.Descriptor) *HeaderData {
	header := headerData(ref, desc)
	header.JQ = crane("manifest") + " " + ref.String()
	header.Referrers = true

	// Handle clicking repo to list tags and such.
	if strings.Contains(ref.String(), "@") && strings.Index(ref.String(), "@") < strings.Index(ref.String(), ":") {
		chunks := strings.SplitN(ref.String(), "@", 2)
		header.Up = &RepoParent{
			Parent:    ref.Context().String(),
			Child:     chunks[1],
			Separator: "@",
		}
	} else if strings.Contains(ref.String(), ":") {
		chunks := strings.SplitN(ref.String(), ":", 2)
		header.Up = &RepoParent{
			Parent:    ref.Context().String(),
			Child:     chunks[1],
			Separator: ":",
		}
	} else {
		header.Up = &RepoParent{
			Parent: ref.String(),
		}
	}

	// Opportunistically show referrers based on cosign scheme if we
	// have a cached tags list response.
	prefix := strings.Replace(desc.Digest.String(), ":", "-", 1)
	tags, ok := h.getTags(ref.Context())
	if ok {
		for _, tag := range tags {
			if tag == prefix {
				// Referrers tag schema
				header.CosignTags = append(header.CosignTags, CosignTag{
					Tag:   tag,
					Short: "fallback",
				})
			} else if strings.HasPrefix(tag, prefix) {
				// Cosign tag schema
				chunks := strings.SplitN(tag, ".", 2)
				if len(chunks) == 2 && len(chunks[1]) != 0 {
					header.CosignTags = append(header.CosignTags, CosignTag{
						Tag:   tag,
						Short: chunks[1],
					})
				}
			}
		}
	}

	return header
}

func headerData(ref name.Reference, desc v1.Descriptor) *HeaderData {
	return &HeaderData{
		Repo:             ref.Context().String(),
		Reference:        ref.String(),
		CosignTags:       []CosignTag{},
		Descriptor:       &desc,
		Handler:          handlerForMT(string(desc.MediaType)),
		EscapedMediaType: url.QueryEscape(string(desc.MediaType)),
		MediaTypeLink:    getLink(string(desc.MediaType)),
	}
}

func renderHeader(w http.ResponseWriter, fname string, prefix string, ref name.Reference, kind string, mediaType types.MediaType, size int64, f httpserve.File, ctype string) error {
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := headerTmpl.Execute(w, TitleData{ref.String()}); err != nil {
		return err
	}

	filename := strings.TrimPrefix(fname, "/"+prefix)
	filename = strings.TrimPrefix(filename, "/")

	tarh, ok := stat.Sys().(*tar.Header)
	if ok {
		filename = tarh.Name
	} else {
		if !stat.IsDir() {
			logs.Debug.Printf("not a tar header or directory")
		}
	}

	tarflags := "tar -Ox "
	if kind == "tar+gzip" {
		tarflags = "tar -Oxz "
	} else if kind == "tar+zstd" {
		tarflags = "tar --zstd -Ox "
	}

	hash, err := v1.NewHash(ref.Identifier())
	if err != nil {
		return err
	}

	filelink := filename

	// Compute links for JQ
	fprefix := ""
	if strings.HasPrefix(filename, "./") {
		fprefix = "./"
	}
	filename = strings.TrimSuffix(filename, "/")
	dir := path.Dir(filename)
	if dir != "." {
		base := path.Base(filename)
		sep := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(filename, fprefix), dir), base)

		href := path.Join(prefix, dir)
		htext := fprefix + dir + sep

		logs.Debug.Printf("dir=%q, sep=%q, base=%q, href=%q, htext=%q", dir, sep, base, href, htext)
		dirlink := fmt.Sprintf(`<a class="mt" href="/%s">%s</a>`, href, htext)
		filelink = dirlink + base
	}

	desc := v1.Descriptor{
		Size:      size,
		Digest:    hash,
		MediaType: mediaType,
	}
	header := headerData(ref, desc)
	header.Up = &RepoParent{
		Parent:    ref.Context().String(),
		Separator: "@",
		Child:     ref.Identifier(),
	}
	header.JQ = crane("blob") + " " + ref.String() + " | " + tarflags + " " + filelink

	if stat.Size() > httpserve.TooBig {
		header.JQ += fmt.Sprintf(" | head -c %d", httpserve.TooBig)
	}
	if ctype == "application/octet-stream" {
		header.JQ += " | xxd"
	}

	if stat.IsDir() {
		tarflags = "tar -tv "
		if kind == "tar+gzip" {
			tarflags = "tar -tvz "
		} else if kind == "tar+zstd" {
			tarflags = "tar --zstd -tv "
		}

		header.JQ = crane("blob") + " " + ref.String() + " | " + tarflags + " " + filelink
	}
	header.SizeLink = fmt.Sprintf("/size/%s?mt=%s&size=%d", ref.Context().Digest(hash.String()).String(), mediaType, int64(size))

	return bodyTmpl.Execute(w, header)
}

func renderDir(w http.ResponseWriter, fname string, prefix string, mediaType types.MediaType, size int64, ref name.Reference, f httpserve.File, ctype string) error {
	// This must be a directory because it wasn't part of a filesystem
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	if !stat.IsDir() {
		return fmt.Errorf("file was not a directory")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := headerTmpl.Execute(w, TitleData{ref.String()}); err != nil {
		return err
	}

	filename := strings.TrimPrefix(fname, "/"+prefix)
	filename = strings.TrimPrefix(filename, "/")

	sys := stat.Sys()
	tarh, ok := sys.(*tar.Header)
	if ok {
		filename = tarh.Name
	} else {
		logs.Debug.Printf("sys: %T", sys)
	}

	tarflags := "tar -tv "

	hash, err := v1.NewHash(ref.Identifier())
	if err != nil {
		return err
	}

	desc := v1.Descriptor{
		Size:      size,
		Digest:    hash,
		MediaType: mediaType,
	}
	header := headerData(ref, desc)

	header.Up = &RepoParent{
		Parent:    ref.Context().String(),
		Separator: "@",
		Child:     ref.Identifier(),
	}

	// TODO: Make filename clickable to go up a directory.
	header.JQ = crane("export") + " " + ref.String() + " | " + tarflags + " " + filename

	header.SizeLink = fmt.Sprintf("/sizes/%s?mt=%s&size=%d", ref.Context().Digest(hash.String()).String(), mediaType, int64(size))

	return bodyTmpl.Execute(w, header)
}

// server.go
var htmlReplacer = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	// "&#34;" is shorter than "&quot;".
	`"`, "&#34;",
	// "&#39;" is shorter than "&apos;" and apos was not in HTML until HTML5.
	"'", "&#39;",
)

func htmlEscape(s string) string {
	return htmlReplacer.Replace(s)
}

type dumbEscaper struct {
	buf *bufio.Writer
}

var (
	amp = []byte("&amp;")
	lt  = []byte("&lt;")
	gt  = []byte("&gt;")
	dq  = []byte("&#34;")
	sq  = []byte("&#39;")
)

func (d *dumbEscaper) Write(p []byte) (n int, err error) {
	for i, b := range p {
		switch b {
		case '&':
			_, err = d.buf.Write(amp)
		case '<':
			_, err = d.buf.Write(lt)
		case '>':
			_, err = d.buf.Write(gt)
		case '"':
			_, err = d.buf.Write(dq)
		case '\'':
			_, err = d.buf.Write(sq)
		default:
			err = d.buf.WriteByte(b)
		}
		if err != nil {
			return i, err
		}
	}
	return len(p), d.buf.Flush()
}
