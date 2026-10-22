package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"golang.org/x/text/encoding/simplifiedchinese"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/Sakana-1314/MList/server/internal/config"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

const adminPW = "admin-secret-1"

// tenv is a fully wired application driven through its root http.Handler.
type tenv struct {
	t     *testing.T
	a     *App
	dir   string // scratch dir; mounts live below it
	ro    string // local dir mounted read-only for alice
	rw    string // local dir mounted writable for alice
	admin string // admin session token
}

func newEnv(t *testing.T) *tenv {
	t.Helper()
	dir := t.TempDir()
	if rp, err := filepath.EvalSymlinks(dir); err == nil {
		dir = rp
	}
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	cfg.DataDir = filepath.Join(dir, "data")
	cfg.CacheDir = filepath.Join(cfg.DataDir, "cache")
	cfg.Admin.Password = adminPW
	cfg.SMB.Smbd = filepath.Join(dir, "no-smbd")
	cfg.SMB.Smbpasswd = filepath.Join(dir, "no-smbpasswd")
	// Never bind real ports or start fsnotify from tests.
	cfg.Services.S3Enabled = false
	cfg.Search.Watch = false
	for _, d := range []string{cfg.DataDir, cfg.CacheDir} {
		must(t, os.MkdirAll(d, 0o755))
	}
	a, err := New(cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	e := &tenv{t: t, a: a, dir: dir, ro: filepath.Join(dir, "ro"), rw: filepath.Join(dir, "rw")}
	must(t, os.MkdirAll(e.ro, 0o755))
	must(t, os.MkdirAll(e.rw, 0o755))
	e.admin = e.login("admin", adminPW)
	return e
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, p string, data string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(p), 0o755))
	must(t, os.WriteFile(p, []byte(data), 0o644))
}

type reqOpt func(*http.Request)

func withHeader(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }
func withRemote(addr string) reqOpt { return func(r *http.Request) { r.RemoteAddr = addr } }
func withCookie(tok string) reqOpt {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "ml_session", Value: tok}) }
}

// do sends a request. tok (if non-empty) is sent as a bearer token. body may be
// nil, a string / []byte (raw) or any other value (JSON encoded).
func (e *tenv) do(method, target, tok string, body any, opts ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	var rd io.Reader
	isJSON := false
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		js, err := json.Marshal(b)
		must(e.t, err)
		rd, isJSON = bytes.NewReader(js), true
	}
	req := httptest.NewRequest(method, target, rd)
	if isJSON {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	e.a.Handler().ServeHTTP(rec, req)
	return rec
}

func expect(t *testing.T, rec *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, status, truncate(rec.Body.String()))
	}
	if status >= 400 && strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil || m["error"] == nil {
			t.Fatalf("error body is not {\"error\": ...}: %s", rec.Body.String())
		}
	}
}

func truncate(s string) string {
	if len(s) > 500 {
		return s[:500] + "..."
	}
	return s
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %T: %v; body: %s", v, err, truncate(rec.Body.String()))
	}
	return v
}

type obj = map[string]any

func q(p string) string { return url.QueryEscape(p) }

func (e *tenv) login(user, pw string) string {
	e.t.Helper()
	rec := e.do("POST", "/api/v1/auth/login", "", obj{"username": user, "password": pw})
	expect(e.t, rec, 200)
	return decode[obj](e.t, rec)["token"].(string)
}

type mountIn struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Writable bool   `json:"writable"`
}

// bucket returns the id of the bucket called name, creating it over paths.
func (e *tenv) bucket(name string, paths ...string) uint {
	e.t.Helper()
	rec := e.do("GET", "/api/v1/admin/buckets", e.admin, nil)
	expect(e.t, rec, 200)
	for _, b := range decode[[]obj](e.t, rec) {
		if b["name"] == name {
			return uint(b["id"].(float64))
		}
	}
	rec = e.do("POST", "/api/v1/admin/buckets", e.admin, obj{"name": name, "paths": paths})
	expect(e.t, rec, 201)
	return uint(decode[obj](e.t, rec)["bucket"].(obj)["id"].(float64))
}

// grants turns mounts into the grants of a user request, creating buckets.
func (e *tenv) grants(mounts ...mountIn) []obj {
	e.t.Helper()
	out := []obj{}
	for _, m := range mounts {
		out = append(out, obj{"bucket_id": e.bucket(m.Name, m.Path), "writable": m.Writable})
	}
	return out
}

func (e *tenv) createUser(name, pw, role string, mounts ...mountIn) uint {
	e.t.Helper()
	rec := e.do("POST", "/api/v1/admin/users", e.admin, obj{"username": name, "password": pw, "role": role, "grants": e.grants(mounts...)})
	expect(e.t, rec, 201)
	return uint(decode[obj](e.t, rec)["id"].(float64))
}

// alice has a read-only mount "只读" and a writable mount "可写".
func (e *tenv) alice() string {
	e.t.Helper()
	e.createUser("alice", "alice-pw", "user",
		mountIn{Name: "只读", Path: e.ro}, mountIn{Name: "可写", Path: e.rw, Writable: true})
	return e.login("alice", "alice-pw")
}

// ---------------------------------------------------------------------------

func TestAuthMethods(t *testing.T) {
	e := newEnv(t)

	rec := e.do("POST", "/api/v1/auth/login", "", obj{"username": "admin", "password": "wrong"})
	expect(t, rec, 401)
	rec = e.do("POST", "/api/v1/auth/login", "", obj{"username": "nobody", "password": "wrong"})
	expect(t, rec, 401)
	expect(t, e.do("POST", "/api/v1/auth/login", "", "not json"), 400)

	rec = e.do("POST", "/api/v1/auth/login", "", obj{"username": "admin", "password": adminPW})
	expect(t, rec, 200)
	body := decode[obj](t, rec)
	tok := body["token"].(string)
	if body["expires_at"] == nil || body["user"].(obj)["username"] != "admin" {
		t.Fatalf("login body: %v", body)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "ml_session" {
			cookie = c
		}
	}
	if cookie == nil || cookie.Value != tok || !cookie.HttpOnly {
		t.Fatalf("session cookie not set correctly: %+v", cookie)
	}

	expect(t, e.do("GET", "/api/v1/auth/me", tok, nil), 200)
	expect(t, e.do("GET", "/api/v1/auth/me", "", nil, withCookie(tok)), 200)
	expect(t, e.do("GET", "/api/v1/auth/me?access_token="+q(tok), "", nil), 200)
	expect(t, e.do("GET", "/api/v1/auth/me", "", nil), 401)
	expect(t, e.do("GET", "/api/v1/auth/me", "garbage", nil), 401)
	expect(t, e.do("GET", "/api/v1/auth/me", "ml_garbage", nil), 401)
	expect(t, e.do("GET", "/api/v1/auth/me", "", nil, withHeader("Authorization", "Bearer ")), 401)
	// A JWT signed with another key must be rejected.
	forged := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJ1aWQiOjEsInZlciI6MCwiZXhwIjo0MTAyNDQ0ODAwfQ.c2lnbmF0dXJl"
	expect(t, e.do("GET", "/api/v1/auth/me", forged, nil), 401)
	// alg=none must be rejected.
	none := "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.eyJ1aWQiOjEsInZlciI6MCwiZXhwIjo0MTAyNDQ0ODAwfQ."
	expect(t, e.do("GET", "/api/v1/auth/me", none, nil), 401)

	rec = e.do("POST", "/api/v1/auth/logout", tok, nil)
	expect(t, rec, 204)
	if !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("logout must expire cookie: %q", rec.Header().Get("Set-Cookie"))
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t)
	attacker := withRemote("192.0.2.7:1234")
	for i := 0; i < 10; i++ {
		expect(t, e.do("POST", "/api/v1/auth/login", "", obj{"username": "admin", "password": "nope"}, attacker), 401)
	}
	good := obj{"username": "admin", "password": adminPW}
	expect(t, e.do("POST", "/api/v1/auth/login", "", good, attacker), 429)
	// Spoofed forwarding headers from an untrusted peer must not reset the limit.
	expect(t, e.do("POST", "/api/v1/auth/login", "", good, attacker, withHeader("X-Forwarded-For", "198.51.100.1")), 429)
	expect(t, e.do("POST", "/api/v1/auth/login", "", good, attacker, withHeader("X-Real-IP", "198.51.100.2")), 429)
	// Other clients are unaffected.
	expect(t, e.do("POST", "/api/v1/auth/login", "", good, withRemote("192.0.2.8:1234")), 200)
	// A reverse proxy on localhost is trusted to report the client address.
	proxy := withRemote("127.0.0.1:5555")
	expect(t, e.do("POST", "/api/v1/auth/login", "", good, proxy, withHeader("X-Forwarded-For", "192.0.2.7")), 429)
	expect(t, e.do("POST", "/api/v1/auth/login", "", good, proxy, withHeader("X-Forwarded-For", "192.0.2.9")), 200)
}

func TestMeHidesMountPaths(t *testing.T) {
	e := newEnv(t)
	alice := e.alice()

	rec := e.do("GET", "/api/v1/auth/me", alice, nil)
	expect(t, rec, 200)
	if strings.Contains(rec.Body.String(), e.dir) || strings.Contains(rec.Body.String(), `"path"`) {
		t.Fatalf("/auth/me leaks local paths to a non-admin: %s", rec.Body.String())
	}
	me := decode[obj](t, rec)
	if me["role"] != "user" || len(me["grants"].([]any)) != 2 {
		t.Fatalf("me: %v", me)
	}
	for _, k := range []string{"password_hash", "PasswordHash", "password_enc", "PasswordEnc", "token_version", "TokenVersion"} {
		if _, ok := me[k]; ok {
			t.Fatalf("me leaks %s", k)
		}
	}
	// The login response must hide them too.
	rec = e.do("POST", "/api/v1/auth/login", "", obj{"username": "alice", "password": "alice-pw"})
	expect(t, rec, 200)
	if strings.Contains(rec.Body.String(), e.dir) {
		t.Fatalf("login response leaks local paths: %s", rec.Body.String())
	}
	// Paths are only exposed through the admin bucket endpoints.
	rec = e.do("GET", "/api/v1/admin/buckets", e.admin, nil)
	expect(t, rec, 200)
	if !strings.Contains(rec.Body.String(), e.ro) {
		t.Fatalf("admin should see bucket paths: %s", rec.Body.String())
	}
	expect(t, e.do("GET", "/api/v1/admin/buckets", alice, nil), 403)
}

func TestChangePassword(t *testing.T) {
	e := newEnv(t)
	alice := e.alice()
	expect(t, e.do("PUT", "/api/v1/auth/password", alice, obj{"old_password": "wrong", "new_password": "newpass1"}), 400)
	expect(t, e.do("PUT", "/api/v1/auth/password", alice, obj{"old_password": "alice-pw", "new_password": "123"}), 400)
	expect(t, e.do("PUT", "/api/v1/auth/password", alice, obj{"old_password": "alice-pw"}), 400)

	rec := e.do("PUT", "/api/v1/auth/password", alice, obj{"old_password": "alice-pw", "new_password": "newpass1"})
	expect(t, rec, 200)
	fresh := decode[obj](t, rec)["token"].(string)
	if fresh == "" || fresh == alice || !strings.Contains(rec.Header().Get("Set-Cookie"), fresh) {
		t.Fatal("password change must return and set a new session token")
	}
	expect(t, e.do("GET", "/api/v1/auth/me", alice, nil), 401)
	expect(t, e.do("GET", "/api/v1/auth/me", fresh, nil), 200)
	expect(t, e.do("POST", "/api/v1/auth/login", "", obj{"username": "alice", "password": "alice-pw"}), 401)
	e.login("alice", "newpass1")
}

func TestAdminUserPassword(t *testing.T) {
	e := newEnv(t)
	alice := e.alice()
	aliceID := uint(decode[obj](t, e.do("GET", "/api/v1/auth/me", alice, nil))["id"].(float64))
	pwURL := fmt.Sprintf("/api/v1/admin/users/%v/password", aliceID)

	// An administrator can read back the password that was set at creation.
	got := decode[obj](t, e.do("GET", pwURL, e.admin, nil))
	if got["password"] != "alice-pw" || got["known"] != true {
		t.Fatalf("password = %v, known = %v", got["password"], got["known"])
	}
	if cc := e.do("GET", pwURL, e.admin, nil).Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}

	// Changing the password updates what is revealed, and the new password logs in.
	expect(t, e.do("PUT", fmt.Sprintf("/api/v1/admin/users/%v", aliceID), e.admin,
		obj{"username": "alice", "password": "new-alice-pw"}), 200)
	if got := decode[obj](t, e.do("GET", pwURL, e.admin, nil)); got["password"] != "new-alice-pw" {
		t.Fatalf("after change: password = %v", got["password"])
	}
	// The old session was invalidated by the change, so log in again.
	alice = e.login("alice", "new-alice-pw")

	// Changing it from the account page must be reflected too.
	expect(t, e.do("PUT", "/api/v1/auth/password", alice,
		obj{"old_password": "new-alice-pw", "new_password": "third-pw-1"}), 200)
	if got := decode[obj](t, e.do("GET", pwURL, e.admin, nil)); got["password"] != "third-pw-1" {
		t.Fatalf("after self-change: password = %v", got["password"])
	}
	alice = e.login("alice", "third-pw-1")

	// A non-admin must not read another user's password.
	expect(t, e.do("GET", pwURL, alice, nil), 403)

	// The password ciphertext must never appear in the user listing or the
	// user's own profile — only the dedicated endpoint reveals it.
	for _, tc := range []struct{ url, tok string }{
		{"/api/v1/admin/users", e.admin},
		{"/api/v1/auth/me", alice},
	} {
		if body := e.do("GET", tc.url, tc.tok, nil).Body.String(); strings.Contains(body, "password_enc") {
			t.Fatalf("%s leaks the password ciphertext: %s", tc.url, truncate(body))
		}
	}
	// An unknown user is a 404.
	expect(t, e.do("GET", "/api/v1/admin/users/99999/password", e.admin, nil), 404)
	// A non-numeric id is rejected.
	expect(t, e.do("GET", "/api/v1/admin/users/abc/password", e.admin, nil), 400)

	// Accounts predating the reversible copy report known=false instead of failing,
	// and the copy is backfilled the next time that user logs in.
	db, err := gorm.Open(sqlite.Open(filepath.Join(e.dir, "data", "mlist.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	must(t, err)
	must(t, db.Exec("UPDATE users SET password_enc = '' WHERE username = 'alice'").Error)
	if sdb, err := db.DB(); err == nil {
		sdb.Close()
	}
	got = decode[obj](t, e.do("GET", pwURL, e.admin, nil))
	if got["known"] != false || got["password"] != "" {
		t.Fatalf("legacy account: %v", got)
	}
	e.login("alice", "third-pw-1")
	if got := decode[obj](t, e.do("GET", pwURL, e.admin, nil)); got["known"] != true || got["password"] != "third-pw-1" {
		t.Fatalf("backfill after login: %v", got)
	}
}

func TestAdminUsers(t *testing.T) {
	e := newEnv(t)
	alice := e.alice()
	users := "/api/v1/admin/users"

	// validation
	roID := e.bucket("只读", e.ro)
	expect(t, e.do("POST", users, e.admin, obj{"username": "bob", "password": "bob-pw1",
		"grants": []obj{{"bucket_id": 999}}}), 400)
	expect(t, e.do("POST", users, e.admin, obj{"username": "bob", "password": "bob-pw1",
		"grants": []obj{{"bucket_id": roID}, {"bucket_id": roID, "writable": true}}}), 400)
	expect(t, e.do("POST", users, e.admin, obj{"username": "bob", "password": "bob-pw1",
		"grants": []obj{{"writable": true}}}), 400)
	expect(t, e.do("POST", users, e.admin, obj{"username": "alice", "password": "whatever1"}), 409)
	for _, name := range []string{"Alice", "ALICE", "Admin"} {
		rec := e.do("POST", users, e.admin, obj{"username": name, "password": "whatever1"})
		expect(t, rec, 409)
		if decode[obj](t, rec)["error"] != "名称已被使用" {
			t.Fatalf("case-insensitive duplicate %s: %s", name, rec.Body.String())
		}
	}
	expect(t, e.do("POST", users, e.admin, obj{"username": "bad name", "password": "whatever1"}), 400)
	expect(t, e.do("POST", users, e.admin, obj{"username": "bob", "password": "short"}), 400)
	expect(t, e.do("POST", users, e.admin, obj{"username": "bob", "password": "bob-pw1", "role": "root"}), 400)

	rec := e.do("GET", users, e.admin, nil)
	expect(t, rec, 200)
	list := decode[[]obj](t, rec)
	if len(list) != 2 || list[1]["username"] != "alice" {
		t.Fatalf("users: %v", list)
	}
	if !strings.Contains(rec.Body.String(), "可写") || strings.Contains(rec.Body.String(), "password") {
		t.Fatalf("admin list should show grants and no password data: %s", rec.Body.String())
	}
	aliceID := uint(list[1]["id"].(float64))

	// non-admins
	expect(t, e.do("GET", users, alice, nil), 403)
	expect(t, e.do("POST", users, alice, obj{"username": "evil", "password": "evil-pw"}), 403)
	expect(t, e.do("GET", "/api/v1/admin/cache", alice, nil), 403)
	expect(t, e.do("POST", "/api/v1/admin/cache/clean", alice, nil), 403)
	expect(t, e.do("GET", "/api/v1/admin/browse?path=/", alice, nil), 403)
	expect(t, e.do("POST", "/api/v1/admin/search/reindex", alice, nil), 403)
	expect(t, e.do("DELETE", fmt.Sprintf("%s/%d", users, 1), alice, nil), 403)

	// can't delete self; last admin can't be demoted or disabled
	expect(t, e.do("DELETE", users+"/1", e.admin, nil), 400)
	expect(t, e.do("PUT", users+"/1", e.admin, obj{"username": "admin", "role": "user"}), 400)
	expect(t, e.do("PUT", users+"/1", e.admin, obj{"username": "admin", "role": "admin", "disabled": true}), 400)
	expect(t, e.do("GET", "/api/v1/auth/me", e.admin, nil), 200)

	// update: rename conflict, missing user, bad id
	expect(t, e.do("PUT", fmt.Sprintf("%s/%d", users, aliceID), e.admin, obj{"username": "admin"}), 409)
	expect(t, e.do("PUT", fmt.Sprintf("%s/%d", users, aliceID), e.admin, obj{"username": "ADMIN"}), 409)
	// changing the case of one's own name is allowed
	expect(t, e.do("PUT", fmt.Sprintf("%s/%d", users, aliceID), e.admin, obj{"username": "Alice", "role": "user"}), 200)
	expect(t, e.do("PUT", fmt.Sprintf("%s/%d", users, aliceID), e.admin, obj{"username": "alice", "role": "user"}), 200)
	if rec := e.do("GET", "/api/v1/fs/list?path=/", alice, nil); len(decode[obj](t, rec)["entries"].([]any)) != 2 {
		t.Fatal("omitting grants must keep them")
	}
	expect(t, e.do("PUT", users+"/999", e.admin, obj{"username": "zed"}), 404)
	expect(t, e.do("PUT", users+"/abc", e.admin, obj{"username": "zed"}), 400)

	// update keeps the password when empty; grants are replaced
	rec = e.do("PUT", fmt.Sprintf("%s/%d", users, aliceID), e.admin, obj{"username": "alice", "role": "user",
		"grants": e.grants(mountIn{Name: "只读", Path: e.ro})})
	expect(t, rec, 200)
	if gs := decode[obj](t, rec)["grants"].([]any); len(gs) != 1 || gs[0].(obj)["name"] != "只读" {
		t.Fatalf("grants after update = %v", gs)
	}
	expect(t, e.do("GET", "/api/v1/auth/me", alice, nil), 200) // no role/disabled change: session still valid
	rec = e.do("GET", "/api/v1/fs/list?path=/", alice, nil)
	if n := len(decode[obj](t, rec)["entries"].([]any)); n != 1 {
		t.Fatalf("alice should now see 1 mount, got %d", n)
	}
	e.login("alice", "alice-pw")

	// password reset by admin invalidates sessions
	expect(t, e.do("PUT", fmt.Sprintf("%s/%d", users, aliceID), e.admin, obj{"username": "alice", "password": "reset-pw"}), 200)
	expect(t, e.do("GET", "/api/v1/auth/me", alice, nil), 401)
	alice = e.login("alice", "reset-pw")

	// disabling logs the user out and blocks login
	expect(t, e.do("PUT", fmt.Sprintf("%s/%d", users, aliceID), e.admin, obj{"username": "alice", "disabled": true}), 200)
	expect(t, e.do("GET", "/api/v1/auth/me", alice, nil), 401)
	expect(t, e.do("POST", "/api/v1/auth/login", "", obj{"username": "alice", "password": "reset-pw"}), 403)

	// a second admin allows demoting the first one
	admin2 := e.createUser("admin2", "admin2-pw", "admin")
	expect(t, e.do("PUT", fmt.Sprintf("%s/%d", users, admin2), e.admin, obj{"username": "admin2", "role": "admin", "disabled": true}), 200)
	expect(t, e.do("PUT", users+"/1", e.admin, obj{"username": "admin", "role": "user"}), 400)

	// delete
	expect(t, e.do("DELETE", fmt.Sprintf("%s/%d", users, aliceID), e.admin, nil), 204)
	expect(t, e.do("DELETE", fmt.Sprintf("%s/%d", users, aliceID), e.admin, nil), 404)
	expect(t, e.do("POST", "/api/v1/auth/login", "", obj{"username": "alice", "password": "reset-pw"}), 401)

	// browse
	rec = e.do("GET", "/api/v1/admin/browse?path="+q(e.dir), e.admin, nil)
	expect(t, rec, 200)
	br := decode[obj](t, rec)
	dirs := br["dirs"].([]any)
	if !slices.Contains(dirs, any(e.ro)) || !slices.Contains(dirs, any(e.rw)) || br["parent"] != filepath.Dir(e.dir) {
		t.Fatalf("browse: %v", br)
	}
	expect(t, e.do("GET", "/api/v1/admin/browse?path=relative", e.admin, nil), 400)
}

// ---------------------------------------------------------------------------

func TestFileWritePermissions(t *testing.T) {
	e := newEnv(t)
	alice := e.alice()
	writeFile(t, filepath.Join(e.ro, "readme.txt"), "hello")
	writeFile(t, filepath.Join(e.ro, "sub", "a.txt"), "aaa")

	rec := e.do("GET", "/api/v1/fs/list", alice, nil)
	expect(t, rec, 200)
	root := decode[struct {
		Path     string `json:"path"`
		Writable bool   `json:"writable"`
		Entries  []struct {
			Name     string `json:"name"`
			Path     string `json:"path"`
			IsDir    bool   `json:"is_dir"`
			Writable bool   `json:"writable"`
		} `json:"entries"`
	}](t, rec)
	if root.Path != "/" || root.Writable || len(root.Entries) != 2 ||
		root.Entries[0].Name != "只读" || root.Entries[0].Writable || root.Entries[0].Path != "/只读" ||
		root.Entries[1].Name != "可写" || !root.Entries[1].Writable || !root.Entries[1].IsDir {
		t.Fatalf("root listing: %+v", root)
	}
	if strings.Contains(rec.Body.String(), e.dir) {
		t.Fatal("listing leaks local paths")
	}
	rec = e.do("GET", "/api/v1/fs/list?path="+q("/只读"), alice, nil)
	expect(t, rec, 200)
	if l := decode[obj](t, rec); l["writable"] != false || len(l["entries"].([]any)) != 2 {
		t.Fatalf("ro listing: %v", l)
	}
	rec = e.do("GET", "/api/v1/fs/list?path="+q("/可写/"), alice, nil)
	expect(t, rec, 200)
	if l := decode[obj](t, rec); l["writable"] != true || l["path"] != "/可写" {
		t.Fatalf("rw listing: %v", l)
	}
	expect(t, e.do("GET", "/api/v1/fs/list?path="+q("/nope"), alice, nil), 404)
	expect(t, e.do("GET", "/api/v1/fs/list?path="+q("/只读/readme.txt"), alice, nil), 400)
	rec = e.do("GET", "/api/v1/fs/stat?path="+q("/只读/readme.txt"), alice, nil)
	expect(t, rec, 200)
	if st := decode[obj](t, rec); st["size"] != float64(5) || st["kind"] != "text" || st["writable"] != false {
		t.Fatalf("stat: %v", st)
	}

	// PUT /fs/file
	expect(t, e.do("PUT", "/api/v1/fs/file?path="+q("/只读/new.txt"), alice, "x"), 403)
	expect(t, e.do("PUT", "/api/v1/fs/file?path="+q("/只读/readme.txt")+"&overwrite=true", alice, "x"), 403)
	rec = e.do("PUT", "/api/v1/fs/file?path="+q("/可写/new.txt"), alice, "first")
	expect(t, rec, 201)
	if en := decode[obj](t, rec); en["path"] != "/可写/new.txt" || en["size"] != float64(5) {
		t.Fatalf("put entry: %v", en)
	}
	expect(t, e.do("PUT", "/api/v1/fs/file?path="+q("/可写/new.txt"), alice, "second"), 409)
	expect(t, e.do("PUT", "/api/v1/fs/file?path="+q("/可写/new.txt")+"&overwrite=false", alice, "second"), 409)
	expect(t, e.do("PUT", "/api/v1/fs/file?path="+q("/可写/new.txt")+"&overwrite=true", alice, "second"), 201)
	if b, _ := os.ReadFile(filepath.Join(e.rw, "new.txt")); string(b) != "second" {
		t.Fatalf("overwrite content = %q", b)
	}
	expect(t, e.do("PUT", "/api/v1/fs/file?path="+q("/可写/深/层/目录/f.bin"), alice, "nested"), 201)
	if b, _ := os.ReadFile(filepath.Join(e.rw, "深", "层", "目录", "f.bin")); string(b) != "nested" {
		t.Fatal("nested upload did not create parents")
	}
	expect(t, e.do("PUT", "/api/v1/fs/file?path="+q("/可写"), alice, "x"), 400)
	expect(t, e.do("PUT", "/api/v1/fs/file?path="+q("/x.txt"), alice, "x"), 404)
	expect(t, e.do("PUT", "/api/v1/fs/file?path="+q("/可写/深"), alice, "x"), 400) // is a directory
	expect(t, e.do("PUT", "/api/v1/fs/file?path="+q("/可写/new.txt/child"), alice, "x"), 400)
	expect(t, e.do("PUT", "/api/v1/fs/file?path="+q("/可写/.lnf-upload-x"), alice, "x"), 400)

	// mkdir
	expect(t, e.do("POST", "/api/v1/fs/mkdir", alice, obj{"path": "/只读/d"}), 403)
	expect(t, e.do("POST", "/api/v1/fs/mkdir", alice, obj{"path": "/可写/d"}), 201)
	expect(t, e.do("POST", "/api/v1/fs/mkdir", alice, obj{"path": "/可写/d"}), 409)
	expect(t, e.do("POST", "/api/v1/fs/mkdir", alice, obj{"path": "/可写"}), 409)
	expect(t, e.do("POST", "/api/v1/fs/mkdir", alice, obj{"path": "/newmount"}), 404)
	expect(t, e.do("POST", "/api/v1/fs/mkdir", alice, obj{}), 400)

	// rename
	expect(t, e.do("POST", "/api/v1/fs/rename", alice, obj{"path": "/只读/readme.txt", "name": "x.txt"}), 403)
	rec = e.do("POST", "/api/v1/fs/rename", alice, obj{"path": "/可写/d", "name": "目录2"})
	expect(t, rec, 200)
	if decode[obj](t, rec)["path"] != "/可写/目录2" {
		t.Fatal("rename result path")
	}
	expect(t, e.do("POST", "/api/v1/fs/rename", alice, obj{"path": "/可写/目录2", "name": "../escape"}), 400)
	expect(t, e.do("POST", "/api/v1/fs/rename", alice, obj{"path": "/可写/目录2", "name": ".."}), 400)
	expect(t, e.do("POST", "/api/v1/fs/rename", alice, obj{"path": "/可写/new.txt", "name": "目录2"}), 409)
	expect(t, e.do("POST", "/api/v1/fs/rename", alice, obj{"path": "/可写", "name": "other"}), 403)

	type batch struct {
		Succeeded []string `json:"succeeded"`
		Failed    []struct {
			Path  string `json:"path"`
			Error string `json:"error"`
		} `json:"failed"`
	}
	// copy read-only -> writable (cross mount)
	rec = e.do("POST", "/api/v1/fs/copy", alice, obj{"sources": []string{"/只读/readme.txt", "/只读/sub"}, "dest_dir": "/可写/目录2"})
	expect(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `"failed":[]`) {
		t.Fatalf("failed must be an empty array: %s", rec.Body.String())
	}
	b := decode[batch](t, rec)
	if len(b.Succeeded) != 2 || len(b.Failed) != 0 {
		t.Fatalf("copy: %+v", b)
	}
	if got, _ := os.ReadFile(filepath.Join(e.rw, "目录2", "sub", "a.txt")); string(got) != "aaa" {
		t.Fatal("cross-mount directory copy failed")
	}
	// copy conflict, overwrite
	b = decode[batch](t, e.do("POST", "/api/v1/fs/copy", alice, obj{"sources": []string{"/只读/readme.txt"}, "dest_dir": "/可写/目录2"}))
	if len(b.Failed) != 1 || b.Failed[0].Path != "/只读/readme.txt" || b.Failed[0].Error == "" {
		t.Fatalf("copy conflict: %+v", b)
	}
	b = decode[batch](t, e.do("POST", "/api/v1/fs/copy", alice, obj{"sources": []string{"/只读/readme.txt"}, "dest_dir": "/可写/目录2", "overwrite": true}))
	if len(b.Succeeded) != 1 {
		t.Fatalf("copy overwrite: %+v", b)
	}
	// copy into read-only fails
	b = decode[batch](t, e.do("POST", "/api/v1/fs/copy", alice, obj{"sources": []string{"/可写/new.txt"}, "dest_dir": "/只读"}))
	if len(b.Failed) != 1 || b.Failed[0].Error != "该目录为只读" || len(b.Succeeded) != 0 {
		t.Fatalf("copy into ro: %+v", b)
	}
	// copy into itself
	b = decode[batch](t, e.do("POST", "/api/v1/fs/copy", alice, obj{"sources": []string{"/可写/目录2"}, "dest_dir": "/可写/目录2/sub"}))
	if len(b.Failed) != 1 {
		t.Fatalf("copy into itself: %+v", b)
	}

	// move: within writable ok; from / into read-only fails
	b = decode[batch](t, e.do("POST", "/api/v1/fs/move", alice, obj{"sources": []string{"/可写/new.txt", "/可写/missing"}, "dest_dir": "/可写/目录2"}))
	if !slices.Equal(b.Succeeded, []string{"/可写/new.txt"}) || len(b.Failed) != 1 || b.Failed[0].Path != "/可写/missing" {
		t.Fatalf("move: %+v", b)
	}
	b = decode[batch](t, e.do("POST", "/api/v1/fs/move", alice, obj{"sources": []string{"/只读/readme.txt"}, "dest_dir": "/可写"}))
	if len(b.Failed) != 1 || b.Failed[0].Error != "该目录为只读" {
		t.Fatalf("move from ro: %+v", b)
	}
	b = decode[batch](t, e.do("POST", "/api/v1/fs/move", alice, obj{"sources": []string{"/可写/目录2/new.txt"}, "dest_dir": "/只读"}))
	if len(b.Failed) != 1 || b.Failed[0].Error != "该目录为只读" {
		t.Fatalf("move into ro: %+v", b)
	}
	if _, err := os.Stat(filepath.Join(e.ro, "readme.txt")); err != nil {
		t.Fatal("read-only source must survive a failed move")
	}
	expect(t, e.do("POST", "/api/v1/fs/move", alice, obj{"sources": []string{"/a"}}), 400)

	// delete
	b = decode[batch](t, e.do("POST", "/api/v1/fs/delete", alice, obj{"paths": []string{"/只读/readme.txt", "/可写/目录2", "/可写/nothing", "/可写", "/"}}))
	if !slices.Equal(b.Succeeded, []string{"/可写/目录2"}) || len(b.Failed) != 4 || b.Failed[0].Error != "该目录为只读" {
		t.Fatalf("delete: %+v", b)
	}
	if _, err := os.Stat(filepath.Join(e.rw, "目录2")); !os.IsNotExist(err) {
		t.Fatal("directory not deleted")
	}
	if _, err := os.Stat(e.rw); err != nil {
		t.Fatal("mount root must never be deleted")
	}
	rec = e.do("POST", "/api/v1/fs/delete", alice, obj{"paths": []string{}})
	expect(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `"succeeded":[]`) {
		t.Fatalf("succeeded must be an empty array: %s", rec.Body.String())
	}
}

func TestPathTraversal(t *testing.T) {
	e := newEnv(t)
	alice := e.alice()
	secret := filepath.Join(e.dir, "secret.txt")
	writeFile(t, secret, "TOPSECRET")
	must(t, os.Symlink(secret, filepath.Join(e.rw, "link.txt")))
	must(t, os.Symlink(e.dir, filepath.Join(e.rw, "linkdir")))
	writeFile(t, filepath.Join(e.rw, "ok.txt"), "fine")

	rel, _ := filepath.Rel(e.rw, secret)
	reads := []string{
		"path=" + q("/可写/../secret.txt"),
		"path=" + q("/可写/../../../../../../"+strings.TrimPrefix(secret, "/")),
		"path=" + q("/可写/"+rel),
		"path=" + q("/可写/..\\secret.txt"),
		"path=" + q(secret),
		"path=" + q("/可写/link.txt"),
		"path=" + q("/可写/linkdir/secret.txt"),
		"path=" + q("/可写/ok.txt\x00/../link.txt"),
		"path=%2F%E5%8F%AF%E5%86%99%2F..%2fsecret.txt",
		"path=/%E5%8F%AF%E5%86%99/..%2f..%2f..%2fsecret.txt",
		"path=" + q("/可写/%2e%2e/secret.txt"),
		"path=" + q("//可写//..//secret.txt"),
	}
	for _, rq := range reads {
		for _, ep := range []string{"/api/v1/fs/raw?", "/api/v1/fs/text?", "/api/v1/fs/stat?", "/api/v1/fs/list?", "/api/v1/media/thumb?"} {
			rec := e.do("GET", ep+rq, alice, nil)
			if rec.Code < 400 || strings.Contains(rec.Body.String(), "TOPSECRET") || strings.Contains(rec.Body.String(), e.dir) {
				t.Errorf("%s%s escaped: %d %s", ep, rq, rec.Code, truncate(rec.Body.String()))
			}
		}
	}
	writes := []struct{ method, target string }{
		{"PUT", "/api/v1/fs/file?path=" + q("/可写/../evil.txt")},
		{"PUT", "/api/v1/fs/file?path=" + q("/可写/linkdir/evil.txt")},
		{"PUT", "/api/v1/fs/file?path=" + q("/可写/link.txt") + "&overwrite=true"},
		{"PUT", "/api/v1/fs/file?path=" + q(filepath.Join(e.dir, "evil.txt"))},
	}
	for _, w := range writes {
		if rec := e.do(w.method, w.target, alice, "pwned"); rec.Code < 400 {
			t.Errorf("%s %s should fail, got %d", w.method, w.target, rec.Code)
		}
	}
	for _, body := range []obj{
		{"path": "/可写/linkdir/evil"},
		{"path": "/可写/../evil"},
	} {
		if rec := e.do("POST", "/api/v1/fs/mkdir", alice, body); rec.Code < 400 {
			t.Errorf("mkdir %v should fail, got %d", body, rec.Code)
		}
	}
	rec := e.do("POST", "/api/v1/fs/copy", alice, obj{"sources": []string{"/可写/linkdir/secret.txt", "/可写/link.txt"}, "dest_dir": "/可写"})
	if len(decode[obj](t, rec)["succeeded"].([]any)) != 0 {
		t.Errorf("copy through symlink escaped: %s", rec.Body.String())
	}
	rec = e.do("POST", "/api/v1/fs/delete", alice, obj{"paths": []string{"/可写/linkdir/secret.txt", "/可写/../secret.txt"}})
	if len(decode[obj](t, rec)["succeeded"].([]any)) != 0 {
		t.Errorf("delete escaped: %s", rec.Body.String())
	}
	rec = e.do("POST", "/api/v1/fs/move", alice, obj{"sources": []string{"/可写/ok.txt"}, "dest_dir": "/可写/linkdir"})
	if len(decode[obj](t, rec)["succeeded"].([]any)) != 0 {
		t.Errorf("move into symlinked dir escaped: %s", rec.Body.String())
	}
	rec = e.do("POST", "/api/v1/fs/rename", alice, obj{"path": "/可写/ok.txt", "name": "../../escaped.txt"})
	if rec.Code < 400 {
		t.Errorf("rename escaped: %d", rec.Code)
	}
	if b, _ := os.ReadFile(secret); string(b) != "TOPSECRET" {
		t.Fatal("secret file was modified")
	}
	for _, p := range []string{filepath.Join(e.dir, "evil.txt"), filepath.Join(e.dir, "evil"), filepath.Join(e.dir, "escaped.txt"), filepath.Join(e.dir, "ok.txt")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was created outside the mount", p)
		}
	}
	// Symlinks escaping the root are hidden from listings.
	rec = e.do("GET", "/api/v1/fs/list?path="+q("/可写"), alice, nil)
	if strings.Contains(rec.Body.String(), "link") {
		t.Errorf("escaping symlinks should not be listed: %s", rec.Body.String())
	}
}

func TestRawAndText(t *testing.T) {
	e := newEnv(t)
	alice := e.alice()
	writeFile(t, filepath.Join(e.ro, "中文 文件.txt"), "0123456789")
	writeFile(t, filepath.Join(e.ro, "page.html"), "<script>alert(1)</script>")
	writeFile(t, filepath.Join(e.ro, "pic.svg"), `<svg xmlns="http://www.w3.org/2000/svg"/>`)
	gbk, _ := simplifiedchinese.GBK.NewEncoder().String("你好，世界！中文编码")
	writeFile(t, filepath.Join(e.ro, "gbk.txt"), gbk)
	writeFile(t, filepath.Join(e.ro, "utf8.md"), "# 标题\nhello")
	raw := "/api/v1/fs/raw?path=" + q("/只读/中文 文件.txt")

	rec := e.do("GET", raw, alice, nil)
	expect(t, rec, 200)
	if rec.Body.String() != "0123456789" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") ||
		rec.Header().Get("Accept-Ranges") != "bytes" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("raw: %v %q", rec.Header(), rec.Body.String())
	}
	if rec.Header().Get("Content-Security-Policy") != "" {
		t.Fatal("plain text should not need a CSP")
	}
	rec = e.do("GET", raw, alice, nil, withHeader("Range", "bytes=2-5"))
	expect(t, rec, 206)
	if rec.Body.String() != "2345" || rec.Header().Get("Content-Range") != "bytes 2-5/10" {
		t.Fatalf("range: %q %q", rec.Body.String(), rec.Header().Get("Content-Range"))
	}
	rec = e.do("GET", raw, alice, nil, withHeader("Range", "bytes=-3"))
	expect(t, rec, 206)
	if rec.Body.String() != "789" {
		t.Fatalf("suffix range: %q", rec.Body.String())
	}
	expect(t, e.do("GET", raw, alice, nil, withHeader("Range", "bytes=50-60")), 416)
	rec = e.do("HEAD", raw, alice, nil)
	expect(t, rec, 200)
	if rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != "10" {
		t.Fatalf("HEAD: len=%d %v", rec.Body.Len(), rec.Header())
	}

	for _, dl := range []string{"1", "true"} {
		rec = e.do("GET", raw+"&download="+dl, alice, nil)
		expect(t, rec, 200)
		disp, params, err := mime.ParseMediaType(rec.Header().Get("Content-Disposition"))
		if err != nil || disp != "attachment" || params["filename"] != "中文 文件.txt" {
			t.Fatalf("download disposition %q -> %q %v %v", rec.Header().Get("Content-Disposition"), disp, params, err)
		}
	}
	rec = e.do("GET", raw, alice, nil)
	if disp, _, _ := mime.ParseMediaType(rec.Header().Get("Content-Disposition")); disp != "inline" {
		t.Fatalf("default disposition = %q", disp)
	}

	for _, name := range []string{"page.html", "pic.svg"} {
		rec = e.do("GET", "/api/v1/fs/raw?path="+q("/只读/"+name), alice, nil)
		expect(t, rec, 200)
		if rec.Header().Get("Content-Security-Policy") != "sandbox" {
			t.Fatalf("%s must be served with CSP sandbox: %v", name, rec.Header())
		}
	}
	expect(t, e.do("GET", "/api/v1/fs/raw?path="+q("/只读"), alice, nil), 400)
	expect(t, e.do("GET", "/api/v1/fs/raw?path="+q("/只读/none"), alice, nil), 404)
	expect(t, e.do("GET", "/api/v1/fs/raw", alice, nil), 400)

	rec = e.do("GET", "/api/v1/fs/text?path="+q("/只读/gbk.txt"), alice, nil)
	expect(t, rec, 200)
	if tx := decode[obj](t, rec); tx["content"] != "你好，世界！中文编码" || tx["truncated"] != false {
		t.Fatalf("gbk text: %v", tx)
	}
	rec = e.do("GET", "/api/v1/fs/text?path="+q("/只读/utf8.md"), alice, nil)
	if tx := decode[obj](t, rec); tx["content"] != "# 标题\nhello" {
		t.Fatalf("utf8 text: %v", tx)
	}
	big := strings.Repeat("中", (4<<20)/3+10) // > 4 MiB, multi-byte at the cut
	writeFile(t, filepath.Join(e.ro, "big.txt"), big)
	rec = e.do("GET", "/api/v1/fs/text?path="+q("/只读/big.txt"), alice, nil)
	expect(t, rec, 200)
	tx := decode[obj](t, rec)
	if tx["truncated"] != true || !strings.HasPrefix(big, tx["content"].(string)) || len(tx["content"].(string)) < 4<<20-3 {
		t.Fatalf("truncated text: truncated=%v len=%d", tx["truncated"], len(tx["content"].(string)))
	}
	expect(t, e.do("GET", "/api/v1/fs/text?path="+q("/只读"), alice, nil), 400)
}

// ---------------------------------------------------------------------------

func TestAPITokens(t *testing.T) {
	e := newEnv(t)
	alice := e.alice()
	writeFile(t, filepath.Join(e.ro, "f.txt"), "data")

	expect(t, e.do("POST", "/api/v1/tokens", alice, obj{"name": ""}), 400)
	expect(t, e.do("POST", "/api/v1/tokens", alice, obj{"name": "  "}), 400)
	expect(t, e.do("POST", "/api/v1/tokens", alice, obj{"name": "x", "expires_in_days": -1}), 400)

	rec := e.do("POST", "/api/v1/tokens", alice, obj{"name": "ro"})
	expect(t, rec, 201)
	ro := decode[obj](t, rec)
	roTok := ro["token"].(string)
	if !strings.HasPrefix(roTok, "ml_") || !strings.HasPrefix(roTok, ro["prefix"].(string)) ||
		len(ro["prefix"].(string)) < 6 || len(ro["prefix"].(string)) >= len(roTok)/2 {
		t.Fatalf("token create: %v", ro)
	}
	if !slices.Equal(ro["scopes"].([]any), []any{"read"}) || ro["expires_at"] != nil {
		t.Fatalf("default scopes/expiry: %v", ro)
	}
	if _, ok := ro["hash"]; ok {
		t.Fatal("create response leaks hash")
	}
	if _, ok := ro["token_enc"]; ok {
		t.Fatal("create response leaks the encrypted token")
	}
	roID := int(ro["id"].(float64))
	// The plaintext is readable again later, and only by its owner.
	rec = e.do("GET", fmt.Sprintf("/api/v1/tokens/%d/secret", roID), alice, nil)
	expect(t, rec, 200)
	if got := decode[obj](t, rec)["token"]; got != roTok {
		t.Fatalf("revealed token %v != created %v", got, roTok)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("secret response must not be cached: %q", cc)
	}
	expect(t, e.do("GET", fmt.Sprintf("/api/v1/tokens/%d/secret", roID), e.admin, nil), 404)
	expect(t, e.do("GET", "/api/v1/tokens/999999/secret", alice, nil), 404)

	rec = e.do("POST", "/api/v1/tokens", alice, obj{"name": "rw", "scopes": []string{"write", "admin", "write"}, "expires_in_days": 30})
	expect(t, rec, 201)
	rw := decode[obj](t, rec)
	rwTok := rw["token"].(string)
	if !slices.Equal(rw["scopes"].([]any), []any{"read", "write"}) || rw["expires_at"] == nil {
		t.Fatalf("rw token: %v", rw)
	}

	rec = e.do("GET", "/api/v1/tokens", alice, nil)
	expect(t, rec, 200)
	list := decode[[]obj](t, rec)
	if len(list) != 2 || strings.Contains(rec.Body.String(), roTok) || strings.Contains(rec.Body.String(), rwTok) ||
		strings.Contains(strings.ToLower(rec.Body.String()), "hash") || strings.Contains(rec.Body.String(), "token_enc") {
		t.Fatalf("token list leaks secrets: %s", rec.Body.String())
	}
	for _, k := range []string{"id", "name", "prefix", "scopes", "expires_at", "last_used_at", "created_at"} {
		if _, ok := list[0][k]; !ok {
			t.Fatalf("token list item misses %s: %v", k, list[0])
		}
	}
	// other users don't see them
	rec = e.do("GET", "/api/v1/tokens", e.admin, nil)
	if !strings.Contains(rec.Body.String(), "[]") {
		t.Fatalf("admin sees alice's tokens: %s", rec.Body.String())
	}

	// read-only token
	expect(t, e.do("GET", "/api/v1/fs/list?path=/", roTok, nil), 200)
	expect(t, e.do("GET", "/api/v1/fs/raw?path="+q("/只读/f.txt"), roTok, nil), 200)
	expect(t, e.do("GET", "/api/v1/fs/raw?path="+q("/只读/f.txt")+"&access_token="+roTok, "", nil), 200)
	expect(t, e.do("GET", "/api/v1/auth/me", roTok, nil), 200)
	rec = e.do("GET", "/api/v1/fs/list?path="+q("/可写"), roTok, nil)
	if decode[obj](t, rec)["writable"] != false {
		t.Fatal("read-only token must see writable mounts as read-only")
	}
	expect(t, e.do("PUT", "/api/v1/fs/file?path="+q("/可写/x.txt"), roTok, "x"), 403)
	expect(t, e.do("POST", "/api/v1/fs/mkdir", roTok, obj{"path": "/可写/d"}), 403)
	writeFile(t, filepath.Join(e.rw, "keep.txt"), "k")
	expect(t, e.do("POST", "/api/v1/fs/rename", roTok, obj{"path": "/可写/keep.txt", "name": "y"}), 403)
	for _, op := range []string{"move", "copy"} {
		b := decode[obj](t, e.do("POST", "/api/v1/fs/"+op, roTok, obj{"sources": []string{"/只读/f.txt"}, "dest_dir": "/可写"}))
		if len(b["succeeded"].([]any)) != 0 {
			t.Fatalf("read-only token could %s", op)
		}
	}
	b := decode[obj](t, e.do("POST", "/api/v1/fs/delete", roTok, obj{"paths": []string{"/可写/keep.txt"}}))
	if len(b["succeeded"].([]any)) != 0 {
		t.Fatal("read-only token could delete")
	}
	expect(t, e.do("POST", "/api/v1/tokens", roTok, obj{"name": "escalate", "scopes": []string{"write"}}), 403)
	expect(t, e.do("DELETE", fmt.Sprintf("/api/v1/tokens/%v", rw["id"]), roTok, nil), 403)
	expect(t, e.do("POST", "/api/v1/s3keys", roTok, nil), 403)
	expect(t, e.do("PUT", "/api/v1/auth/password", roTok, obj{"old_password": "alice-pw", "new_password": "hacked1"}), 403)
	expect(t, e.do("GET", "/api/v1/tokens", roTok, nil), 200)

	// write token
	expect(t, e.do("PUT", "/api/v1/fs/file?path="+q("/可写/x.txt"), rwTok, "x"), 201)
	expect(t, e.do("PUT", "/api/v1/fs/file?path="+q("/只读/x.txt"), rwTok, "x"), 403)
	expect(t, e.do("POST", "/api/v1/fs/mkdir", rwTok, obj{"path": "/可写/d"}), 201)
	rec = e.do("POST", "/api/v1/tokens", rwTok, obj{"name": "child"})
	expect(t, rec, 201)
	child := decode[obj](t, rec)

	// last_used_at is recorded
	list = decode[[]obj](t, e.do("GET", "/api/v1/tokens", alice, nil))
	for _, it := range list {
		if it["name"] == "ro" && it["last_used_at"] == nil {
			t.Fatal("last_used_at not recorded")
		}
	}

	// revoke
	expect(t, e.do("DELETE", fmt.Sprintf("/api/v1/tokens/%v", child["id"]), e.admin, nil), 404) // not admin's token
	expect(t, e.do("DELETE", fmt.Sprintf("/api/v1/tokens/%v", child["id"]), alice, nil), 204)
	expect(t, e.do("DELETE", fmt.Sprintf("/api/v1/tokens/%v", child["id"]), alice, nil), 404)
	expect(t, e.do("DELETE", "/api/v1/tokens/abc", alice, nil), 400)
	expect(t, e.do("GET", "/api/v1/auth/me", child["token"].(string), nil), 401)

	// expired
	db, err := gorm.Open(sqlite.Open(filepath.Join(e.a.cfg.DataDir, "mlist.db")+"?_pragma=busy_timeout(5000)"),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	must(t, err)
	must(t, db.Exec("UPDATE access_tokens SET expires_at = ? WHERE id = ?", time.Now().Add(-time.Minute), rw["id"]).Error)
	if sdb, err := db.DB(); err == nil {
		sdb.Close()
	}
	expect(t, e.do("GET", "/api/v1/auth/me", rwTok, nil), 401)

	// tokens die with their user's disabled state
	aliceID := decode[obj](t, e.do("GET", "/api/v1/auth/me", alice, nil))["id"]
	expect(t, e.do("PUT", fmt.Sprintf("/api/v1/admin/users/%v", aliceID), e.admin, obj{"username": "alice", "disabled": true}), 200)
	if rec := e.do("GET", "/api/v1/auth/me", roTok, nil); rec.Code != 401 && rec.Code != 403 {
		t.Fatalf("token of disabled user: %d", rec.Code)
	}

	// read-only admin token: may read admin data but not change it
	rec = e.do("POST", "/api/v1/tokens", e.admin, obj{"name": "admin-ro"})
	admRO := decode[obj](t, rec)["token"].(string)
	expect(t, e.do("GET", "/api/v1/admin/users", admRO, nil), 200)
	expect(t, e.do("GET", "/api/v1/admin/cache", admRO, nil), 200)
	expect(t, e.do("POST", "/api/v1/admin/users", admRO, obj{"username": "x", "password": "xxxxxx"}), 403)
	expect(t, e.do("PUT", fmt.Sprintf("/api/v1/admin/users/%v", aliceID), admRO, obj{"username": "alice"}), 403)
	expect(t, e.do("DELETE", fmt.Sprintf("/api/v1/admin/users/%v", aliceID), admRO, nil), 403)
	expect(t, e.do("POST", "/api/v1/admin/cache/clean", admRO, nil), 403)
	expect(t, e.do("POST", "/api/v1/admin/search/reindex", admRO, nil), 403)
}

func TestS3Keys(t *testing.T) {
	e := newEnv(t)
	alice := e.alice()

	rec := e.do("POST", "/api/v1/s3keys", alice, nil)
	expect(t, rec, 201)
	k1 := decode[obj](t, rec)
	if k1["access_key"] == "" || len(k1["secret_key"].(string)) < 20 || k1["read_only"] != false {
		t.Fatalf("s3 key: %v", k1)
	}
	ak := k1["access_key"].(string)
	// The secret stays readable for its owner.
	rec = e.do("GET", "/api/v1/s3keys/"+ak+"/secret", alice, nil)
	expect(t, rec, 200)
	if got := decode[obj](t, rec)["secret_key"]; got != k1["secret_key"] {
		t.Fatalf("revealed secret %v != created %v", got, k1["secret_key"])
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("secret response must not be cached: %q", cc)
	}
	expect(t, e.do("GET", "/api/v1/s3keys/"+ak+"/secret", e.admin, nil), 404)
	expect(t, e.do("GET", "/api/v1/s3keys/deadbeef/secret", alice, nil), 404)
	if _, ok := k1["secret_enc"]; ok {
		t.Fatal("leaks encrypted secret")
	}
	rec = e.do("POST", "/api/v1/s3keys", alice, obj{"name": "backup", "read_only": true})
	expect(t, rec, 201)
	k2 := decode[obj](t, rec)
	if k2["name"] != "backup" || k2["read_only"] != true {
		t.Fatalf("s3 key 2: %v", k2)
	}
	expect(t, e.do("POST", "/api/v1/s3keys", alice, "{bad"), 400)

	rec = e.do("GET", "/api/v1/s3keys", alice, nil)
	expect(t, rec, 200)
	list := decode[[]obj](t, rec)
	if len(list) != 2 || strings.Contains(rec.Body.String(), k1["secret_key"].(string)) ||
		strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("s3 key list leaks secrets: %s", rec.Body.String())
	}
	if rec := e.do("GET", "/api/v1/s3keys", e.admin, nil); strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("admin sees alice's keys: %s", rec.Body.String())
	}
	expect(t, e.do("DELETE", "/api/v1/s3keys/"+k1["access_key"].(string), e.admin, nil), 404)
	expect(t, e.do("DELETE", "/api/v1/s3keys/"+k1["access_key"].(string), alice, nil), 204)
	expect(t, e.do("DELETE", "/api/v1/s3keys/"+k1["access_key"].(string), alice, nil), 404)
	if n := len(decode[[]obj](t, e.do("GET", "/api/v1/s3keys", alice, nil))); n != 1 {
		t.Fatalf("keys after delete = %d", n)
	}
}

// ---------------------------------------------------------------------------

func TestSearch(t *testing.T) {
	e := newEnv(t)
	// bob's mount contains alice's read-only mount, so the index has a single
	// top-level root and alice's results must be mapped onto her mount name.
	shared := filepath.Join(e.dir, "shared")
	aliceRO := filepath.Join(shared, "docs")
	writeFile(t, filepath.Join(aliceRO, "report-2024.pdf"), "pdf")
	writeFile(t, filepath.Join(shared, "report-secret.txt"), "secret")
	writeFile(t, filepath.Join(e.rw, "notes", "Report Final.txt"), "txt")
	writeFile(t, filepath.Join(e.dir, "outside", "report-outside.txt"), "x")
	e.createUser("alice", "alice-pw", "user",
		mountIn{Name: "文档", Path: aliceRO}, mountIn{Name: "可写", Path: e.rw, Writable: true})
	e.createUser("bob", "bob-pw1", "user", mountIn{Name: "共享", Path: shared})
	alice := e.login("alice", "alice-pw")
	e.a.search.Rebuild()

	type res struct {
		Results []struct {
			Name     string `json:"name"`
			Path     string `json:"path"`
			Kind     string `json:"kind"`
			Writable bool   `json:"writable"`
			Score    int    `json:"score"`
			Matches  []int  `json:"matches"`
		} `json:"results"`
		Total    int     `json:"total"`
		Indexing bool    `json:"indexing"`
		TookMS   float64 `json:"took_ms"`
	}
	rec := e.do("GET", "/api/v1/fs/search?q=report", alice, nil)
	expect(t, rec, 200)
	r := decode[res](t, rec)
	paths := []string{}
	for _, x := range r.Results {
		paths = append(paths, x.Path)
		if len(x.Matches) != 6 {
			t.Errorf("%s matches = %v", x.Path, x.Matches)
		}
	}
	slices.Sort(paths)
	if !slices.Equal(paths, []string{"/可写/notes/Report Final.txt", "/文档/report-2024.pdf"}) || r.Total != 2 {
		t.Fatalf("alice search: %v total=%d", paths, r.Total)
	}
	if strings.Contains(rec.Body.String(), e.dir) {
		t.Fatal("search leaks local paths")
	}
	for _, x := range r.Results {
		if x.Path == "/文档/report-2024.pdf" && (x.Kind != "pdf" || x.Writable || !slices.Equal(x.Matches, []int{0, 1, 2, 3, 4, 5})) {
			t.Errorf("pdf result: %+v", x)
		}
		if x.Path == "/可写/notes/Report Final.txt" && !x.Writable {
			t.Errorf("writable flag: %+v", x)
		}
	}
	// Chinese / unicode match positions are counted in characters.
	writeFile(t, filepath.Join(aliceRO, "年度总结报告.docx"), "x")
	e.a.search.Rebuild()
	r = decode[res](t, e.do("GET", "/api/v1/fs/search?q="+q("总结"), alice, nil))
	if len(r.Results) != 1 || r.Results[0].Path != "/文档/年度总结报告.docx" || !slices.Equal(r.Results[0].Matches, []int{2, 3}) {
		t.Fatalf("unicode search: %+v", r.Results)
	}

	r = decode[res](t, e.do("GET", "/api/v1/fs/search?q=report&path="+q("/可写"), alice, nil))
	if len(r.Results) != 1 || r.Results[0].Path != "/可写/notes/Report Final.txt" {
		t.Fatalf("scoped search: %+v", r.Results)
	}
	r = decode[res](t, e.do("GET", "/api/v1/fs/search?q=report&kind=pdf", alice, nil))
	if len(r.Results) != 1 || r.Results[0].Kind != "pdf" {
		t.Fatalf("kind search: %+v", r.Results)
	}
	r = decode[res](t, e.do("GET", "/api/v1/fs/search?q=report&limit=1", alice, nil))
	if len(r.Results) != 1 || r.Total != 2 {
		t.Fatalf("limit: %d total %d", len(r.Results), r.Total)
	}
	r = decode[res](t, e.do("GET", "/api/v1/fs/search?q=report&path="+q("/文档/../../"), alice, nil))
	if len(r.Results) != 2 {
		t.Fatalf("path traversal in search scope: %+v", r.Results)
	}
	r = decode[res](t, e.do("GET", "/api/v1/fs/search?q=report&path="+q("/共享"), alice, nil))
	if len(r.Results) != 0 {
		t.Fatalf("search in someone else's mount: %+v", r.Results)
	}
	expect(t, e.do("GET", "/api/v1/fs/search?q=", alice, nil), 400)
	expect(t, e.do("GET", "/api/v1/fs/search?q=%20", alice, nil), 400)

	// bob sees everything under shared, mapped to his mount
	bob := e.login("bob", "bob-pw1")
	r = decode[res](t, e.do("GET", "/api/v1/fs/search?q=report", bob, nil))
	paths = paths[:0]
	for _, x := range r.Results {
		paths = append(paths, x.Path)
	}
	slices.Sort(paths)
	if !slices.Equal(paths, []string{"/共享/docs/report-2024.pdf", "/共享/report-secret.txt"}) {
		t.Fatalf("bob search: %v", paths)
	}
}

// ---------------------------------------------------------------------------

func TestAdminCacheSearch(t *testing.T) {
	e := newEnv(t)

	// Runtime options now live in config.yaml; the admin API must not offer them.
	expect(t, e.do("GET", "/api/v1/admin/settings", e.admin, nil), 404)
	expect(t, e.do("PUT", "/api/v1/admin/settings", e.admin, obj{}), 404)

	rec := e.do("GET", "/api/v1/admin/cache", e.admin, nil)
	expect(t, rec, 200)
	c := decode[obj](t, rec)
	if c["dir"] != e.a.cfg.CacheDir || c["sessions"] == nil {
		t.Fatalf("cache stats: %v", c)
	}
	if _, ok := c["sessions"].([]any); !ok {
		t.Fatalf("sessions must be an array: %v", c)
	}
	if _, ok := c["last_cleanup"]; !ok {
		t.Fatalf("cache stats misses last_cleanup: %v", c)
	}
	for _, b := range []string{"thumb", "transcode"} {
		bs := c[b].(obj)
		for _, k := range []string{"files", "bytes", "max_bytes", "ttl_hours"} {
			if _, ok := bs[k]; !ok {
				t.Fatalf("cache %s misses %s: %v", b, k, bs)
			}
		}
	}
	if c["thumb"].(obj)["ttl_hours"] != float64(0) {
		t.Fatalf("default thumb ttl must be 0 (never expires): %v", c["thumb"])
	}
	for _, body := range []any{nil, obj{"kind": "thumb"}, obj{"kind": "all"}, obj{"kind": "transcode"}} {
		rec = e.do("POST", "/api/v1/admin/cache/clean", e.admin, body)
		expect(t, rec, 200)
		cr := decode[obj](t, rec)
		if _, ok := cr["removed_files"]; !ok {
			t.Fatalf("clean result: %v", cr)
		}
		if _, ok := cr["removed_bytes"]; !ok {
			t.Fatalf("clean result: %v", cr)
		}
	}
	expect(t, e.do("POST", "/api/v1/admin/cache/clean", e.admin, obj{"kind": "bogus"}), 400)
	// TTL 0: a fresh thumbnail is not expired, so cleaning must keep it.
	thumb := filepath.Join(e.a.cfg.CacheDir, "thumb", "ab", "abcdef.jpg")
	writeFile(t, thumb, "jpegdata")
	cr := decode[obj](t, e.do("POST", "/api/v1/admin/cache/clean", e.admin, obj{"kind": "thumb"}))
	if cr["removed_files"] != float64(0) || cr["removed_bytes"] != float64(0) {
		t.Fatalf("clean must only drop expired entries: %v", cr)
	}
	if _, err := os.Stat(thumb); err != nil {
		t.Fatalf("fresh thumbnail was removed: %v", err)
	}
	if last := decode[obj](t, e.do("GET", "/api/v1/admin/cache", e.admin, nil))["last_cleanup"]; last == nil {
		t.Fatal("last_cleanup not set after a cleanup")
	}

	// search status / reindex
	e.createUser("alice", "alice-pw", "user", mountIn{Name: "w", Path: e.rw, Writable: true})
	writeFile(t, filepath.Join(e.rw, "a", "b.txt"), "x")
	rec = e.do("GET", "/api/v1/admin/search/status", e.admin, nil)
	expect(t, rec, 200)
	st := decode[obj](t, rec)
	for _, k := range []string{"indexing", "entries", "roots", "last_built", "duration_ms"} {
		if _, ok := st[k]; !ok {
			t.Fatalf("index status misses %s: %v", k, st)
		}
	}
	expect(t, e.do("POST", "/api/v1/admin/search/reindex", e.admin, nil), 202)
	deadline := time.Now().Add(10 * time.Second)
	for {
		st = decode[obj](t, e.do("GET", "/api/v1/admin/search/status", e.admin, nil))
		if st["last_built"] != nil && st["indexing"] == false {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reindex did not finish: %v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st["roots"] != float64(1) || st["entries"] != float64(2) {
		t.Fatalf("index status after reindex: %v", st)
	}
}

func TestSystemInfo(t *testing.T) {
	e := newEnv(t)
	alice := e.alice()
	rec := e.do("GET", "/api/v1/system/info", alice, nil)
	expect(t, rec, 200)
	info := decode[struct {
		Version   string   `json:"version"`
		Hostname  string   `json:"hostname"`
		Addresses []string `json:"addresses"`
		HTTPPort  *int     `json:"http_port"`
		FFmpeg    *bool    `json:"ffmpeg"`
		Services  map[string]map[string]any
	}](t, rec)
	if info.Version != "test" || info.Addresses == nil || info.HTTPPort == nil || info.FFmpeg == nil {
		t.Fatalf("system info: %s", rec.Body.String())
	}
	for _, name := range []string{"webdav", "s3", "smb"} {
		sv, ok := info.Services[name]
		if !ok {
			t.Fatalf("missing service %s: %s", name, rec.Body.String())
		}
		if _, ok := sv["enabled"].(bool); !ok {
			t.Fatalf("%s.enabled: %v", name, sv)
		}
		if _, ok := sv["running"].(bool); !ok {
			t.Fatalf("%s.running: %v", name, sv)
		}
	}
	if info.Services["webdav"]["path"] != "/dav" || info.Services["webdav"]["enabled"] != true {
		t.Fatalf("webdav: %v", info.Services["webdav"])
	}
	// S3 is disabled in tests and shares the main HTTP port behind /s3.
	if info.Services["s3"]["enabled"] != false || info.Services["s3"]["path"] != "/s3" {
		t.Fatalf("s3: %v", info.Services["s3"])
	}
	if _, ok := info.Services["s3"]["region"]; ok {
		t.Fatalf("system info must not expose an s3 region: %v", info.Services["s3"])
	}
	if info.Services["smb"]["enabled"] != false || info.Services["smb"]["port"] != float64(445) {
		t.Fatalf("smb: %v", info.Services["smb"])
	}
	expect(t, e.do("GET", "/api/v1/system/info", "", nil), 401)
}

func TestRoutingAndSpec(t *testing.T) {
	e := newEnv(t)

	rec := e.do("GET", "/api/openapi.json", "", nil)
	expect(t, rec, 200)
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("openapi content type %q", rec.Header().Get("Content-Type"))
	}
	var spec struct {
		OpenAPI string                               `json:"openapi"`
		Paths   map[string]map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil || spec.OpenAPI == "" || len(spec.Paths) < 20 {
		t.Fatalf("openapi.json: %v", err)
	}

	rec = e.do("GET", "/api/x", "", nil)
	expect(t, rec, 404)
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatal("unknown API route must return JSON")
	}
	expect(t, e.do("GET", "/api/v1/nope", e.admin, nil), 404)
	expect(t, e.do("DELETE", "/api/v1/fs/list", e.admin, nil), 404)

	// Windows' WebDAV client probes OPTIONS / before mapping /dav/.
	rec = e.do("OPTIONS", "/", "", nil)
	expect(t, rec, 200)
	if rec.Header().Get("DAV") == "" {
		t.Fatal("OPTIONS / must advertise DAV")
	}

	for _, p := range []string{"/files/abc", "/", "/settings/tokens"} {
		rec = e.do("GET", p, "", nil)
		expect(t, rec, 200)
		if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") || !strings.Contains(strings.ToLower(rec.Body.String()), "<!doctype html") {
			t.Fatalf("SPA fallback for %s: %v", p, rec.Header())
		}
	}

	// 规范里每个 operation 都必须真实存在路由；未标注 security 的视为公开接口，
	// 且只允许是下面这几个（新增公开接口时必须显式登记，避免误把受保护接口写成公开）。
	publicOps := map[string]int{
		"POST /auth/login":  400, // 未带凭据时由参数校验拒绝
		"POST /auth/logout": 204, // 只清 Cookie，必须在会话过期后仍可用
		"GET /media/thumb":  403, // 只校验摘要签名，不要求登录
		// 分享链接：凭随机令牌只读访问，未知令牌返回 404
		"GET /share/{token}":            404,
		"GET /share/{token}/list":       404,
		"GET /share/{token}/raw":        404,
		"GET /share/{token}/raw/{name}": 404,
		"GET /share/{token}/text":       404,
	}
	seen := map[string]bool{}
	for p, ops := range spec.Paths {
		for method := range ops {
			m := strings.ToUpper(method)
			target := "/api/v1" + replacePathParams(p)
			key := m + " " + p
			rec := e.do(m, target, "", nil)
			_, requiresAuth := ops[method]["security"]
			if rec.Code == 404 && strings.Contains(rec.Body.String(), "接口不存在") {
				t.Errorf("%s %s is in the spec but not routed", m, target)
				continue
			}
			if !requiresAuth {
				want, ok := publicOps[key]
				if !ok {
					t.Errorf("%s %s is public in the spec but is not a known public operation", m, target)
					continue
				}
				seen[key] = true
				if rec.Code != want {
					t.Errorf("%s %s without credentials: got %d, want %d", m, target, rec.Code, want)
				}
				continue
			}
			if rec.Code != 401 {
				t.Errorf("%s %s without credentials: %d", m, target, rec.Code)
			}
		}
	}
	for key := range publicOps {
		if !seen[key] {
			t.Errorf("%s is expected to be public, but the spec marks it as protected", key)
		}
	}
}

// replacePathParams 把规范里的路径参数替换成可用于请求路由的样例值。
func replacePathParams(p string) string {
	return pathParamRe.ReplaceAllStringFunc(p, func(m string) string {
		switch strings.TrimSuffix(strings.TrimPrefix(m, "{"), "}") {
		case "file":
			return "0.ts"
		case "id", "access_key":
			return "x"
		default:
			return "x"
		}
	})
}

var pathParamRe = regexp.MustCompile(`\{[^}]+\}`)

// ---------------------------------------------------------------------------

func TestMedia(t *testing.T) {
	e := newEnv(t)
	alice := e.alice()

	img := image.NewRGBA(image.Rect(0, 0, 640, 320))
	for x := 0; x < 640; x++ {
		for y := 0; y < 320; y++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var buf bytes.Buffer
	must(t, png.Encode(&buf, img))
	writeFile(t, filepath.Join(e.ro, "pic.png"), buf.String())
	writeFile(t, filepath.Join(e.ro, "note.txt"), "text")

	// A listing hands out the content digest plus its signature; the preview is
	// then fetched by digest alone, with no path in the URL.
	rec := e.do("GET", "/api/v1/fs/list?path="+q("/只读"), alice, nil)
	expect(t, rec, 200)
	list := decode[struct {
		Entries []struct {
			Name     string `json:"name"`
			Thumb    string `json:"thumb"`
			ThumbSig string `json:"thumb_sig"`
		} `json:"entries"`
	}](t, rec)
	byName := map[string]struct{ thumb, sig string }{}
	for _, en := range list.Entries {
		byName[en.Name] = struct{ thumb, sig string }{en.Thumb, en.ThumbSig}
	}
	pic := byName["pic.png"]
	if pic.thumb == "" || pic.sig == "" {
		t.Fatalf("pic.png carries no preview digest: %+v", pic)
	}
	// The digest is not a path: it must not leak the file location.
	if strings.Contains(pic.thumb, "pic.png") || strings.Contains(pic.thumb, "/") {
		t.Fatalf("digest leaks the path: %q", pic.thumb)
	}
	if txt := byName["note.txt"]; txt.thumb != "" {
		t.Fatalf("non-previewable file carries a digest: %+v", txt)
	}

	thumbURL := "/api/v1/media/thumb?size=128&d=" + q(pic.thumb) + "&s=" + q(pic.sig)
	rec = e.do("GET", thumbURL, "", nil) // no session: the signature is the credential
	expect(t, rec, 200)
	if rec.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("thumb content type %q", rec.Header().Get("Content-Type"))
	}
	th, err := jpeg.Decode(rec.Body)
	if err != nil || th.Bounds().Dx() > 128 || th.Bounds().Dy() > 128 {
		t.Fatalf("thumb: %v %v", err, th)
	}
	// Bad signature, unknown digest and tampered digest must not produce an image.
	expect(t, e.do("GET", "/api/v1/media/thumb?d="+q(pic.thumb)+"&s=bogus", alice, nil), 403)
	expect(t, e.do("GET", "/api/v1/media/thumb?d=deadbeef&s="+q(pic.sig), alice, nil), 403)
	expect(t, e.do("GET", "/api/v1/media/thumb?d="+q(pic.thumb), alice, nil), 403)
	// A correctly signed but unknown digest is a clean 404, not an error.
	expect(t, e.do("GET", "/api/v1/media/thumb?d=deadbeef&s="+q(e.a.auth.ThumbSignature("deadbeef")), alice, nil), 404)

	if !e.a.media.HasFFmpeg() {
		t.Skip("ffmpeg/ffprobe not installed")
	}
	clip := filepath.Join(e.rw, "clip.mp4")
	out, err := exec.Command("ffmpeg", "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=duration=3:size=320x240:rate=25",
		"-f", "lavfi", "-i", "sine=duration=3", "-shortest", "-pix_fmt", "yuv420p", clip).CombinedOutput()
	if err != nil {
		t.Skipf("ffmpeg cannot generate test clip: %v %s", err, out)
	}
	writeFile(t, filepath.Join(e.rw, "clip.srt"), "1\n00:00:00,000 --> 00:00:01,000\n你好\n")

	rec = e.do("GET", "/api/v1/media/info?path="+q("/可写/clip.mp4"), alice, nil)
	expect(t, rec, 200)
	mi := decode[struct {
		Duration   float64 `json:"duration"`
		Format     string  `json:"format"`
		DirectPlay bool    `json:"direct_play"`
		Video      *struct {
			Codec  string `json:"codec"`
			Width  int    `json:"width"`
			Height int    `json:"height"`
		} `json:"video"`
		Audio     []obj `json:"audio"`
		Subtitles []struct {
			External string `json:"external"`
		} `json:"subtitles"`
	}](t, rec)
	if mi.Duration < 2.5 || mi.Duration > 3.5 || mi.Video == nil || mi.Video.Width != 320 || mi.Video.Height != 240 ||
		len(mi.Audio) != 1 || len(mi.Subtitles) != 1 || mi.Subtitles[0].External != "/可写/clip.srt" {
		t.Fatalf("media info: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), e.dir) {
		t.Fatal("media info leaks local paths")
	}
	expect(t, e.do("GET", "/api/v1/media/info?path="+q("/只读/note.txt"), alice, nil), 415)

	// The video preview also comes from the digest a listing handed out.
	rec = e.do("GET", "/api/v1/fs/stat?path="+q("/可写/clip.mp4"), alice, nil)
	expect(t, rec, 200)
	clipMeta := decode[struct {
		Thumb    string `json:"thumb"`
		ThumbSig string `json:"thumb_sig"`
	}](t, rec)
	if clipMeta.Thumb == "" || clipMeta.ThumbSig == "" {
		t.Fatalf("video carries no preview digest: %+v", clipMeta)
	}
	thumb := e.do("GET", "/api/v1/media/thumb?d="+q(clipMeta.Thumb)+"&s="+q(clipMeta.ThumbSig), alice, nil)
	expect(t, thumb, 200)
	if _, err := jpeg.Decode(thumb.Body); err != nil {
		t.Fatalf("video thumb: %v", err)
	}

	rec = e.do("GET", "/api/v1/media/subtitle?path="+q("/可写/clip.srt"), alice, nil)
	expect(t, rec, 200)
	if !strings.HasPrefix(rec.Body.String(), "WEBVTT") || !strings.Contains(rec.Body.String(), "你好") {
		t.Fatalf("subtitle: %q", rec.Body.String())
	}
	expect(t, e.do("GET", "/api/v1/media/subtitle?path="+q("/只读/note.txt"), alice, nil), 400)

	// HLS smoke test
	expect(t, e.do("POST", "/api/v1/media/transcode", alice, obj{"path": "/只读/note.txt"}), 415)
	rec = e.do("POST", "/api/v1/media/transcode", alice, obj{"path": "/可写/clip.mp4", "quality": "480p"})
	expect(t, rec, 200)
	tc := decode[obj](t, rec)
	id := tc["id"].(string)
	if tc["playlist"] != "/api/v1/media/hls/"+id+"/index.m3u8" || tc["duration"].(float64) < 2.5 {
		t.Fatalf("transcode: %v", tc)
	}
	// Without a codec the server picks HEVC when it can encode it.
	codecs := decode[obj](t, e.do("GET", "/api/v1/media/info?path="+q("/可写/clip.mp4"), alice, nil))["transcode_codecs"].([]any)
	wantCodec := "h264"
	if slices.Contains(codecs, any("h265")) {
		wantCodec = "h265"
	}
	if tc["codec"] != wantCodec {
		t.Fatalf("default codec %v, want %s (server codecs %v)", tc["codec"], wantCodec, codecs)
	}
	rec = e.do("GET", tc["playlist"].(string), alice, nil)
	expect(t, rec, 200)
	if rec.Header().Get("Content-Type") != "application/vnd.apple.mpegurl" ||
		!strings.HasPrefix(rec.Body.String(), "#EXTM3U") || !strings.Contains(rec.Body.String(), "0.ts") {
		t.Fatalf("playlist: %v %s", rec.Header(), rec.Body.String())
	}
	// The access token is propagated to segment URLs for <video> playback.
	rec = e.do("GET", tc["playlist"].(string)+"?access_token="+q(alice), "", nil)
	expect(t, rec, 200)
	if !strings.Contains(rec.Body.String(), "access_token="+q(alice)) {
		t.Fatalf("playlist with access_token: %s", rec.Body.String())
	}
	rec = e.do("GET", "/api/v1/media/hls/"+id+"/0.ts", alice, nil)
	expect(t, rec, 200)
	if rec.Header().Get("Content-Type") != "video/mp2t" || rec.Body.Len() < 188 || rec.Body.Bytes()[0] != 0x47 {
		t.Fatalf("segment 0: %v len=%d", rec.Header(), rec.Body.Len())
	}
	// Sessions are per user.
	e.createUser("bob", "bob-pw1", "user", mountIn{Name: "w", Path: e.rw})
	bob := e.login("bob", "bob-pw1")
	expect(t, e.do("GET", tc["playlist"].(string), bob, nil), 404)
	expect(t, e.do("GET", "/api/v1/media/hls/"+id+"/0.ts", bob, nil), 404)
	expect(t, e.do("DELETE", "/api/v1/media/hls/"+id, bob, nil), 404)
	expect(t, e.do("GET", "/api/v1/media/hls/"+id+"/abc.ts", alice, nil), 404)
	expect(t, e.do("GET", "/api/v1/media/hls/"+id+"/999.ts", alice, nil), 404)
	expect(t, e.do("GET", "/api/v1/media/hls/nope/index.m3u8", alice, nil), 404)

	sessions := decode[obj](t, e.do("GET", "/api/v1/admin/cache", e.admin, nil))["sessions"].([]any)
	if len(sessions) != 1 || sessions[0].(obj)["path"] != "/可写/clip.mp4" {
		t.Fatalf("cache sessions: %v", sessions)
	}
	// Stopping kills ffmpeg but keeps the session and its cached segments.
	expect(t, e.do("DELETE", "/api/v1/media/hls/"+id, alice, nil), 204)
	expect(t, e.do("DELETE", "/api/v1/media/hls/"+id, alice, nil), 204)
	expect(t, e.do("GET", "/api/v1/media/hls/"+id+"/0.ts", alice, nil), 200)
}

func TestAdminBuckets(t *testing.T) {
	e := newEnv(t)
	buckets := "/api/v1/admin/buckets"
	a, b, c := filepath.Join(e.dir, "a"), filepath.Join(e.dir, "b"), filepath.Join(e.dir, "c")
	writeFile(t, filepath.Join(a, "docs", "1.txt"), "one")
	writeFile(t, filepath.Join(a, "same.txt"), "same")
	writeFile(t, filepath.Join(b, "docs", "2.txt"), "two")
	writeFile(t, filepath.Join(b, "same.txt"), "same")
	writeFile(t, filepath.Join(c, "docs", "1.txt"), "different")

	// validation
	expect(t, e.do("POST", buckets, e.admin, obj{"name": "x", "paths": []string{filepath.Join(e.dir, "missing")}}), 400)
	expect(t, e.do("POST", buckets, e.admin, obj{"name": "x", "paths": []string{"relative"}}), 400)
	expect(t, e.do("POST", buckets, e.admin, obj{"name": "x", "paths": []string{}}), 400)
	expect(t, e.do("POST", buckets, e.admin, obj{"name": "a/b", "paths": []string{a}}), 400)
	expect(t, e.do("POST", buckets, e.admin, obj{"name": "x", "paths": []string{a, filepath.Join(a, "docs")}}), 400)

	// Conflicting content is refused and nothing is copied.
	rec := e.do("POST", buckets, e.admin, obj{"name": "资料", "paths": []string{a, b, c}})
	expect(t, rec, 409)
	cs := decode[obj](t, rec)["conflicts"].([]any)
	if len(cs) != 1 || cs[0].(obj)["path"] != "/docs/1.txt" {
		t.Fatalf("conflicts %v", cs)
	}
	if _, err := os.Stat(filepath.Join(a, "docs", "2.txt")); err == nil {
		t.Fatal("refused merge must not copy anything")
	}

	// Non-conflicting directories are merged.
	rec = e.do("POST", buckets, e.admin, obj{"name": "资料", "paths": []string{a, b}})
	expect(t, rec, 201)
	created := decode[obj](t, rec)
	id := uint(created["bucket"].(obj)["id"].(float64))
	if n := created["sync"].(obj)["copied"].(float64); n != 2 {
		t.Fatalf("copied %v", created["sync"])
	}
	for _, d := range []string{a, b} {
		for _, f := range []string{"docs/1.txt", "docs/2.txt", "same.txt"} {
			if _, err := os.Stat(filepath.Join(d, f)); err != nil {
				t.Fatalf("%s missing in %s", f, d)
			}
		}
	}
	expect(t, e.do("POST", buckets, e.admin, obj{"name": "资料", "paths": []string{c}}), 409)

	// Writes through the API land in both directories.
	e.createUser("alice", "alice-pw", "user", mountIn{Name: "资料", Path: a, Writable: true})
	alice := e.login("alice", "alice-pw")
	expect(t, e.do("PUT", "/api/v1/fs/file?path="+q("/资料/new/up.txt"), alice, "uploaded"), 201)
	if got, _ := os.ReadFile(filepath.Join(b, "new", "up.txt")); string(got) != "uploaded" {
		t.Fatalf("backup copy: %q", got)
	}
	expect(t, e.do("POST", "/api/v1/fs/delete", alice, obj{"paths": []string{"/资料/docs"}}), 200)
	if _, err := os.Stat(filepath.Join(b, "docs")); !os.IsNotExist(err) {
		t.Fatal("delete not replicated")
	}

	// Out-of-band changes are repaired by a sync.
	writeFile(t, filepath.Join(b, "smb.txt"), "via smb")
	rec = e.do("POST", fmt.Sprintf("%s/%d/sync", buckets, id), e.admin, nil)
	expect(t, rec, 200)
	if got, _ := os.ReadFile(filepath.Join(a, "smb.txt")); string(got) != "via smb" {
		t.Fatalf("sync: %q", got)
	}

	// Adding a conflicting directory is refused; reordering is not a merge.
	expect(t, e.do("PUT", fmt.Sprintf("%s/%d", buckets, id), e.admin, obj{"name": "资料", "paths": []string{a, b, c}}), 200)
	writeFile(t, filepath.Join(e.dir, "d", "same.txt"), "not the same")
	expect(t, e.do("PUT", fmt.Sprintf("%s/%d", buckets, id), e.admin,
		obj{"name": "资料", "paths": []string{a, b, filepath.Join(e.dir, "d")}}), 409)
	rec = e.do("PUT", fmt.Sprintf("%s/%d", buckets, id), e.admin, obj{"name": "文档", "paths": []string{b, a}})
	expect(t, rec, 200)

	// The listing shows directory status and users; the offline primary falls back.
	must(t, os.Rename(b, b+".off"))
	rec = e.do("GET", buckets, e.admin, nil)
	expect(t, rec, 200)
	list := decode[[]obj](t, rec)
	if len(list) != 1 || list[0]["name"] != "文档" || list[0]["users"].(float64) != 1 {
		t.Fatalf("list %v", list)
	}
	dirs := list[0]["dirs"].([]any)
	if dirs[0].(obj)["available"] != false || dirs[1].(obj)["active"] != true {
		t.Fatalf("dirs %v", dirs)
	}
	rec = e.do("GET", "/api/v1/fs/list?path="+q("/文档"), alice, nil)
	expect(t, rec, 200)
	if !strings.Contains(rec.Body.String(), "smb.txt") {
		t.Fatalf("fallback listing %s", rec.Body.String())
	}
	must(t, os.Rename(b+".off", b))

	// Deleting the bucket revokes access but keeps the files.
	expect(t, e.do("DELETE", fmt.Sprintf("%s/%d", buckets, id), e.admin, nil), 204)
	expect(t, e.do("DELETE", fmt.Sprintf("%s/%d", buckets, id), e.admin, nil), 404)
	if rec := e.do("GET", "/api/v1/fs/list?path=/", alice, nil); len(decode[obj](t, rec)["entries"].([]any)) != 0 {
		t.Fatal("grant survived bucket deletion")
	}
	if _, err := os.Stat(filepath.Join(a, "smb.txt")); err != nil {
		t.Fatal("bucket deletion removed files")
	}
}

func TestShares(t *testing.T) {
	e := newEnv(t)
	alice := e.alice()
	aliceID := uint(1)
	for _, u := range decode[[]obj](t, e.do("GET", "/api/v1/admin/users", e.admin, nil)) {
		if u["username"] == "alice" {
			aliceID = uint(u["id"].(float64))
		}
	}
	writeFile(t, filepath.Join(e.rw, "pub", "a.txt"), "hello")
	writeFile(t, filepath.Join(e.rw, "pub", "sub", "b.txt"), "nested")
	writeFile(t, filepath.Join(e.rw, "secret.txt"), "top secret")
	must(t, os.Symlink(filepath.Join(e.rw, "secret.txt"), filepath.Join(e.rw, "pub", "link.txt")))

	// Only folders, never the root; expiry must lie in the future.
	expect(t, e.do("POST", "/api/v1/shares", alice, obj{"path": "/可写/pub/a.txt"}), 400)
	expect(t, e.do("POST", "/api/v1/shares", alice, obj{"path": "/"}), 400)
	expect(t, e.do("POST", "/api/v1/shares", alice, obj{"path": "/可写/missing"}), 404)
	past := time.Now().Add(-time.Hour).Format(time.RFC3339)
	expect(t, e.do("POST", "/api/v1/shares", alice, obj{"path": "/可写/pub", "expires_at": past}), 400)
	rec := e.do("POST", "/api/v1/tokens", alice, obj{"name": "ro", "scopes": []string{"read"}})
	expect(t, rec, 201)
	roTok := decode[obj](t, rec)["token"].(string)
	expect(t, e.do("POST", "/api/v1/shares", roTok, obj{"path": "/可写/pub"}), 403)

	rec = e.do("POST", "/api/v1/shares", alice, obj{"path": "/可写/pub"})
	expect(t, rec, 201)
	sh := decode[obj](t, rec)
	id, tok := uint(sh["id"].(float64)), sh["token"].(string)
	if sh["name"] != "pub" || sh["status"] != "active" || sh["expires_at"] != nil || len(tok) < 20 {
		t.Fatalf("share %v", sh)
	}
	pub := "/api/v1/share/" + tok

	// Public, read-only access without a session.
	rec = e.do("GET", pub, "", nil)
	expect(t, rec, 200)
	if info := decode[obj](t, rec); info["name"] != "pub" || info["owner"] != "alice" {
		t.Fatalf("info %v", info)
	}
	rec = e.do("GET", pub+"/list", "", nil)
	expect(t, rec, 200)
	body := rec.Body.String()
	if strings.Contains(body, "可写") || strings.Contains(body, "link.txt") || !strings.Contains(body, `"/a.txt"`) {
		t.Fatalf("listing must be relative and skip links leaving the folder: %s", body)
	}
	for _, en := range decode[obj](t, rec)["entries"].([]any) {
		if en.(obj)["writable"] != false {
			t.Fatal("shared entries must be read-only")
		}
	}
	rec = e.do("GET", pub+"/list?path="+q("/sub"), "", nil)
	expect(t, rec, 200)
	if !strings.Contains(rec.Body.String(), `"/sub/b.txt"`) {
		t.Fatalf("sub listing %s", rec.Body.String())
	}
	rec = e.do("GET", pub+"/raw?path="+q("/sub/b.txt"), "", nil)
	expect(t, rec, 200)
	if rec.Body.String() != "nested" {
		t.Fatalf("raw %q", rec.Body.String())
	}
	expect(t, e.do("GET", pub+"/raw/b.txt?download=1&path="+q("/sub/b.txt"), "", nil), 200)
	rec = e.do("GET", pub+"/text?path="+q("/a.txt"), "", nil)
	expect(t, rec, 200)
	if decode[obj](t, rec)["content"] != "hello" {
		t.Fatal("text")
	}
	// Nothing outside the folder is reachable.
	for _, p := range []string{"/../secret.txt", "/link.txt", "../../可写/secret.txt"} {
		rec = e.do("GET", pub+"/raw?path="+q(p), "", nil)
		if rec.Code == 200 && strings.Contains(rec.Body.String(), "secret") {
			t.Fatalf("%s escaped the shared folder", p)
		}
	}
	expect(t, e.do("GET", "/api/v1/share/nonexistent-token/list", "", nil), 404)
	expect(t, e.do("PUT", "/api/v1/fs/file?path="+q("/pub/x.txt"), "", "x"), 401)

	// Expiry: an expired share answers 410 until its owner extends it.
	future := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	expect(t, e.do("PUT", fmt.Sprintf("/api/v1/shares/%d", id), alice, obj{"expires_at": past}), 400)
	rec = e.do("PUT", fmt.Sprintf("/api/v1/shares/%d", id), alice, obj{"expires_at": future.Format(time.RFC3339)})
	expect(t, rec, 200)
	if decode[obj](t, rec)["expires_at"] != future.Format(time.RFC3339) {
		t.Fatalf("expiry not saved: %s", rec.Body.String())
	}
	gone := time.Now().Add(-time.Minute)
	must(t, e.a.store.SetShareExpiry(aliceID, id, &gone))
	expect(t, e.do("GET", pub+"/list", "", nil), 410)
	if s := decode[[]obj](t, e.do("GET", "/api/v1/shares", alice, nil))[0]; s["status"] != "expired" {
		t.Fatalf("status %v", s)
	}
	expect(t, e.do("PUT", fmt.Sprintf("/api/v1/shares/%d", id), alice, obj{"expires_at": nil}), 200)
	expect(t, e.do("GET", pub+"/list", "", nil), 200)

	// Permissions follow the owner: revoking the bucket breaks the link.
	users := "/api/v1/admin/users"
	expect(t, e.do("PUT", fmt.Sprintf("%s/%d", users, aliceID), e.admin, obj{"username": "alice", "role": "user",
		"grants": e.grants(mountIn{Name: "只读", Path: e.ro})}), 200)
	expect(t, e.do("GET", pub+"/list", "", nil), 404)
	if s := decode[[]obj](t, e.do("GET", "/api/v1/shares", alice, nil))[0]; s["status"] != "unavailable" {
		t.Fatalf("status after revoke %v", s)
	}
	expect(t, e.do("PUT", fmt.Sprintf("%s/%d", users, aliceID), e.admin, obj{"username": "alice", "role": "user",
		"grants": e.grants(mountIn{Name: "只读", Path: e.ro}, mountIn{Name: "可写", Path: e.rw, Writable: true})}), 200)
	expect(t, e.do("GET", pub+"/list", "", nil), 200)
	// A read-only grant still allows reading.
	expect(t, e.do("PUT", fmt.Sprintf("%s/%d", users, aliceID), e.admin, obj{"username": "alice", "role": "user",
		"grants": e.grants(mountIn{Name: "可写", Path: e.rw})}), 200)
	expect(t, e.do("GET", pub+"/list", "", nil), 200)
	// Moving the folder away or disabling the owner breaks it too.
	must(t, os.Rename(filepath.Join(e.rw, "pub"), filepath.Join(e.rw, "pub2")))
	expect(t, e.do("GET", pub+"/list", "", nil), 404)
	must(t, os.Rename(filepath.Join(e.rw, "pub2"), filepath.Join(e.rw, "pub")))
	expect(t, e.do("PUT", fmt.Sprintf("%s/%d", users, aliceID), e.admin, obj{"username": "alice", "disabled": true}), 200)
	expect(t, e.do("GET", pub+"/list", "", nil), 404)
	expect(t, e.do("PUT", fmt.Sprintf("%s/%d", users, aliceID), e.admin, obj{"username": "alice", "disabled": false}), 200)
	alice = e.login("alice", "alice-pw")
	expect(t, e.do("GET", pub+"/list", "", nil), 200)

	// Other users cannot see or change it; deleting revokes it.
	e.createUser("bob", "bob-pw1", "user")
	bob := e.login("bob", "bob-pw1")
	if n := len(decode[[]obj](t, e.do("GET", "/api/v1/shares", bob, nil))); n != 0 {
		t.Fatalf("bob sees %d shares", n)
	}
	expect(t, e.do("PUT", fmt.Sprintf("/api/v1/shares/%d", id), bob, obj{"expires_at": nil}), 404)
	expect(t, e.do("DELETE", fmt.Sprintf("/api/v1/shares/%d", id), bob, nil), 404)
	expect(t, e.do("DELETE", fmt.Sprintf("/api/v1/shares/%d", id), alice, nil), 204)
	expect(t, e.do("GET", pub, "", nil), 404)
}

func TestDirSizesInListings(t *testing.T) {
	e := newEnv(t)
	alice := e.alice()
	writeFile(t, filepath.Join(e.rw, "sub", "a.txt"), "12345")
	writeFile(t, filepath.Join(e.rw, "sub", "deep", "b.txt"), "123")
	writeFile(t, filepath.Join(e.rw, "c.txt"), "1")

	sizes := func(p string) map[string]float64 {
		rec := e.do("GET", "/api/v1/fs/list?path="+q(p), alice, nil)
		expect(t, rec, 200)
		out := map[string]float64{}
		for _, x := range decode[obj](t, rec)["entries"].([]any) {
			m := x.(obj)
			out[m["name"].(string)] = m["size"].(float64)
		}
		return out
	}
	// Before the index is built the size is unknown.
	if got := sizes("/可写")["sub"]; got != -1 {
		t.Fatalf("unindexed dir size = %v, want -1", got)
	}
	e.a.search.Rebuild()
	if got := sizes("/可写")["sub"]; got != 8 {
		t.Fatalf("sub = %v, want 8", got)
	}
	if got := sizes("/")["可写"]; got != 9 {
		t.Fatalf("bucket root = %v, want 9", got)
	}
	// New files count once the index has them.
	expect(t, e.do("PUT", "/api/v1/fs/file?path="+q("/可写/sub/new.txt"), alice, "abcdefghij"), 201)
	e.a.search.Rebuild()
	if got := sizes("/可写")["sub"]; got != 18 {
		t.Fatalf("sub after upload = %v, want 18", got)
	}
}
