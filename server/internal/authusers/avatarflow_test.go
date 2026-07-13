// The avatar-flow integration test drives the profile feature end to end
// over real HTTP and a real Postgres: preset selection, photo upload with
// re-encode, the serving endpoint's caching contract, the admin moderation
// clear, and the audit trail - the docs/08 section 7 shape for each.
package authusers_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"macquiz/server/internal/authusers"
	"macquiz/server/internal/blobstore"
	"macquiz/server/internal/db"
	"macquiz/server/internal/httpserver"
)

func TestAvatarFlowE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := freshDatabase(t, ctx, baseURL, "macquiz_avatartest")
	if _, err := db.MigrateUp(ctx, sqlDB); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	blobDir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := authusers.NewService(sqlDB, "test-secret", log)
	svc.SetAvatarStore(blobstore.Local{Dir: blobDir, Ext: ".jpg"})
	handler := authusers.NewHandler(svc, false)
	router := httpserver.New(httpserver.BuildInfo{Version: "test"},
		httpserver.Deps{DB: sqlDB, Auth: handler})
	server := httptest.NewServer(router)
	defer server.Close()

	if err := svc.EnsureBootstrapAdmin(ctx, "admin@school.test", "admin-password-1", "Root Admin"); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	admin, _, _, err := svc.Login(ctx, "admin@school.test", "admin-password-1")
	if err != nil {
		t.Fatalf("admin login: %v", err)
	}
	student, initialPassword, err := svc.CreateUser(ctx, admin, "student", "stu@school.test", "Stu Dent")
	if err != nil {
		t.Fatalf("create student: %v", err)
	}

	_, _, adminCookies := call(t, server, "POST", "/api/v1/auth/login",
		map[string]string{"email": "admin@school.test", "password": "admin-password-1"}, nil)

	t.Run("avatar edits wait out the forced first-login reset", func(t *testing.T) {
		_, _, freshCookies := call(t, server, "POST", "/api/v1/auth/login",
			map[string]string{"email": "stu@school.test", "password": initialPassword}, nil)
		status, body, _ := call(t, server, "POST", "/api/v1/auth/me/avatar/preset",
			map[string]string{"preset": "boba"}, freshCookies)
		if status != 403 || body["code"] != "PASSWORD_CHANGE_REQUIRED" {
			t.Fatalf("preset before password change = %d %v, want 403 PASSWORD_CHANGE_REQUIRED", status, body)
		}
		status, _, _ = call(t, server, "POST", "/api/v1/auth/password",
			map[string]string{"current_password": initialPassword, "new_password": "student-password-1"}, freshCookies)
		if status != 204 {
			t.Fatalf("change password = %d, want 204", status)
		}
	})

	_, _, studentCookies := call(t, server, "POST", "/api/v1/auth/login",
		map[string]string{"email": "stu@school.test", "password": "student-password-1"}, nil)

	t.Run("preset selection round-trips", func(t *testing.T) {
		status, body, _ := call(t, server, "POST", "/api/v1/auth/me/avatar/preset",
			map[string]string{"preset": "boba"}, studentCookies)
		if status != 200 {
			t.Fatalf("select preset = %d %v, want 200", status, body)
		}
		if avatar := body["user"].(map[string]any)["avatar"]; avatar != "preset:boba" {
			t.Fatalf("avatar = %v, want preset:boba", avatar)
		}
		_, body, _ = call(t, server, "GET", "/api/v1/auth/me", nil, studentCookies)
		if avatar := body["user"].(map[string]any)["avatar"]; avatar != "preset:boba" {
			t.Fatalf("/me avatar = %v, want preset:boba", avatar)
		}
	})

	t.Run("unknown preset is rejected", func(t *testing.T) {
		status, body, _ := call(t, server, "POST", "/api/v1/auth/me/avatar/preset",
			map[string]string{"preset": "not-a-sticker"}, studentCookies)
		if status != 422 {
			t.Fatalf("unknown preset = %d %v, want 422", status, body)
		}
	})

	t.Run("preset avatar has no photo to serve", func(t *testing.T) {
		status, _ := rawGet(t, server, "/api/v1/users/"+student.ID+"/avatar", adminCookies, "")
		if status != 404 {
			t.Fatalf("GET avatar for preset user = %d, want 404", status)
		}
	})

	var photoETag string
	t.Run("upload re-encodes to a 256px JPEG and serves it", func(t *testing.T) {
		status, body := rawPut(t, server, "/api/v1/auth/me/avatar", testPhoto(t, 400, 200), studentCookies)
		if status != 200 {
			t.Fatalf("upload = %d %s, want 200", status, body)
		}
		if !strings.Contains(string(body), `"avatar":"upload:`) {
			t.Fatalf("upload response has no upload: avatar, got %s", body)
		}

		status, resp := rawGetFull(t, server, "/api/v1/users/"+student.ID+"/avatar", studentCookies, "")
		if status != 200 {
			t.Fatalf("GET avatar = %d, want 200", status)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "image/jpeg" {
			t.Fatalf("avatar Content-Type = %q, want image/jpeg", ct)
		}
		photoETag = resp.Header.Get("ETag")
		if photoETag == "" {
			t.Fatal("avatar response has no ETag")
		}
		img, err := jpeg.Decode(bytes.NewReader(resp.body))
		if err != nil {
			t.Fatalf("served avatar is not a JPEG: %v", err)
		}
		if b := img.Bounds(); b.Dx() != 256 || b.Dy() != 256 {
			t.Fatalf("served avatar is %dx%d, want 256x256", b.Dx(), b.Dy())
		}
	})

	t.Run("If-None-Match returns 304", func(t *testing.T) {
		status, _ := rawGet(t, server, "/api/v1/users/"+student.ID+"/avatar", studentCookies, photoETag)
		if status != 304 {
			t.Fatalf("conditional GET = %d, want 304", status)
		}
	})

	t.Run("avatar fetch requires authentication", func(t *testing.T) {
		status, _ := rawGet(t, server, "/api/v1/users/"+student.ID+"/avatar", nil, "")
		if status != 401 {
			t.Fatalf("unauthenticated avatar GET = %d, want 401", status)
		}
	})

	t.Run("oversized upload is rejected", func(t *testing.T) {
		status, body := rawPut(t, server, "/api/v1/auth/me/avatar", make([]byte, 3<<20), studentCookies)
		if status != 422 || !strings.Contains(string(body), "2 MB") {
			t.Fatalf("3MB upload = %d %s, want 422 mentioning the 2 MB cap", status, body)
		}
	})

	t.Run("non-image upload is rejected", func(t *testing.T) {
		status, body := rawPut(t, server, "/api/v1/auth/me/avatar", []byte("definitely not pixels"), studentCookies)
		if status != 422 {
			t.Fatalf("garbage upload = %d %s, want 422", status, body)
		}
	})

	t.Run("switching back to a preset deletes the stored photo", func(t *testing.T) {
		status, _, _ := call(t, server, "POST", "/api/v1/auth/me/avatar/preset",
			map[string]string{"preset": "rocket"}, studentCookies)
		if status != 200 {
			t.Fatalf("preset switch = %d, want 200", status)
		}
		status, _ = rawGet(t, server, "/api/v1/users/"+student.ID+"/avatar", studentCookies, "")
		if status != 404 {
			t.Fatalf("GET avatar after preset switch = %d, want 404", status)
		}
		entries, err := os.ReadDir(blobDir)
		if err != nil {
			t.Fatalf("read blob dir: %v", err)
		}
		if len(entries) != 0 {
			t.Fatalf("blob dir still holds %d files after the upload was replaced", len(entries))
		}
	})

	t.Run("admin list carries the avatar", func(t *testing.T) {
		status, body, _ := call(t, server, "GET", "/api/v1/users?role=student", nil, adminCookies)
		if status != 200 {
			t.Fatalf("list users = %d, want 200", status)
		}
		users := body["users"].([]any)
		if len(users) != 1 || users[0].(map[string]any)["avatar"] != "preset:rocket" {
			t.Fatalf("listed student avatar = %v, want preset:rocket", users)
		}
	})

	t.Run("admin clears an avatar via PATCH", func(t *testing.T) {
		status, body, _ := call(t, server, "PATCH", "/api/v1/users/"+student.ID,
			map[string]any{"clear_avatar": true}, adminCookies)
		if status != 200 {
			t.Fatalf("admin clear = %d %v, want 200", status, body)
		}
		if avatar, present := body["user"].(map[string]any)["avatar"]; present && avatar != nil {
			t.Fatalf("avatar after admin clear = %v, want absent", avatar)
		}
		var audits int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM audit_log WHERE action = 'users.updated'
			   AND detail->'changes' ? 'avatar'`).Scan(&audits); err != nil || audits != 1 {
			t.Fatalf("admin avatar-clear audit rows = %d (err %v), want 1", audits, err)
		}
	})

	t.Run("self delete reverts to initials", func(t *testing.T) {
		if _, err := svc.SelectAvatarPreset(ctx, student.ID, "dino"); err != nil {
			t.Fatalf("re-set preset: %v", err)
		}
		status, body := rawDelete(t, server, "/api/v1/auth/me/avatar", studentCookies)
		if status != 200 || strings.Contains(string(body), `"avatar"`) {
			t.Fatalf("self delete = %d %s, want 200 with no avatar field", status, body)
		}
	})

	t.Run("every avatar mutation left an audit row", func(t *testing.T) {
		var audits int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM audit_log WHERE action = 'profile.updated'
			   AND resource_type = 'user' AND actor_id = $1`, student.ID).Scan(&audits); err != nil {
			t.Fatalf("count profile audits: %v", err)
		}
		// boba, upload, rocket, dino, clear - the rejected mutations wrote none.
		if audits != 5 {
			t.Fatalf("profile.updated audit rows = %d, want 5", audits)
		}
	})
}

// testPhoto renders a w x h PNG so the upload pipeline has real pixels.
func testPhoto(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 120, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test photo: %v", err)
	}
	return buf.Bytes()
}

type rawResponse struct {
	Header http.Header
	body   []byte
}

// rawRequest performs one non-JSON request with explicit cookie control.
func rawRequest(t *testing.T, server *httptest.Server, method, path string, body []byte, cookies map[string]string, ifNoneMatch string) (int, rawResponse) {
	t.Helper()
	req, err := http.NewRequest(method, server.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for name, value := range cookies {
		req.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, rawResponse{Header: resp.Header, body: got}
}

func rawPut(t *testing.T, server *httptest.Server, path string, body []byte, cookies map[string]string) (int, []byte) {
	status, resp := rawRequest(t, server, "PUT", path, body, cookies, "")
	return status, resp.body
}

func rawDelete(t *testing.T, server *httptest.Server, path string, cookies map[string]string) (int, []byte) {
	status, resp := rawRequest(t, server, "DELETE", path, nil, cookies, "")
	return status, resp.body
}

func rawGet(t *testing.T, server *httptest.Server, path string, cookies map[string]string, ifNoneMatch string) (int, []byte) {
	status, resp := rawRequest(t, server, "GET", path, nil, cookies, ifNoneMatch)
	return status, resp.body
}

func rawGetFull(t *testing.T, server *httptest.Server, path string, cookies map[string]string, ifNoneMatch string) (int, rawResponse) {
	return rawRequest(t, server, "GET", path, nil, cookies, ifNoneMatch)
}
