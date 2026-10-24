package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNewerVersion(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"0.5.0", "0.4.0", true}, {"0.10.0", "0.9.9", true}, {"1.0.0", "0.99.99", true},
		{"0.4.0", "0.4.0", false}, {"0.3.0", "0.4.0", false}, {"v0.5.0", "0.4.0", true},
		{"0.5.0", "dev", false}, {"dev", "0.5.0", false}, {"", "0.1.0", false}, {"0.5.0-rc1", "0.4.0", false},
	} {
		if got := newerVersion(c.a, c.b); got != c.want {
			t.Errorf("newerVersion(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// fakeGitHub serves a release list and the installer it points at.
func fakeGitHub(t *testing.T, installer []byte, digest string) {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(installer)
	if digest == "" {
		digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	name := assetName()
	mux.HandleFunc("/releases", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `[
 {"tag_name":"v0.9.0","assets":[]},
 {"tag_name":"desktop-v0.6.0","prerelease":true,"assets":[]},
 {"tag_name":"desktop-v0.4.0","assets":[]},
 {"tag_name":"desktop-v0.5.0","assets":[{"name":%q,"browser_download_url":%q,"size":%d,"digest":%q},{"name":"other","browser_download_url":"x","size":1}]},
 {"tag_name":"desktop-v0.7.0","draft":true,"assets":[]}
]`, name, srv.URL+"/dl/desktop-v0.5.0/"+name, len(installer), digest)
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) { w.Write(installer) })
	oldAPI, oldPrefix := releasesAPI, downloadPrefix
	releasesAPI, downloadPrefix = srv.URL+"/releases", srv.URL+"/dl/"
	t.Cleanup(func() { releasesAPI, downloadPrefix = oldAPI, oldPrefix })
	updates.mu.Lock()
	updates.at = time.Time{}
	updates.mu.Unlock()
}

func withAppVersion(t *testing.T, v string) {
	old := appVersionFile
	appVersionFile = v
	t.Cleanup(func() { appVersionFile = old })
}

func TestCheckAppUpdate(t *testing.T) {
	if assetName() == "" {
		t.Skip("no installer for this system")
	}
	fakeGitHub(t, []byte("installer"), "")
	withAppVersion(t, "0.4.0\n")
	u := NewMoleService().CheckAppUpdate(true)
	if u.Error != "" || !u.Available || u.Latest != "0.5.0" || u.Current != "0.4.0" || !u.Installable || u.Asset != assetName() {
		t.Fatalf("%+v", u)
	}
	withAppVersion(t, "0.5.0")
	if u := NewMoleService().CheckAppUpdate(true); u.Available {
		t.Fatalf("already up to date: %+v", u)
	}
	withAppVersion(t, "dev")
	if u := NewMoleService().CheckAppUpdate(true); u.Available {
		t.Fatalf("a build from source is never told to update: %+v", u)
	}
}

func TestCheckAppUpdateOffline(t *testing.T) {
	releasesAPI = "http://127.0.0.1:1/releases"
	t.Cleanup(func() { releasesAPI = "https://api.github.com/repos/Luqueee/mole/releases?per_page=30" })
	withAppVersion(t, "0.4.0")
	u := NewMoleService().CheckAppUpdate(true)
	if u.Error == "" || u.Available {
		t.Fatalf("%+v", u)
	}
}

func waitProgress(t *testing.T, m *MoleService) UpdateProgress {
	t.Helper()
	for i := 0; i < 200; i++ {
		p := m.GetUpdateProgress()
		if p.Phase == "opened" || p.Phase == "failed" {
			return p
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("download did not finish")
	return UpdateProgress{}
}

func TestDownloadVerifiesBeforeOpening(t *testing.T) {
	if assetName() == "" {
		t.Skip("no installer for this system")
	}
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)
	t.Setenv("LocalAppData", cache)
	t.Setenv("PATH", t.TempDir()) // nothing to open the file with: opening fails after verification
	withAppVersion(t, "0.4.0")
	m := NewMoleService()

	// wrong digest: refused, and nothing is left behind
	fakeGitHub(t, []byte("tampered"), "sha256:"+strings.Repeat("0", 64))
	m.CheckAppUpdate(true)
	if err := m.DownloadAppUpdate(); err != nil {
		t.Fatal(err)
	}
	p := waitProgress(t, m)
	if p.Phase != "failed" || !strings.Contains(p.Error, "does not match") {
		t.Fatalf("%+v", p)
	}
	var leftovers []string
	_ = walk(cache, func(p string) { leftovers = append(leftovers, p) })
	if len(leftovers) != 0 {
		t.Fatalf("an unverified download was kept: %v", leftovers)
	}

	// right digest: verified and kept; opening fails only because the test has no opener
	fakeGitHub(t, []byte("the real installer"), "")
	m.CheckAppUpdate(true)
	if err := m.DownloadAppUpdate(); err != nil {
		t.Fatal(err)
	}
	p = waitProgress(t, m)
	if strings.Contains(p.Error, "does not match") || p.Bytes != int64(len("the real installer")) {
		t.Fatalf("%+v", p)
	}
	found := false
	_ = walk(cache, func(path string) {
		if strings.HasSuffix(path, assetName()) {
			b, _ := os.ReadFile(path)
			found = string(b) == "the real installer"
		}
	})
	if !found {
		t.Fatal("the verified installer was not kept")
	}
}

func TestDownloadRefusesWithoutADigestOrFromElsewhere(t *testing.T) {
	if assetName() == "" {
		t.Skip("no installer for this system")
	}
	withAppVersion(t, "0.4.0")
	m := NewMoleService()
	fakeGitHub(t, []byte("x"), "md5:abc")
	u := m.CheckAppUpdate(true)
	if u.Installable {
		t.Fatalf("a release without a sha256 digest must not be installable: %+v", u)
	}
	if err := m.DownloadAppUpdate(); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("%v", err)
	}
	// a download address outside the project is refused even with a digest
	fakeGitHub(t, []byte("x"), "")
	m.CheckAppUpdate(true)
	downloadPrefix = "https://github.com/Luqueee/mole/releases/download/"
	if err := m.DownloadAppUpdate(); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("%v", err)
	}
}

func TestOpenReleasePageOnlyOpensOurPages(t *testing.T) {
	m := NewMoleService()
	for _, u := range []string{"https://evil.example/", "file:///etc/passwd", "https://github.com/Luqueee/mole/releases/tag/x y", "", "-a calc"} {
		if err := m.OpenReleasePage(u); err == nil {
			t.Errorf("%q was opened", u)
		}
	}
}

func walk(dir string, f func(string)) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		p := dir + string(os.PathSeparator) + e.Name()
		if e.IsDir() {
			_ = walk(p, f)
		} else {
			f(p)
		}
	}
	return nil
}
