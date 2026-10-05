// Folio's read-only release server and local, atomic release publisher.
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
	"net/http"
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

const maxAPK = 100 * 1024 * 1024 // Matches the Android updater's limit.

var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-exp\.[1-9][0-9]{0,3})?$`)

func validVersion(version string, code int64) bool {
	if !versionPattern.MatchString(version) || code <= 0 || code > 2100000000 {
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

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: folio-release-server serve|publish [flags]")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	data := flags.String("data", "/var/lib/folio-releases", "release storage directory")
	base := flags.String("base-url", "https://folio.flandolf.me", "public HTTPS origin")
	listen := flags.String("listen", "127.0.0.1:8787", "HTTP address (behind Nginx)")
	apk := flags.String("apk", "", "signed APK to publish")
	version := flags.String("version-name", "", "APK versionName")
	code := flags.Int64("version-code", 0, "APK versionCode")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *base != "https://folio.flandolf.me" {
		return errors.New("base-url must be https://folio.flandolf.me (the Android updater trusts this host)")
	}
	*base = strings.TrimSuffix(*base, "/")
	switch args[0] {
	case "serve":
		return serve(*data, *listen)
	case "publish":
		if *apk == "" {
			return errors.New("publish requires -apk, -version-name and -version-code")
		}
		_, err := publish(*data, *base, *apk, *version, *code, "", time.Now().UTC())
		return err
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func readRelease(path string) (release, error) {
	var r release
	f, err := os.Open(path)
	if err != nil {
		return r, err
	}
	defer f.Close()
	err = json.NewDecoder(io.LimitReader(f, 1024*1024)).Decode(&r)
	if err != nil {
		return r, err
	}
	if !validVersion(r.VersionName, r.VersionCode) || r.Tag != "v"+r.VersionName || len(r.Assets) != 2 || r.Assets[0].Name != "folio-"+r.VersionName+".apk" || r.Assets[1].Name != "SHA256SUMS" {
		return r, errors.New("invalid release manifest")
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
func publish(data, base, apk, version string, code int64, expected string, published time.Time) (release, error) {
	var r release
	if !validVersion(version, code) {
		return r, errors.New("invalid version name/code")
	}
	if err := os.MkdirAll(data, 0755); err != nil {
		return r, err
	}
	lock, err := os.OpenFile(filepath.Join(data, ".publish.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return r, err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return r, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	stage, err := os.MkdirTemp(data, ".publish-*")
	if err != nil {
		return r, err
	}
	defer os.RemoveAll(stage)
	name := "folio-" + version + ".apk"
	dest := filepath.Join(stage, name)
	in, err := os.Open(apk)
	if err != nil {
		return r, err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return r, err
	}
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(out, hash), io.LimitReader(in, maxAPK+1))
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
	if size == 0 || size > maxAPK {
		return r, errors.New("APK must be between 1 byte and 100 MiB")
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if expected != "" && !strings.EqualFold(expected, digest) {
		return r, errors.New("APK checksum mismatch")
	}
	if err = validateAPK(dest); err != nil {
		return r, err
	}
	r = release{VersionCode: code, VersionName: version, Tag: "v" + version, Name: "Folio " + version, Published: published.UTC()}
	r.URL = base + "/releases/" + r.Tag + "/"
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
	latest, latestErr := readRelease(filepath.Join(data, "latest.json"))
	if latestErr != nil && !errors.Is(latestErr, os.ErrNotExist) {
		return r, latestErr
	}
	if latestErr == nil && latest.VersionCode == code && latest.Tag != r.Tag {
		return r, errors.New("version code already belongs to another release")
	}
	directory := filepath.Join(data, r.Tag)
	if existing, err := readRelease(filepath.Join(directory, "release.json")); err == nil {
		if existing.VersionCode != code || existing.Assets[0].Digest != r.Assets[0].Digest {
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
<title>Folio experimental builds</title><style>body{font:18px system-ui,sans-serif;max-width:720px;margin:64px auto;padding:0 24px;line-height:1.6;color:#192b29;background:#f7faf8}a{color:#23695c}article{border-top:1px solid #ccd8d1;padding:20px 0}small{color:#465c53}</style>
<h1>Folio experimental builds</h1><p>Experimental Folio builds for Android. These may be less stable. Download a signed APK, then open it on your device to install. Stable builds are on <a href="https://github.com/flandyw/folio/releases">GitHub Releases</a>.</p>
{{range .}}<article><h2>{{.Name}}</h2><p><a href="{{(index .Assets 0).URL}}">Download {{(index .Assets 0).Name}}</a> · <a href="{{(index .Assets 1).URL}}">SHA-256 checksum</a></p><small>Android 8 or newer · Version code {{.VersionCode}} · {{.Published.Format "2006-01-02"}}</small></article>{{else}}<p>No releases published yet.</p>{{end}}
<p><a href="https://github.com/flandyw/folio">Source code</a></p></html>`))

func handler(data string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-cache")
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
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
		latestPath := filepath.Join(data, "latest.json")
		switch req.URL.Path {
		case "/releases/latest.json":
			r, err := readRelease(latestPath)
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
			r, err := readRelease(latestPath)
			if err != nil {
				http.NotFound(w, req)
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
			entries, err := os.ReadDir(data)
			if err != nil {
				http.Error(w, "releases unavailable", http.StatusServiceUnavailable)
				return
			}
			var releases []release
			for _, e := range entries {
				if e.IsDir() && strings.HasPrefix(e.Name(), "v") {
					if r, err := readRelease(filepath.Join(data, e.Name(), "release.json")); err == nil {
						releases = append(releases, r)
					}
				}
			}
			sort.Slice(releases, func(i, j int) bool { return releases[i].VersionCode > releases[j].VersionCode })
			renderPage(w, req, releases)
			return
		}
		parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/releases/"), "/")
		if !strings.HasPrefix(req.URL.Path, "/releases/") || len(parts) != 2 || !strings.HasPrefix(parts[0], "v") || !versionPattern.MatchString(strings.TrimPrefix(parts[0], "v")) {
			http.NotFound(w, req)
			return
		}
		r, err := readRelease(filepath.Join(data, parts[0], "release.json"))
		if err != nil || r.Tag != parts[0] {
			http.NotFound(w, req)
			return
		}
		if parts[1] == "" {
			renderPage(w, req, []release{r})
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
		root, err := os.OpenRoot(data) // Prevent symlinks escaping the release directory.
		if err != nil {
			http.NotFound(w, req)
			return
		}
		defer root.Close()
		f, err := root.Open(filepath.Join(r.Tag, selected.Name))
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

func renderPage(w http.ResponseWriter, req *http.Request, releases []release) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	if req.Method == http.MethodGet {
		if err := pageTemplate.Execute(w, releases); err != nil {
			log.Printf("render: %v", err)
		}
	}
}

func serve(data, listen string) error {
	if _, err := os.ReadDir(data); err != nil {
		return err
	}
	server := &http.Server{Addr: listen, Handler: handler(data), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	// Downloads can take several minutes; Nginx bounds stalled downstream connections.
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
