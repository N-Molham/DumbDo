// DumbDo server — Go rewrite of the original Express server.
// Zero third-party dependencies: static file serving behind a PIN cookie,
// CORS origin validation, brute-force lockout, and atomic todo persistence.
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/big"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Environment
var (
	port       = envOr("PORT", "3000")
	pin        = os.Getenv("DUMBDO_PIN")
	siteTitle  = envOr("DUMBDO_SITE_TITLE", "DumbDo")
	singleList = os.Getenv("SINGLE_LIST") == "true"
	// Original Express code treats an unset NODE_ENV as "not production"
	nodeEnv = os.Getenv("NODE_ENV")

	publicDir = "public"
	dataDir   = "data"
	dataFile  = filepath.Join(dataDir, "todos.json")
)

const (
	minPinLength = 4
	maxPinLength = 10
	maxAttempts  = 5
	lockoutTime  = 15 * time.Minute
	maxBodyBytes = 100 << 10 // express.json() default limit
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ---------- CORS / origin validation (parity with scripts/cors.js) ----------

var (
	allowedOrigins []string
	allOrigins     bool
)

func setupOrigins() {
	nodeEnvForCors := nodeEnv
	if nodeEnvForCors == "" {
		nodeEnvForCors = "production" // cors.js default
	}
	allowed := os.Getenv("ALLOWED_ORIGINS")
	switch {
	case nodeEnvForCors == "development" || allowed == "*":
		allOrigins = true
	case allowed != "":
		for _, origin := range strings.Split(allowed, ",") {
			origin = strings.TrimSpace(origin)
			if origin == "" {
				continue
			}
			allowedOrigins = append(allowedOrigins, normalizeOrigin(origin))
		}
	}
	if allOrigins {
		log.Println("ALLOWED ORIGINS: *")
	} else {
		log.Println("ALLOWED ORIGINS:", allowedOrigins)
	}
}

func normalizeOrigin(origin string) string {
	// Mirrors new URL(origin).origin — scheme://host[:port]
	if origin == "" {
		return origin
	}
	u, err := urlOrigin(origin)
	if err != nil {
		log.Printf("Error parsing origin %q: %v", origin, err)
		return origin
	}
	return u
}

func urlOrigin(origin string) (string, error) {
	s := origin
	schemeSep := strings.Index(s, "://")
	if schemeSep <= 0 {
		return "", fmt.Errorf("invalid URL")
	}
	scheme := s[:schemeSep]
	rest := s[schemeSep+3:]
	host := rest
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		host = rest[:i]
	}
	if host == "" {
		return "", fmt.Errorf("invalid URL")
	}
	return scheme + "://" + host, nil
}

// corsMiddleware mirrors the express cors() package for the options used:
// origin from config, credentials true, fixed method/header list.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := "*"
		if !allOrigins {
			origin = strings.Join(allowedOrigins, ", ") // cors pkg echoes configured origins
			if len(allowedOrigins) > 0 {
				origin = allowedOrigins[0]
			}
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")

		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Authorization")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originValidationMiddleware validates the Referer header on /api routes
// (parity with the original implementation, which reads the referer).
func originValidationMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		referer := r.Header.Get("Referer")
		if !validateOrigin(referer) {
			log.Printf("Blocked request from origin: %s", referer)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"Forbidden"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func validateOrigin(origin string) bool {
	if allOrigins {
		return true
	}
	if origin == "" {
		log.Println("No origin to validate.")
		return false
	}
	normalized := normalizeOrigin(origin)
	for _, allowed := range allowedOrigins {
		if normalized == allowed {
			return true
		}
	}
	log.Printf("Blocked request from origin: %s", origin)
	return false
}

// ---------- Brute force lockout ----------

type attempt struct {
	count       int
	lastAttempt time.Time
}

type lockoutMap struct {
	mu       sync.Mutex
	attempts map[string]*attempt
}

var lockouts = &lockoutMap{attempts: map[string]*attempt{}}

func (l *lockoutMap) reset(ip string) {
	l.mu.Lock()
	delete(l.attempts, ip)
	l.mu.Unlock()
}

func (l *lockoutMap) isLockedOut(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.attempts[ip]
	if !ok {
		return false
	}
	if a.count >= maxAttempts && time.Since(a.lastAttempt) < lockoutTime {
		return true
	}
	if time.Since(a.lastAttempt) >= lockoutTime {
		delete(l.attempts, ip)
	}
	return false
}

func (l *lockoutMap) record(ip string) (count, attemptsLeft int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.attempts[ip]
	if a == nil {
		a = &attempt{}
		l.attempts[ip] = a
	}
	a.count++
	a.lastAttempt = time.Now()
	return a.count, maxAttempts - a.count
}

func (l *lockoutMap) attemptsInfo(ip string) (attemptsLeft, lockoutMinutes int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.attempts[ip]
	if !ok {
		return maxAttempts, 0
	}
	left := maxAttempts - a.count
	minutes := 0
	if left <= 0 {
		minutes = int((lockoutTime - time.Since(a.lastAttempt) + time.Minute - 1) / time.Minute)
		if minutes < 0 {
			minutes = 0
		}
	}
	return left, minutes
}

func cleanupLockouts() {
	ticker := time.NewTicker(time.Minute)
	for range ticker.C {
		lockouts.mu.Lock()
		now := time.Now()
		for ip, a := range lockouts.attempts {
			if now.Sub(a.lastAttempt) >= lockoutTime {
				delete(lockouts.attempts, ip)
			}
		}
		lockouts.mu.Unlock()
	}
}

// ---------- PIN helpers ----------

func secureCompare(a, b string) bool {
	pad := func(s string) []byte {
		b := make([]byte, maxPinLength)
		copy(b, s)
		for i := len(s); i < maxPinLength; i++ {
			b[i] = '0'
		}
		return b
	}
	return subtle.ConstantTimeCompare(pad(a), pad(b)) == 1
}

func isValidPin(provided string) bool {
	return pin == "" || (provided != "" && secureCompare(provided, pin))
}

func randomDelay() {
	n, err := rand.Int(rand.Reader, big.NewInt(100)) // 50..149 ms, parity with randomInt(50,150)
	if err != nil {
		n = big.NewInt(50)
	}
	time.Sleep(time.Duration(50+n.Int64()) * time.Millisecond)
}

// clientIP honours the original `app.set('trust proxy', 1)`: first hop of
// X-Forwarded-For, else the socket address.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		parts := strings.Split(fwd, ",")
		return strings.TrimSpace(parts[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------- Persistence ----------

var (
	todosMu sync.RWMutex
	todos   json.RawMessage // in-memory cache of data/todos.json
)

func atomicWriteTodos(data []byte) error {
	tmp := dataFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dataFile)
}

func initDataFile() error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	raw, err := os.ReadFile(dataFile)
	if err != nil {
		raw = []byte("{}")
		if err := atomicWriteTodos(raw); err != nil {
			return err
		}
	}
	todos = raw
	log.Println("Todo list stored at:", dataFile)
	return nil
}

// ---------- PWA manifests (parity with scripts/pwa-manifest-generator.js) ----------

type pwaIcon struct {
	Src   string `json:"src"`
	Type  string `json:"type"`
	Sizes string `json:"sizes"`
}

type pwaManifest struct {
	Name            string    `json:"name"`
	ShortName       string    `json:"short_name"`
	Description     string    `json:"description"`
	StartURL        string    `json:"start_url"`
	Display         string    `json:"display"`
	BackgroundColor string    `json:"background_color"`
	ThemeColor      string    `json:"theme_color"`
	Icons           []pwaIcon `json:"icons"`
	Orientation     string    `json:"orientation"`
}

var (
	assetManifestJSON []byte
	pwaManifestJSON   []byte
)

func buildManifests() {
	var assets []string
	filepath.WalkDir(publicDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(publicDir, path)
		if err != nil {
			return nil
		}
		assets = append(assets, "/"+filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(assets)
	// Keep files first, directories' generated files included; parity with the
	// original recursive readdir (unsorted). Sort for deterministic caching.
	var err error
	if assetManifestJSON, err = json.MarshalIndent(assets, "", "  "); err != nil {
		log.Println("asset manifest error:", err)
	}
	manifest := pwaManifest{
		Name:            siteTitle,
		ShortName:       siteTitle,
		Description:     "A stupidly simple todo list",
		StartURL:        "/",
		Display:         "standalone",
		BackgroundColor: "#ffffff",
		ThemeColor:      "#000000",
		Icons: []pwaIcon{
			{Src: "assets/logo-192.png", Type: "image/png", Sizes: "192x192"},
			{Src: "assets/logo-512.png", Type: "image/png", Sizes: "512x512"},
		},
		Orientation: "any",
	}
	if pwaManifestJSON, err = json.MarshalIndent(manifest, "", "  "); err != nil {
		log.Println("pwa manifest error:", err)
	}
}

// ---------- HTTP helpers ----------

func jsonBody(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(body)
}

func jsonValue(w http.ResponseWriter, status int, v any) {
	body, _ := json.Marshal(v)
	jsonBody(w, status, body)
}

func sendFile(w http.ResponseWriter, path string, cacheMaxAge int) {
	data, err := os.ReadFile(path)
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	if cacheMaxAge > 0 {
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", cacheMaxAge))
	}
	if ct := mime.TypeByExtension(filepath.Ext(path)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Write(data)
}

// ---------- Handlers ----------

func handlePinRequired(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	locked := lockouts.isLockedOut(ip)
	attemptsLeft, lockoutMinutes := lockouts.attemptsInfo(ip)
	jsonValue(w, http.StatusOK, map[string]any{
		"required":       pin != "",
		"length":         len(pin),
		"locked":         locked,
		"attemptsLeft":   max(0, attemptsLeft),
		"lockoutMinutes": lockoutMinutes,
	})
}

func handleVerifyPin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if lockouts.isLockedOut(ip) {
		_, lockoutMinutes := lockouts.attemptsInfo(ip)
		jsonValue(w, http.StatusTooManyRequests, map[string]any{
			"error":          fmt.Sprintf("Too many attempts. Please try again in %d minutes.", lockoutMinutes),
			"locked":         true,
			"lockoutMinutes": lockoutMinutes,
		})
		return
	}

	var body struct {
		Pin string `json:"pin"`
	}
	raw, _ := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err := json.Unmarshal(raw, &body); err != nil {
		body.Pin = ""
	}

	if pin != "" && (len(body.Pin) < minPinLength || len(body.Pin) > maxPinLength) {
		_, attemptsLeft := lockouts.record(ip)
		jsonValue(w, http.StatusUnauthorized, map[string]any{
			"valid":        false,
			"error":        fmt.Sprintf("PIN must be between %d and %d digits", minPinLength, maxPinLength),
			"attemptsLeft": attemptsLeft,
		})
		return
	}

	randomDelay()

	if pin == "" || secureCompare(body.Pin, pin) {
		lockouts.reset(ip)
		http.SetCookie(w, &http.Cookie{
			Name:     "DUMBDO_PIN",
			Value:    body.Pin,
			Path:     "/",
			HttpOnly: true,
			Secure:   nodeEnv == "production", // parity with original behaviour
			SameSite: http.SameSiteStrictMode,
		})
		jsonValue(w, http.StatusOK, map[string]any{"valid": true})
		return
	}

	_, attemptsLeft := lockouts.record(ip)
	jsonValue(w, http.StatusUnauthorized, map[string]any{
		"valid":        false,
		"error":        fmt.Sprintf("Invalid PIN. %d attempts remaining before lockout.", attemptsLeft),
		"attemptsLeft": attemptsLeft,
	})
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	jsonValue(w, http.StatusOK, map[string]any{
		"siteTitle":  siteTitle,
		"singleList": singleList,
	})
}

// pinMiddleware protects everything registered after it.
func pinMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := ""
		if c, err := r.Cookie("DUMBDO_PIN"); err == nil {
			provided = c.Value
		}
		if provided == "" {
			provided = r.Header.Get("X-Pin")
		}
		if isValidPin(provided) {
			next.ServeHTTP(w, r)
			return
		}

		isAPI := strings.HasPrefix(r.URL.Path, "/api/")
		// Original: req.xhr (X-Requested-With header) or /api path → JSON 401
		if isAPI || r.Header.Get("X-Requested-With") != "" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"Invalid PIN"}`))
			return
		}
		if r.URL.Path != "/login" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	sendFile(w, filepath.Join(publicDir, "index.html"), 0)
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	provided := ""
	if c, err := r.Cookie("DUMBDO_PIN"); err == nil {
		provided = c.Value
	}
	if provided == "" {
		provided = r.Header.Get("X-Pin")
	}
	if isValidPin(provided) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	sendFile(w, filepath.Join(publicDir, "login.html"), 0)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	jsonValue(w, http.StatusOK, map[string]any{"ok": true})
}

// protectedStatic serves everything else under public/ (parity with
// express.static, with the 1h max-age the original runtime added).
func protectedStatic(w http.ResponseWriter, r *http.Request) {
	upath := r.URL.Path
	if strings.HasSuffix(upath, "/") {
		http.NotFound(w, r)
		return
	}
	if strings.Contains(upath, "..") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.ServeFile(w, r, filepath.Join(publicDir, filepath.Clean(upath)))
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

func handleGetTodos(w http.ResponseWriter, r *http.Request) {
	todosMu.RLock()
	defer todosMu.RUnlock()
	jsonBody(w, http.StatusOK, todos)
}

func handlePostTodos(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		jsonValue(w, http.StatusBadRequest, map[string]any{"error": "Invalid todos payload"})
		return
	}
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		jsonValue(w, http.StatusBadRequest, map[string]any{"error": "Invalid todos payload"})
		return
	}
	obj, ok := decoded.(map[string]any)
	if !ok || obj == nil {
		jsonValue(w, http.StatusBadRequest, map[string]any{"error": "Invalid todos payload"})
		return
	}

	pretty, _ := json.MarshalIndent(obj, "", "  ")
	todosMu.Lock()
	if err := atomicWriteTodos(pretty); err != nil {
		todosMu.Unlock()
		jsonValue(w, http.StatusInternalServerError, map[string]any{"error": "Failed to save todos"})
		return
	}
	todos = pretty
	todosMu.Unlock()
	jsonValue(w, http.StatusOK, map[string]any{"success": true})
}

// ---------- Router ----------

// apiHandler dispatches the three public API endpoints itself and funnels
// everything else (/api/todos) through the PIN check, mirroring the
// registration order of the original Express server.
func apiHandler() http.Handler {
	protected := pinMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/todos":
			handleGetTodos(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/api/todos":
			handlePostTodos(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/pin-required":
			handlePinRequired(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/api/verify-pin":
			handleVerifyPin(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/api/config":
			handleConfig(w, r)
		default:
			protected.ServeHTTP(w, r)
		}
	})
}

func route(mux *http.ServeMux) http.Handler {
	mux.HandleFunc("GET /health", handleHealth)

	// Public API + assets (before PIN middleware, parity with original order)
	mux.Handle("/api/", corsMiddleware(originValidationMiddleware(apiHandler())))

	mux.HandleFunc("GET /login.js", func(w http.ResponseWriter, r *http.Request) { sendFile(w, filepath.Join(publicDir, "login.js"), 0) })
	mux.HandleFunc("GET /styles.css", func(w http.ResponseWriter, r *http.Request) {
		sendFile(w, filepath.Join(publicDir, "assets/styles.css"), 0)
	})
	mux.HandleFunc("GET /favicon.svg", func(w http.ResponseWriter, r *http.Request) {
		sendFile(w, filepath.Join(publicDir, "assets/favicon.svg"), 0)
	})
	mux.HandleFunc("GET /asset-manifest.json", func(w http.ResponseWriter, r *http.Request) { jsonBody(w, http.StatusOK, assetManifestJSON) })
	mux.HandleFunc("GET /manifest.json", func(w http.ResponseWriter, r *http.Request) { jsonBody(w, http.StatusOK, pwaManifestJSON) })

	// Protected: index, login page, todos API, static files
	protected := pinMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			handleIndex(w, r)
		case r.URL.Path == "/login":
			handleLogin(w, r)
		default:
			protectedStatic(w, r)
		}
	}))
	mux.Handle("/", corsMiddleware(protected))

	return corsMiddleware(mux)
}

func main() {
	log.SetFlags(log.LstdFlags)
	setupOrigins()
	buildManifests()
	if err := initDataFile(); err != nil {
		log.Fatal("init data:", err)
	}
	go cleanupLockouts()

	mux := http.NewServeMux()
	handler := route(mux)

	log.Printf("DumbDo server running at http://localhost:%s", port)
	log.Println("PIN protection:", map[bool]string{true: "enabled", false: "disabled"}[pin != ""])

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(server.ListenAndServe())
}
