package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func folioConfig(data string) config {
	return config{Data: data, Base: "https://folio.flandolf.me", Slug: "folio", Name: "Folio", Channel: "experimental",
		SourceURL: "https://github.com/flandyw/folio", StableURL: "https://github.com/flandyw/folio/releases",
		Scheme: schemeFolio, MaxAPK: defaultMaxAPK}
}

func testAPK(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.apk")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(f)
	w, err := z.Create("AndroidManifest.xml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, text); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func addRelease(t *testing.T, data, apk, version string, code int64) release {
	t.Helper()
	r, err := publish(folioConfig(data), apk, version, code, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPublishOrderingAndImmutability(t *testing.T) {
	data, apk := t.TempDir(), testAPK(t, "one")
	newest := addRelease(t, data, apk, "2.1.8", 218)
	addRelease(t, data, apk, "2.1.7", 217)
	addRelease(t, data, apk, "2.1.8", 218) // Idempotent retry.
	latest, err := folioConfig("").readRelease(filepath.Join(data, "latest.json"))
	if err != nil || latest.Tag != newest.Tag {
		t.Fatalf("latest downgraded: %+v, %v", latest, err)
	}
	if _, err := publish(folioConfig(data), testAPK(t, "changed"), "2.1.8", 218, "", time.Now()); err == nil {
		t.Fatal("overwrote an immutable release")
	}
	if _, err := publish(folioConfig(data), apk, "../../escape", 219, "", time.Now()); err == nil {
		t.Fatal("accepted traversal version")
	}
	if _, err := publish(folioConfig(data), apk, "2.1.9", 219, strings.Repeat("0", 64), time.Now()); err == nil {
		t.Fatal("accepted bad checksum")
	}
	latest, err = folioConfig("").readRelease(filepath.Join(data, "latest.json"))
	if err != nil || latest.Tag != newest.Tag {
		t.Fatal("failed publish changed latest")
	}
}

func TestDownloadsAndAPI(t *testing.T) {
	data, apk := t.TempDir(), testAPK(t, "download me")
	r := addRelease(t, data, apk, "2.1.8", 218)
	server := httptest.NewServer(handler(folioConfig(data)))
	defer server.Close()
	resp, err := http.Get(server.URL + "/releases/latest.json")
	if err != nil {
		t.Fatal(err)
	}
	var got release
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got.VersionCode != 218 || got.Assets[0].URL != r.Assets[0].URL {
		t.Fatalf("bad updater manifest: %+v", got)
	}
	path := "/releases/" + r.Tag + "/" + r.Assets[0].Name
	resp, err = http.Get(server.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	bytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	sum := sha256.Sum256(bytes)
	if "sha256:"+hex.EncodeToString(sum[:]) != r.Assets[0].Digest {
		t.Fatal("download checksum mismatch")
	}
	if resp.Header.Get("Content-Type") != "application/vnd.android.package-archive" {
		t.Fatal("bad APK MIME type")
	}
	req, _ := http.NewRequest("GET", server.URL+path, nil)
	req.Header.Set("Range", "bytes=0-9")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	bytes, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || len(bytes) != 10 {
		t.Fatal("range download failed")
	}
	req, _ = http.NewRequest("GET", server.URL+path, nil)
	req.Header.Set("If-None-Match", `"`+r.Assets[0].Digest+`"`)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 304 {
		t.Fatal("conditional request failed")
	}
	resp, err = http.Head(server.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.ContentLength != r.Assets[0].Size {
		t.Fatal("HEAD size differs")
	}
	for _, path := range []string{"/releases/latest.json", "/releases/", path, "/publish"} {
		req, _ := http.NewRequest("POST", server.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 405 {
			t.Fatalf("write allowed on %s", path)
		}
	}
	for _, path := range []string{"/releases/../latest.json", "/releases/%2e%2e/latest.json", "/releases/v2.1.8/.publish.lock", "/releases/v2.1.8/folio-2.1.8.apk/extra"} {
		resp, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Fatalf("unexpected file exposed by %s", path)
		}
	}
}

func TestConcurrentPublishDoesNotDowngrade(t *testing.T) {
	data, apk := t.TempDir(), testAPK(t, "concurrent")
	var wg sync.WaitGroup
	for _, item := range []struct {
		version string
		code    int64
	}{{"2.1.7", 217}, {"2.1.8", 218}, {"2.1.9", 219}} {
		wg.Add(1)
		go func(version string, code int64) {
			defer wg.Done()
			if _, err := publish(folioConfig(data), apk, version, code, "", time.Now()); err != nil {
				t.Error(err)
			}
		}(item.version, item.code)
	}
	wg.Wait()
	r, err := folioConfig("").readRelease(filepath.Join(data, "latest.json"))
	if err != nil || r.VersionCode != 219 {
		t.Fatalf("bad concurrent latest: %+v %v", r, err)
	}
}

func TestEmptyAndCorruptCatalog(t *testing.T) {
	data := t.TempDir()
	w := httptest.NewRecorder()
	handler(folioConfig(data)).ServeHTTP(w, httptest.NewRequest("GET", "/releases/latest.json", nil))
	if w.Code != 404 {
		t.Fatal("empty server should have no update")
	}
	if err := os.WriteFile(filepath.Join(data, "latest.json"), []byte("broken"), 0644); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	handler(folioConfig(data)).ServeHTTP(w, httptest.NewRequest("GET", "/releases/latest.json", nil))
	if w.Code != 503 {
		t.Fatal("corrupt catalog must not mean up to date")
	}
	if _, err := publish(folioConfig(data), testAPK(t, "one"), "2.1.8", 218, "", time.Now()); err == nil {
		t.Fatal("silently replaced corrupt catalog")
	}
}

func TestExperimentalRevisionsAndLegacyHistory(t *testing.T) {
	data, apk := t.TempDir(), testAPK(t, "revision")
	addRelease(t, data, apk, "2.1.7", 217)
	first := addRelease(t, data, apk, "2.1.7-exp.1", 2170001)
	second := addRelease(t, data, apk, "2.1.7-exp.2", 2170002)
	addRelease(t, data, apk, "2.1.7-exp.1", 2170001)
	latest, err := folioConfig("").readRelease(filepath.Join(data, "latest.json"))
	if err != nil || latest.VersionCode != second.VersionCode {
		t.Fatal("revision pointer downgraded")
	}
	for _, r := range []release{first, second} {
		w := httptest.NewRecorder()
		handler(folioConfig(data)).ServeHTTP(w, httptest.NewRequest("GET", "/releases/"+r.Tag+"/"+r.Assets[0].Name, nil))
		if w.Code != 200 {
			t.Fatalf("experimental APK not served: %s", r.Tag)
		}
	}
	for _, bad := range []struct {
		version string
		code    int64
	}{
		{"2.1.7-exp.0", 2170000}, {"2.1.7-exp.10000", 2180000},
		{"2.1.7-exp.3", 2170002}, {"02.1.7-exp.3", 2170003},
		{"2100.0.0-exp.1", 2100000001}, {"2.1.7-exp.1/escape", 2170001},
	} {
		if _, err := publish(folioConfig(data), apk, bad.version, bad.code, "", time.Now()); err == nil {
			t.Fatalf("accepted invalid revision: %+v", bad)
		}
	}
	stable := addRelease(t, data, apk, "2.1.8", 2180000)
	if stable.VersionCode <= second.VersionCode {
		t.Fatal("next stable did not supersede experimental")
	}
}

func TestRejectUntrustedManifest(t *testing.T) {
	data := t.TempDir()
	r := addRelease(t, data, testAPK(t, "manifest"), "2.1.8", 218)
	mutations := map[string]func(*release){
		"external page":      func(r *release) { r.URL = "https://evil.example/" },
		"external download":  func(r *release) { r.Assets[0].URL = "https://evil.example/build.apk" },
		"external checksum":  func(r *release) { r.Assets[1].URL = "https://evil.example/SHA256SUMS" },
		"header injection":   func(r *release) { r.Assets[0].Digest = "sha256:bad\r\nInjected: yes" },
		"missing digest":     func(r *release) { r.Assets[0].Digest = "" },
		"empty APK":          func(r *release) { r.Assets[0].Size = 0 },
		"oversized APK":      func(r *release) { r.Assets[0].Size = defaultMaxAPK + 1 },
		"oversized checksum": func(r *release) { r.Assets[1].Size = defaultMaxAPK },
		"draft":              func(r *release) { r.Draft = true },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			bad := r
			bad.Assets = append([]asset(nil), r.Assets...)
			mutate(&bad)
			if err := writeJSON(filepath.Join(data, "latest.json"), bad); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"/releases/latest.json", "/releases/latest", "/releases/latest.apk"} {
				w := httptest.NewRecorder()
				handler(folioConfig(data)).ServeHTTP(w, httptest.NewRequest("GET", path, nil))
				if w.Code != 503 {
					t.Fatalf("%s: status %d", path, w.Code)
				}
			}
		})
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{
		"trailing JSON":      append(append([]byte(nil), raw...), []byte("{}")...),
		"trailing junk":      append(append([]byte(nil), raw...), []byte("broken")...),
		"oversized manifest": append(append([]byte(nil), raw...), []byte(strings.Repeat(" ", maxManifest))...),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(data, "latest.json"), body, 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := folioConfig("").readRelease(filepath.Join(data, "latest.json")); err == nil {
				t.Fatal("accepted corrupt manifest")
			}
		})
	}
}

func TestConfinedReleaseReads(t *testing.T) {
	for _, target := range []string{"latest.json", "manifest", "directory", "asset", "fifo manifest", "fifo asset"} {
		t.Run(target, func(t *testing.T) {
			data, outside := t.TempDir(), t.TempDir()
			r := addRelease(t, data, testAPK(t, "confined"), "2.1.8", 218)
			externalManifest := filepath.Join(outside, "release.json")
			if err := writeJSON(externalManifest, r); err != nil {
				t.Fatal(err)
			}
			path, replacement := filepath.Join(data, "latest.json"), externalManifest
			request := "/releases/latest.json"
			if target != "latest.json" {
				path = filepath.Join(data, r.Tag, "release.json")
				request = "/releases/" + r.Tag + "/release.json"
			}
			if target == "directory" {
				path, replacement = filepath.Join(data, r.Tag), outside
				if err := os.RemoveAll(path); err != nil {
					t.Fatal(err)
				}
			} else {
				if target == "asset" || target == "fifo asset" {
					path = filepath.Join(data, r.Tag, r.Assets[0].Name)
					request = "/releases/" + r.Tag + "/" + r.Assets[0].Name
					replacement = testAPK(t, "confined")
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasPrefix(target, "fifo") {
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Symlink(replacement, path); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			handler(folioConfig(data)).ServeHTTP(w, httptest.NewRequest("GET", request, nil))
			if w.Code != 404 && w.Code != 503 {
				t.Fatalf("unsafe file served: status %d", w.Code)
			}
			if target == "manifest" || target == "directory" {
				w = httptest.NewRecorder()
				handler(folioConfig(data)).ServeHTTP(w, httptest.NewRequest("GET", "/releases/", nil))
				if strings.Contains(w.Body.String(), r.Assets[0].Name) {
					t.Fatal("unsafe manifest included in catalog")
				}
			}
		})
	}
}

func TestRequestLimitsAndSecurityHeaders(t *testing.T) {
	data := t.TempDir()
	r := addRelease(t, data, testAPK(t, "ranges"), "2.1.8", 218)
	h := handler(folioConfig(data))
	path := "/releases/" + r.Tag + "/" + r.Assets[0].Name
	for _, ranges := range [][]string{{"bytes=0-1,2-3"}, {"bytes=0-1", "bytes=2-3"}} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header["Range"] = ranges
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 416 {
			t.Fatalf("accepted multiple ranges: %d", w.Code)
		}
	}
	for _, chunked := range []bool{false, true} {
		req := httptest.NewRequest("GET", path, strings.NewReader("body"))
		if chunked {
			req.ContentLength = -1
			req.TransferEncoding = []string{"chunked"}
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 400 || w.Header().Get("Connection") != "close" {
			t.Fatal("accepted request body")
		}
	}
	for _, path := range []string{"/healthz", "/releases/", "/missing", path} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		for key, expected := range map[string]string{"X-Frame-Options": "DENY", "X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer"} {
			if w.Header().Get(key) != expected {
				t.Fatalf("%s missing %s", path, key)
			}
		}
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), "form-action 'none'") {
			t.Fatal("missing CSP")
		}
	}
}

func TestLoopbackOnly(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8787", "[::1]:8787"} {
		if err := validateListen(addr); err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
	}
	for _, addr := range []string{":8787", "0.0.0.0:8787", "[::]:8787", "192.0.2.1:8787", "localhost:8787", "invalid"} {
		if err := serve(folioConfig(t.TempDir()), addr); err == nil {
			t.Fatalf("accepted public/ambiguous listener %s", addr)
		}
	}
}

func TestPublishVerifiedChecksum(t *testing.T) {
	data, apk := t.TempDir(), testAPK(t, "verified")
	bytes, err := os.ReadFile(apk)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(bytes)
	if err := run([]string{"publish", "-data", data, "-base-url", "https://folio.flandolf.me", "-slug", "folio", "-version-scheme", "folio", "-apk", apk, "-version-name", "2.1.8", "-version-code", "218", "-expected-sha256", hex.EncodeToString(digest[:])}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"publish", "-data", data, "-base-url", "https://folio.flandolf.me", "-slug", "folio", "-version-scheme", "folio", "-apk", testAPK(t, "replaced"), "-version-name", "2.1.9", "-version-code", "219", "-expected-sha256", hex.EncodeToString(digest[:])}); err == nil {
		t.Fatal("published replaced APK")
	}
	latest, err := folioConfig("").readRelease(filepath.Join(data, "latest.json"))
	if err != nil || latest.VersionCode != 218 {
		t.Fatal("failed checksum changed latest")
	}
	if _, err := os.Stat(filepath.Join(data, "v2.1.9")); !os.IsNotExist(err) {
		t.Fatal("failed publish left a release")
	}
}

func TestPublishRejectsUnsafeLock(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			data := t.TempDir()
			path := filepath.Join(data, ".publish.lock")
			if kind == "symlink" {
				if err := os.Symlink(filepath.Join(t.TempDir(), "lock"), path); err != nil {
					t.Fatal(err)
				}
			} else if err := syscall.Mkfifo(path, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := publish(folioConfig(data), testAPK(t, "locked"), "2.1.8", 218, "", time.Now()); err == nil {
				t.Fatal("accepted unsafe publish lock")
			}
		})
	}
}

func TestOtherAppConfig(t *testing.T) {
	data := t.TempDir()
	cfg := config{Data: data, Base: "https://updates.example.org", Slug: "other-app", Name: "Other App", Channel: "beta", Scheme: schemeAny, MaxAPK: defaultMaxAPK}
	apk := testAPK(t, "other")
	r, err := publish(cfg, apk, "1.4.0-beta2", 14002, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if r.Assets[0].Name != "other-app-1.4.0-beta2.apk" || r.URL != "https://updates.example.org/releases/v1.4.0-beta2/" || r.Name != "Other App 1.4.0-beta2" {
		t.Fatalf("unexpected release %+v", r)
	}
	// A release from a differently configured channel is not trusted.
	if _, err := folioConfig(data).readRelease(filepath.Join(data, "latest.json")); err == nil {
		t.Fatal("read another app's manifest")
	}
	for _, bad := range []string{"../x", ".hidden", "a/b", "", "a b"} {
		if _, err := publish(cfg, apk, bad, 15000, "", time.Now()); err == nil {
			t.Fatalf("accepted version %q", bad)
		}
	}
	w := httptest.NewRecorder()
	handler(cfg).ServeHTTP(w, httptest.NewRequest("GET", "/releases/", nil))
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "Other App beta builds") || strings.Contains(body, "Source code") {
		t.Fatalf("page: %d %s", w.Code, body)
	}
	w = httptest.NewRecorder()
	handler(cfg).ServeHTTP(w, httptest.NewRequest("GET", "/releases/"+r.Tag+"/"+r.Assets[0].Name, nil))
	if w.Code != 200 {
		t.Fatalf("download: %d", w.Code)
	}
}

func TestConfigValidation(t *testing.T) {
	good := config{Data: "/tmp/x", Base: "https://updates.example.org", Slug: "app", Name: "App", Channel: "beta", Scheme: schemeAny, MaxAPK: defaultMaxAPK}
	if err := good.validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*config){
		"http":           func(c *config) { c.Base = "http://updates.example.org" },
		"path":           func(c *config) { c.Base = "https://updates.example.org/x" },
		"trailing slash": func(c *config) { c.Base = "https://updates.example.org/" },
		"slug":           func(c *config) { c.Slug = "../app" },
		"scheme":         func(c *config) { c.Scheme = "weird" },
		"link":           func(c *config) { c.SourceURL = "javascript:alert(1)" },
		"size":           func(c *config) { c.MaxAPK = 0 },
	} {
		c := good
		mutate(&c)
		if c.validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
