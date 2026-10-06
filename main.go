// A read-only Android release server and local, atomic release publisher.
package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const defaultMaxAPK = 100 * 1024 * 1024 // Matches the Folio Android updater's limit.
const maxManifest = 1024 * 1024

var expectedDigestPattern = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// versionPattern is the only thing the URL router and the tag/directory names rely on:
// it must never admit a slash or a leading dot.
var versionPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$`)

var folioVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-exp\.[1-9][0-9]{0,3})?$`)

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

const (
	schemeAny   = "any"   // Any safe versionName with a positive versionCode.
	schemeFolio = "folio" // X.Y.Z[-exp.N] where the code is commitCount*10000+N.
)

// config describes one app's release channel. Everything app-specific lives here.
type config struct {
	Data      string // release storage directory
	Base      string // public HTTPS origin, no trailing slash
	Slug      string // file/tag prefix: <slug>-<version>.apk
	Name      string // display name
	Channel   string // channel label for the page title, e.g. "experimental"
	SourceURL string // optional "Source code" link
	StableURL string // optional link to stable releases
	Scheme    string // schemeAny or schemeFolio
	MaxAPK    int64
}

func (c config) validate() error {
	u, err := url.Parse(c.Base)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil || strings.HasSuffix(c.Base, "/") {
		return errors.New("base-url must be a bare HTTPS origin such as https://updates.example.com")
	}
	if !slugPattern.MatchString(c.Slug) {
		return errors.New("slug must be lowercase letters, digits and dashes (max 40)")
	}
	if c.Name == "" || len(c.Name) > 80 || strings.ContainsAny(c.Name, "\r\n") {
		return errors.New("name must be 1-80 characters on one line")
	}
	if c.Channel == "" || len(c.Channel) > 40 || strings.ContainsAny(c.Channel, "\r\n") {
		return errors.New("channel must be 1-40 characters on one line")
	}
	for _, link := range []string{c.SourceURL, c.StableURL} {
		if link == "" {
			continue
		}
		l, err := url.Parse(link)
		if err != nil || l.Scheme != "https" || l.Host == "" {
			return errors.New("source-url and stable-url must be HTTPS URLs")
		}
	}
	if c.Scheme != schemeAny && c.Scheme != schemeFolio {
		return fmt.Errorf("version-scheme must be %q or %q", schemeAny, schemeFolio)
	}
	if c.MaxAPK <= 0 || c.MaxAPK > 2<<30 {
		return errors.New("max-apk-mb must be between 1 and 2048")
	}
	return nil
}

func (c config) apkName(version string) string { return c.Slug + "-" + version + ".apk" }

func (c config) validVersion(version string, code int64) bool {
	if !versionPattern.MatchString(version) || code <= 0 || code > 2100000000 {
		return false
	}
	if c.Scheme != schemeFolio {
		return true
	}
	if !folioVersionPattern.MatchString(version) {
		return false
	}
	base, revision, experimental := strings.Cut(version, "-exp.")
	if !experimental {
		return true
	} // Keep previously published legacy builds readable.
	parts := strings.Split(base, ".")
	numbers := make([]int64, 3)
	for i, part := range parts {
		n, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return false
		}
		numbers[i] = n
	}
	if numbers[0] > 2100 || numbers[1] > 9 || numbers[2] > 9 ||
		fmt.Sprintf("%d.%d.%d", numbers[0], numbers[1], numbers[2]) != base {
		return false
	}
	r, err := strconv.ParseInt(revision, 10, 64)
	if err != nil {
		return false
	}
	count := numbers[0]*100 + numbers[1]*10 + numbers[2]
	return count > 0 && code == count*10000+r
}

type asset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Size   int64  `json:"size"`
	Digest string `json:"digest,omitempty"`
}

// GitHub-compatible fields let the app retain its existing checksum/download parser.
// Explicit version_code also avoids ambiguity in historical v0.2.N tags.
type release struct {
	VersionCode int64     `json:"version_code"`
	VersionName string    `json:"version_name"`
	Tag         string    `json:"tag_name"`
	Name        string    `json:"name"`
	URL         string    `json:"html_url"`
	Published   time.Time `json:"published_at"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	Assets      []asset   `json:"assets"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// Every flag defaults from a RELEASE_* environment variable, so a systemd
// EnvironmentFile (or the install config) can configure the service without
// repeating itself on the command line.
func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: release-server serve|publish [flags]")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	cfg := config{}
	flags.StringVar(&cfg.Data, "data", env("RELEASE_DATA", "/var/lib/release-server"), "release storage directory [RELEASE_DATA]")
	flags.StringVar(&cfg.Base, "base-url", env("RELEASE_BASE_URL", ""), "public HTTPS origin [RELEASE_BASE_URL]")
	flags.StringVar(&cfg.Slug, "slug", env("RELEASE_SLUG", ""), "APK/tag prefix, e.g. myapp -> myapp-1.2.3.apk [RELEASE_SLUG]")
	flags.StringVar(&cfg.Name, "name", env("RELEASE_NAME", ""), "display name (default: slug) [RELEASE_NAME]")
	flags.StringVar(&cfg.Channel, "channel", env("RELEASE_CHANNEL", "experimental"), "channel label on the download page [RELEASE_CHANNEL]")
	flags.StringVar(&cfg.SourceURL, "source-url", env("RELEASE_SOURCE_URL", ""), "optional source code link [RELEASE_SOURCE_URL]")
	flags.StringVar(&cfg.StableURL, "stable-url", env("RELEASE_STABLE_URL", ""), "optional stable releases link [RELEASE_STABLE_URL]")
	flags.StringVar(&cfg.Scheme, "version-scheme", env("RELEASE_VERSION_SCHEME", schemeAny), "any | folio (X.Y.Z[-exp.N], code=count*10000+N) [RELEASE_VERSION_SCHEME]")
	maxMB := flags.Int64("max-apk-mb", func() int64 { n, _ := strconv.ParseInt(env("RELEASE_MAX_APK_MB", "100"), 10, 64); return n }(), "largest accepted APK in MiB [RELEASE_MAX_APK_MB]")
	listen := flags.String("listen", env("RELEASE_LISTEN", "127.0.0.1:8787"), "HTTP address (behind Nginx) [RELEASE_LISTEN]")
	apk := flags.String("apk", "", "signed APK to publish")
	version := flags.String("version-name", "", "APK versionName")
	expected := flags.String("expected-sha256", "", "SHA-256 of the verified APK")
	code := flags.Int64("version-code", 0, "APK versionCode")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if cfg.Name == "" {
		cfg.Name = cfg.Slug
	}
	cfg.MaxAPK = *maxMB << 20
	if err := cfg.validate(); err != nil {
		return err
	}
	switch args[0] {
	case "serve":
		return serve(cfg, *listen)
	case "publish":
		if *apk == "" {
			return errors.New("publish requires -apk, -version-name and -version-code")
		}
		_, err := publish(cfg, *apk, *version, *code, *expected, time.Now().UTC())
		return err
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func (c config) readRelease(path string) (release, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return release{}, err
	}
	defer root.Close()
	return c.readReleaseAt(root, filepath.Base(path))
}

// Linux is the supported deployment platform. O_NONBLOCK prevents a planted FIFO
// from hanging a request before its type can be checked on the opened descriptor.
func openRegular(root *os.Root, path string) (*os.File, error) {
	f, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("not a regular file")
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func (c config) readReleaseAt(root *os.Root, path string) (release, error) {
	var r release
	f, err := openRegular(root, path)
	if err != nil {
		return r, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return r, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxManifest {
		return r, errors.New("invalid release manifest size/type")
	}
	decoder := json.NewDecoder(io.LimitReader(f, maxManifest+1))
	if err := decoder.Decode(&r); err != nil {
		return r, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return r, errors.New("trailing release manifest data")
	}
	if !c.validVersion(r.VersionName, r.VersionCode) || r.Tag != "v"+r.VersionName ||
		len(r.Assets) != 2 || r.Assets[0].Name != c.apkName(r.VersionName) ||
		r.Assets[1].Name != "SHA256SUMS" || r.URL != c.Base+"/releases/"+r.Tag+"/" ||
		r.Published.IsZero() || r.Draft || r.Prerelease {
		return r, errors.New("invalid release manifest")
	}
	for i, a := range r.Assets {
		if a.URL != r.URL+a.Name || !digestPattern.MatchString(a.Digest) || a.Size <= 0 ||
			(i == 0 && a.Size > c.MaxAPK) || (i == 1 && a.Size != int64(64+2+len(r.Assets[0].Name)+1)) {
			return r, errors.New("invalid release asset")
		}
	}
	return r, nil
}

// Fsync the file and containing directory, then expose complete metadata with rename.
func writeJSON(path string, value any) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".json-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0644); err == nil {
		err = json.NewEncoder(f).Encode(value)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func validateAPK(path string) error {
	z, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("invalid APK: %w", err)
	}
	defer z.Close()
	for _, f := range z.File {
		if f.Name == "AndroidManifest.xml" {
			return nil
		}
	}
	return errors.New("APK has no AndroidManifest.xml")
}

// Only the publisher writes. The HTTP process has a read-only filesystem sandbox.
// flock serializes publishers, directories are immutable, and latest never downgrades.
func publish(c config, apk, version string, code int64, expected string, published time.Time) (release, error) {
	var r release
	data := c.Data
	if err := c.validate(); err != nil {
		return r, err
	}
	if expected != "" && !expectedDigestPattern.MatchString(expected) {
		return r, errors.New("invalid expected SHA-256")
	}
	if !c.validVersion(version, code) {
		return r, errors.New("invalid version name/code")
	}
	if err := os.MkdirAll(data, 0755); err != nil {
		return r, err
	}
	root, err := os.OpenRoot(data)
	if err != nil {
		return r, err
	}
	defer root.Close()
	lock, err := root.OpenFile(".publish.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return r, err
	}
	defer lock.Close()
	lockInfo, err := lock.Stat()
	if err != nil {
		return r, err
	}
	if !lockInfo.Mode().IsRegular() {
		return r, errors.New("invalid publish lock")
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return r, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	stage, err := os.MkdirTemp(data, ".publish-*")
	if err != nil {
		return r, err
	}
	defer os.RemoveAll(stage)
	name := c.apkName(version)
	dest := filepath.Join(stage, name)
	in, err := os.OpenFile(apk, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return r, err
	}
	defer in.Close()
	inputInfo, err := in.Stat()
	if err != nil {
		return r, err
	}
	if !inputInfo.Mode().IsRegular() || inputInfo.Size() <= 0 || inputInfo.Size() > c.MaxAPK {
		return r, errors.New("APK must be a regular file between 1 byte and the size limit")
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return r, err
	}
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(out, hash), io.LimitReader(in, c.MaxAPK+1))
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err != nil {
		return r, err
	}
	if closeErr != nil {
		return r, closeErr
	}
	if size == 0 || size > c.MaxAPK {
		return r, errors.New("APK must be between 1 byte and the size limit")
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if expected != "" && !strings.EqualFold(expected, digest) {
		return r, errors.New("APK checksum mismatch")
	}
	if err = validateAPK(dest); err != nil {
		return r, err
	}
	r = release{VersionCode: code, VersionName: version, Tag: "v" + version, Name: c.Name + " " + version, Published: published.UTC()}
	r.URL = c.Base + "/releases/" + r.Tag + "/"
	sums := []byte(digest + "  " + name + "\n")
	if err = os.WriteFile(filepath.Join(stage, "SHA256SUMS"), sums, 0644); err != nil {
		return r, err
	}
	sumFile, err := os.OpenFile(filepath.Join(stage, "SHA256SUMS"), os.O_RDWR, 0)
	if err != nil {
		return r, err
	}
	err = sumFile.Sync()
	sumFile.Close()
	if err != nil {
		return r, err
	}
	sumHash := sha256.Sum256(sums)
	r.Assets = []asset{
		{Name: name, URL: r.URL + name, Size: size, Digest: "sha256:" + digest},
		{Name: "SHA256SUMS", URL: r.URL + "SHA256SUMS", Size: int64(len(sums)), Digest: "sha256:" + hex.EncodeToString(sumHash[:])},
	}
	if err = writeJSON(filepath.Join(stage, "release.json"), r); err != nil {
		return r, err
	}
	latest, latestErr := c.readReleaseAt(root, "latest.json")
	if latestErr != nil && !errors.Is(latestErr, os.ErrNotExist) {
		return r, latestErr
	}
	if latestErr == nil && latest.VersionCode == code && latest.Tag != r.Tag {
		return r, errors.New("version code already belongs to another release")
	}
	directory := filepath.Join(data, r.Tag)
	if existing, err := c.readReleaseAt(root, filepath.Join(r.Tag, "release.json")); err == nil {
		if existing.Tag != r.Tag || existing.VersionCode != code || existing.Assets[0].Digest != r.Assets[0].Digest {
			return r, errors.New("release already exists with different content; publish a new version")
		}
		r = existing
	} else {
		if !errors.Is(err, os.ErrNotExist) {
			return r, err
		}
		if err = os.Chmod(stage, 0755); err != nil {
			return r, err
		}
		if err = os.Rename(stage, directory); err != nil {
			return r, err
		}
		if err = syncDir(data); err != nil {
			return r, err
		}
	}
	if latestErr != nil || code >= latest.VersionCode {
		if err = writeJSON(filepath.Join(data, "latest.json"), r); err != nil {
			return r, err
		}
	}
	log.Printf("published %s (version code %d, SHA-256 %s)", r.Tag, code, digest)
	return r, nil
}

var pageTemplate = template.Must(template.New("releases").Parse(`<!doctype html>
<html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Config.Name}} {{.Config.Channel}} builds</title><style>body{font:18px system-ui,sans-serif;max-width:720px;margin:64px auto;padding:0 24px;line-height:1.6;color:#192b29;background:#f7faf8}a{color:#23695c}article{border-top:1px solid #ccd8d1;padding:20px 0}small{color:#465c53}</style>
<h1>{{.Config.Name}} {{.Config.Channel}} builds</h1><p>{{.Config.Channel}} {{.Config.Name}} builds for Android. These may be less stable. Download a signed APK, then open it on your device to install.{{if .Config.StableURL}} Stable builds are on <a href="{{.Config.StableURL}}">the releases page</a>.{{end}}</p>
{{range .Releases}}<article><h2>{{.Name}}</h2><p><a href="{{(index .Assets 0).URL}}">Download {{(index .Assets 0).Name}}</a> · <a href="{{(index .Assets 1).URL}}">SHA-256 checksum</a></p><small>Android 8 or newer · Version code {{.VersionCode}} · {{.Published.Format "2006-01-02"}}</small></article>{{else}}<p>No releases published yet.</p>{{end}}
{{if .Config.SourceURL}}<p><a href="{{.Config.SourceURL}}">Source code</a></p>{{end}}</html>`))

func handler(cfg config) http.Handler {
	data := cfg.Data
	active := make(chan struct{}, 128)
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
		select {
		case active <- struct{}{}:
			defer func() { <-active }()
		default:
			w.Header().Set("Retry-After", "5")
			http.Error(w, "server busy", http.StatusServiceUnavailable)
			return
		}
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if req.ContentLength != 0 || len(req.TransferEncoding) != 0 {
			w.Header().Set("Connection", "close")
			http.Error(w, "request body not allowed", http.StatusBadRequest)
			return
		}
		// Only a single range is needed by the updater; multipart ranges amplify work.
		ranges := req.Header.Values("Range")
		if len(ranges) > 1 || (len(ranges) == 1 && strings.Contains(ranges[0], ",")) {
			http.Error(w, "multiple ranges not supported", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if req.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			if req.Method == http.MethodGet {
				fmt.Fprintln(w, "ok")
			}
			return
		}
		if req.URL.Path == "/" || req.URL.Path == "/releases" {
			http.Redirect(w, req, "/releases/", http.StatusFound)
			return
		}
		root, err := os.OpenRoot(data)
		if err != nil {
			http.Error(w, "releases unavailable", http.StatusServiceUnavailable)
			return
		}
		defer root.Close()
		latestPath := "latest.json"
		switch req.URL.Path {
		case "/releases/latest.json":
			r, err := cfg.readReleaseAt(root, latestPath)
			if errors.Is(err, os.ErrNotExist) {
				http.NotFound(w, req)
				return
			}
			if err != nil {
				http.Error(w, "release unavailable", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if req.Method == http.MethodGet {
				json.NewEncoder(w).Encode(r)
			}
			return
		case "/releases/latest", "/releases/latest.apk":
			r, err := cfg.readReleaseAt(root, latestPath)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					http.NotFound(w, req)
				} else {
					http.Error(w, "release unavailable", http.StatusServiceUnavailable)
				}
				return
			}
			target := "/releases/" + r.Tag + "/"
			if strings.HasSuffix(req.URL.Path, ".apk") {
				target += r.Assets[0].Name
			}
			http.Redirect(w, req, target, http.StatusFound)
			return
		}
		if req.URL.Path == "/releases/" {
			dir, err := root.Open(".")
			if err != nil {
				http.Error(w, "releases unavailable", http.StatusServiceUnavailable)
				return
			}
			entries, err := dir.ReadDir(-1)
			dir.Close()
			if err != nil {
				http.Error(w, "releases unavailable", http.StatusServiceUnavailable)
				return
			}
			var releases []release
			for _, e := range entries {
				if e.IsDir() && strings.HasPrefix(e.Name(), "v") {
					if r, err := cfg.readReleaseAt(root, filepath.Join(e.Name(), "release.json")); err == nil && r.Tag == e.Name() {
						releases = append(releases, r)
					}
				}
			}
			sort.Slice(releases, func(i, j int) bool { return releases[i].VersionCode > releases[j].VersionCode })
			renderPage(w, req, cfg, releases)
			return
		}
		parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/releases/"), "/")
		if !strings.HasPrefix(req.URL.Path, "/releases/") || len(parts) != 2 || !strings.HasPrefix(parts[0], "v") || !versionPattern.MatchString(strings.TrimPrefix(parts[0], "v")) {
			http.NotFound(w, req)
			return
		}
		r, err := cfg.readReleaseAt(root, filepath.Join(parts[0], "release.json"))
		if err != nil || r.Tag != parts[0] {
			http.NotFound(w, req)
			return
		}
		if parts[1] == "" {
			renderPage(w, req, cfg, []release{r})
			return
		}
		if parts[1] == "release.json" {
			w.Header().Set("Content-Type", "application/json")
			if req.Method == http.MethodGet {
				json.NewEncoder(w).Encode(r)
			}
			return
		}
		var selected *asset
		for i := range r.Assets {
			if r.Assets[i].Name == parts[1] {
				selected = &r.Assets[i]
			}
		}
		if selected == nil {
			http.NotFound(w, req)
			return
		}
		f, err := openRegular(root, filepath.Join(r.Tag, selected.Name))
		if err != nil {
			http.NotFound(w, req)
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() != selected.Size {
			http.Error(w, "asset unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("ETag", `"`+selected.Digest+`"`)
		if strings.HasSuffix(selected.Name, ".apk") {
			w.Header().Set("Content-Type", "application/vnd.android.package-archive")
			w.Header().Set("Content-Disposition", `attachment; filename="`+selected.Name+`"`)
		} else {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		}
		http.ServeContent(w, req, selected.Name, info.ModTime(), f)
	})
}

func renderPage(w http.ResponseWriter, req *http.Request, cfg config, releases []release) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	if req.Method == http.MethodGet {
		if err := pageTemplate.Execute(w, struct {
			Config   config
			Releases []release
		}{cfg, releases}); err != nil {
			log.Printf("render: %v", err)
		}
	}
}

func validateListen(listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("listen must use a loopback IP behind Nginx")
	}
	return nil
}

func serve(cfg config, listen string) error {
	data := cfg.Data
	if err := validateListen(listen); err != nil {
		return err
	}
	if _, err := os.ReadDir(data); err != nil {
		return err
	}
	server := &http.Server{Addr: listen, Handler: handler(cfg), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, WriteTimeout: 10 * time.Minute, MaxHeaderBytes: 16 * 1024}
	// Bound total download time as well as Nginx's stalled downstream timeout.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			log.Printf("shutdown: %v", err)
		}
		close(done)
	}()
	log.Printf("serving %s on %s", data, listen)
	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		<-done
		return nil
	}
	return err
}
