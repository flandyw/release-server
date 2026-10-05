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
	"testing"
	"time"
)

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
	r, err := publish(data, "https://folio.flandolf.me", apk, version, code, "", time.Now())
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
	latest, err := readRelease(filepath.Join(data, "latest.json"))
	if err != nil || latest.Tag != newest.Tag {
		t.Fatalf("latest downgraded: %+v, %v", latest, err)
	}
	if _, err := publish(data, "https://folio.flandolf.me", testAPK(t, "changed"), "2.1.8", 218, "", time.Now()); err == nil {
		t.Fatal("overwrote an immutable release")
	}
	if _, err := publish(data, "https://folio.flandolf.me", apk, "../../escape", 219, "", time.Now()); err == nil {
		t.Fatal("accepted traversal version")
	}
	if _, err := publish(data, "https://folio.flandolf.me", apk, "2.1.9", 219, strings.Repeat("0", 64), time.Now()); err == nil {
		t.Fatal("accepted bad checksum")
	}
	latest, err = readRelease(filepath.Join(data, "latest.json"))
	if err != nil || latest.Tag != newest.Tag {
		t.Fatal("failed publish changed latest")
	}
}

func TestDownloadsAndAPI(t *testing.T) {
	data, apk := t.TempDir(), testAPK(t, "download me")
	r := addRelease(t, data, apk, "2.1.8", 218)
	server := httptest.NewServer(handler(data))
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
			if _, err := publish(data, "https://folio.flandolf.me", apk, version, code, "", time.Now()); err != nil {
				t.Error(err)
			}
		}(item.version, item.code)
	}
	wg.Wait()
	r, err := readRelease(filepath.Join(data, "latest.json"))
	if err != nil || r.VersionCode != 219 {
		t.Fatalf("bad concurrent latest: %+v %v", r, err)
	}
}

func TestEmptyAndCorruptCatalog(t *testing.T) {
	data := t.TempDir()
	w := httptest.NewRecorder()
	handler(data).ServeHTTP(w, httptest.NewRequest("GET", "/releases/latest.json", nil))
	if w.Code != 404 {
		t.Fatal("empty server should have no update")
	}
	if err := os.WriteFile(filepath.Join(data, "latest.json"), []byte("broken"), 0644); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	handler(data).ServeHTTP(w, httptest.NewRequest("GET", "/releases/latest.json", nil))
	if w.Code != 503 {
		t.Fatal("corrupt catalog must not mean up to date")
	}
	if _, err := publish(data, "https://folio.flandolf.me", testAPK(t, "one"), "2.1.8", 218, "", time.Now()); err == nil {
		t.Fatal("silently replaced corrupt catalog")
	}
}

func TestExperimentalRevisionsAndLegacyHistory(t *testing.T) {
	data, apk := t.TempDir(), testAPK(t, "revision")
	addRelease(t, data, apk, "2.1.7", 217)
	first := addRelease(t, data, apk, "2.1.7-exp.1", 2170001)
	second := addRelease(t, data, apk, "2.1.7-exp.2", 2170002)
	addRelease(t, data, apk, "2.1.7-exp.1", 2170001)
	latest, err := readRelease(filepath.Join(data, "latest.json"))
	if err != nil || latest.VersionCode != second.VersionCode {
		t.Fatal("revision pointer downgraded")
	}
	for _, r := range []release{first, second} {
		w := httptest.NewRecorder()
		handler(data).ServeHTTP(w, httptest.NewRequest("GET", "/releases/"+r.Tag+"/"+r.Assets[0].Name, nil))
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
		if _, err := publish(data, "https://folio.flandolf.me", apk, bad.version, bad.code, "", time.Now()); err == nil {
			t.Fatalf("accepted invalid revision: %+v", bad)
		}
	}
	stable := addRelease(t, data, apk, "2.1.8", 2180000)
	if stable.VersionCode <= second.VersionCode {
		t.Fatal("next stable did not supersede experimental")
	}
}
