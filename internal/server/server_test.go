package server

import (
	"crypto/tls"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"
)

func testApp(t *testing.T) *App {
	t.Helper()
	app, err := New(Config{
		Addr: "127.0.0.1:0", Token: "test-access-token", Version: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func TestAuthenticationAndCapabilities(t *testing.T) {
	app := testApp(t)
	unauthorized := httptest.NewRecorder()
	app.router.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/capabilities", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated API status = %d", unauthorized.Code)
	}

	badLogin := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"token":"wrong"}`))
	badLogin.Header.Set("Content-Type", "application/json")
	badResult := httptest.NewRecorder()
	app.router.ServeHTTP(badResult, badLogin)
	if badResult.Code != http.StatusUnauthorized {
		t.Fatalf("invalid login status = %d", badResult.Code)
	}

	login := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"token":"test-access-token"}`))
	login.Header.Set("Content-Type", "application/json")
	loginResult := httptest.NewRecorder()
	app.router.ServeHTTP(loginResult, login)
	if loginResult.Code != http.StatusOK {
		t.Fatalf("valid login status = %d", loginResult.Code)
	}
	cookies := loginResult.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login returned %d cookies, want 1", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != "remotemac_session" || cookie.Value != sessionCookieValue("test-access-token") || !cookie.HttpOnly || cookie.Secure || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie attributes = %+v", cookie)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/capabilities", nil)
	request.AddCookie(cookie)
	result := httptest.NewRecorder()
	app.router.ServeHTTP(result, request)
	if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), `"codec":"H264"`) || !strings.Contains(result.Body.String(), `"version":"test"`) {
		t.Fatalf("authenticated capabilities response: status=%d body=%s", result.Code, result.Body.String())
	}
}

func TestAuthenticationSupportsCookieUnsafeTokenCharacters(t *testing.T) {
	app, err := New(Config{Addr: "127.0.0.1:0", Token: "token with spaces; punctuation", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"token":"token with spaces; punctuation"}`))
	login.Header.Set("Content-Type", "application/json")
	loginResult := httptest.NewRecorder()
	app.router.ServeHTTP(loginResult, login)
	if loginResult.Code != http.StatusOK {
		t.Fatalf("valid login status = %d body=%s", loginResult.Code, loginResult.Body.String())
	}
	cookies := loginResult.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value != sessionCookieValue("token with spaces; punctuation") {
		t.Fatalf("session cookie = %+v", cookies)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/capabilities", nil)
	request.AddCookie(cookies[0])
	result := httptest.NewRecorder()
	app.router.ServeHTTP(result, request)
	if result.Code != http.StatusOK {
		t.Fatalf("authenticated capabilities status = %d body=%s", result.Code, result.Body.String())
	}
}

func TestHealthIsPublicAndWebSocketRequiresExactHTTPOrigin(t *testing.T) {
	app := testApp(t)
	health := httptest.NewRecorder()
	app.router.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d", health.Code)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/rtc", nil)
	request.Host = "mac.example:8443"
	request.Header.Set("Origin", "http://mac.example:8443")
	if !sameOriginWebSocket(request) {
		t.Fatal("same-origin HTTP WebSocket request was rejected")
	}
	request.Header.Set("Origin", "https://mac.example:8443")
	if sameOriginWebSocket(request) {
		t.Fatal("HTTPS WebSocket origin was accepted by the HTTP service")
	}
	request.Header.Set("Origin", "http://evil.example")
	if sameOriginWebSocket(request) {
		t.Fatal("cross-origin WebSocket request was accepted")
	}
	request.TLS = &tls.ConnectionState{}
	request.Header.Set("Origin", "http://mac.example:8443")
	if sameOriginWebSocket(request) {
		t.Fatal("TLS WebSocket request was accepted by the HTTP-only service")
	}
}

func TestTestNetworkShaperIsOptInAndAuthenticated(t *testing.T) {
	requestBody := strings.NewReader(`{"bitrate":500000}`)
	disabled := testApp(t)
	disabledResult := httptest.NewRecorder()
	disabled.router.ServeHTTP(disabledResult, httptest.NewRequest(http.MethodPost, "/api/diagnostics/test-network-rate", requestBody))
	if disabledResult.Code != http.StatusNotFound {
		t.Fatalf("test shaper route without opt-in status = %d, want 404", disabledResult.Code)
	}

	app, err := New(Config{
		Addr: "127.0.0.1:0", Token: "test-access-token", EnableTestNetworkShaper: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	unauthorized := httptest.NewRecorder()
	app.router.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/api/diagnostics/test-network-rate", strings.NewReader(`{"bitrate":500000}`)))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated test shaper status = %d, want 401", unauthorized.Code)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/diagnostics/test-network-rate", strings.NewReader(`{"bitrate":500000}`))
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(&http.Cookie{Name: "remotemac_session", Value: sessionCookieValue("test-access-token")})
	result := httptest.NewRecorder()
	app.router.ServeHTTP(result, request)
	if result.Code != http.StatusOK {
		t.Fatalf("authenticated test shaper status = %d body=%s", result.Code, result.Body.String())
	}
	var body struct {
		Bitrate int  `json:"bitrate"`
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(result.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Bitrate != 500_000 || !body.Enabled || app.testShaper.RateBPS() != 500_000 {
		t.Fatalf("test shaper response=%+v rate=%d", body, app.testShaper.RateBPS())
	}
}

func TestEmbeddedAssetsServeTheirContents(t *testing.T) {
	app := testApp(t)
	entries, err := fs.ReadDir(staticFiles, "static/assets")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{".js": false, ".css": false}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		extension := path.Ext(entry.Name())
		expectedType, relevant := map[string]string{
			".js":  "javascript",
			".css": "text/css",
		}[extension]
		if !relevant {
			continue
		}
		assetPath := path.Join("static/assets", entry.Name())
		expectedBody, err := fs.ReadFile(staticFiles, assetPath)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodGet, "/assets/"+entry.Name(), nil)
		result := httptest.NewRecorder()
		app.router.ServeHTTP(result, request)
		if result.Code != http.StatusOK {
			t.Errorf("%s status = %d", entry.Name(), result.Code)
			continue
		}
		if !strings.Contains(result.Header().Get("Content-Type"), expectedType) {
			t.Errorf("%s Content-Type = %q, want %q", entry.Name(), result.Header().Get("Content-Type"), expectedType)
		}
		if result.Body.String() != string(expectedBody) {
			t.Errorf("%s did not serve the embedded asset contents", entry.Name())
		}
		found[extension] = true
	}
	for extension, present := range found {
		if !present {
			t.Errorf("embedded asset %s was not tested", extension)
		}
	}
	missing := httptest.NewRecorder()
	app.router.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/assets/missing.js", nil))
	if missing.Code != http.StatusNotFound {
		t.Errorf("missing embedded asset status = %d, want %d", missing.Code, http.StatusNotFound)
	}
}

func TestRejectEmptyToken(t *testing.T) {
	for _, token := range []string{"", "  "} {
		if _, err := New(Config{Token: token}); err == nil {
			t.Fatal("accepted empty token")
		}
	}
}

func TestLoginBodyLimit(t *testing.T) {
	app := testApp(t)
	request := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"token":"`+strings.Repeat("x", 8192)+`"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	app.router.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", response.Code)
	}
	if len(response.Result().Cookies()) != 0 {
		t.Fatal("oversized login received cookie")
	}
}
