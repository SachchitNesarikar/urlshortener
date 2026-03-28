package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-redis/redis/v8"
	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
)

type User struct {
	ID        int64     `json:"id"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

type URL struct {
	ID          int64      `json:"id"`
	UserID      int64      `json:"user_id"`
	ShortCode   string     `json:"short_code"`
	OriginalURL string     `json:"original_url"`
	Clicks      int64      `json:"clicks"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

type AnalyticsRecord struct {
	ID         int64     `json:"id"`
	ShortCode  string    `json:"short_code"`
	IPAddress  string    `json:"ip_address"`
	UserAgent  string    `json:"user_agent"`
	Referrer   string    `json:"referrer"`
	DeviceType string    `json:"device_type"`
	Browser    string    `json:"browser"`
	ClickedAt  time.Time `json:"clicked_at"`
}

type ClicksByDay struct {
	Date   string `json:"date"`
	Clicks int64  `json:"clicks"`
}

type ReferrerCount struct {
	Referrer string `json:"referrer"`
	Count    int64  `json:"count"`
}

type KVCount struct {
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

type AnalyticsSummary struct {
	TotalClicks      int64             `json:"total_clicks"`
	ClicksByDay      []ClicksByDay     `json:"clicks_by_day"`
	TopReferrers     []ReferrerCount   `json:"top_referrers"`
	DeviceBreakdown  []KVCount         `json:"device_breakdown"`
	BrowserBreakdown []KVCount         `json:"browser_breakdown"`
	RecentClicks     []AnalyticsRecord `json:"recent_clicks"`
}

type ShortenRequest struct {
	URL        string `json:"url"`
	CustomSlug string `json:"custom_slug,omitempty"`
	ExpiresIn  *int   `json:"expires_in_hours,omitempty"`
}

type ShortenResponse struct {
	ShortCode   string     `json:"short_code"`
	ShortURL    string     `json:"short_url"`
	OriginalURL string     `json:"original_url"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

type AuthRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

const base62Chars = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

var validSlugRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{3,32}$`)

var reservedSlugs = map[string]bool{
	"api": true, "static": true, "login": true, "register": true,
	"logout": true, "dashboard": true, "admin": true, "health": true,
}

func encodeBase62(n int64) string {
	if n == 0 {
		return "0"
	}
	var sb strings.Builder
	for n > 0 {
		sb.WriteByte(base62Chars[n%62])
		n /= 62
	}
	b := []byte(sb.String())
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64
	cap     float64
}

func newRateLimiter(rps, burst float64) *RateLimiter {
	rl := &RateLimiter{buckets: make(map[string]*bucket), rate: rps, cap: burst}
	go func() {
		for range time.Tick(5 * time.Minute) {
			rl.mu.Lock()
			cutoff := time.Now().Add(-10 * time.Minute)
			for ip, b := range rl.buckets {
				if b.lastSeen.Before(cutoff) {
					delete(rl.buckets, ip)
				}
			}
			rl.mu.Unlock()
		}
	}()
	return rl
}

func (rl *RateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	b, ok := rl.buckets[ip]
	if !ok {
		rl.buckets[ip] = &bucket{tokens: rl.cap - 1, lastSeen: now}
		return true
	}
	elapsed := now.Sub(b.lastSeen).Seconds()
	b.tokens += elapsed * rl.rate
	if b.tokens > rl.cap {
		b.tokens = rl.cap
	}
	b.lastSeen = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func detectDevice(ua string) string {
	u := strings.ToLower(ua)
	if strings.Contains(u, "mobile") || strings.Contains(u, "iphone") || strings.Contains(u, "android") {
		return "Mobile"
	}
	if strings.Contains(u, "ipad") || strings.Contains(u, "tablet") {
		return "Tablet"
	}
	return "Desktop"
}

func detectBrowser(ua string) string {
	u := strings.ToLower(ua)
	switch {
	case strings.Contains(u, "edg/"):
		return "Edge"
	case strings.Contains(u, "opr/") || strings.Contains(u, "opera"):
		return "Opera"
	case strings.Contains(u, "chrome") && !strings.Contains(u, "chromium"):
		return "Chrome"
	case strings.Contains(u, "firefox"):
		return "Firefox"
	case strings.Contains(u, "safari") && !strings.Contains(u, "chrome"):
		return "Safari"
	default:
		return "Other"
	}
}

const sessionCookie = "sess"
const sessionTTL = 7 * 24 * time.Hour

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

type App struct {
	db      *sql.DB
	redis   *redis.Client
	ctx     context.Context
	limiter *RateLimiter
}

func newApp() *App {
	_ = godotenv.Load()

	dsn := fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		getenv("DB_HOST", "localhost"),
		getenv("DB_PORT", "5432"),
		getenv("DB_USER", "postgres"),
		getenv("DB_PASSWORD", "postgres"),
		getenv("DB_NAME", "urlshortener"),
	)
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatalf("postgres open: %v", err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)
	if err := db.Ping(); err != nil {
		log.Fatalf("postgres ping: %v", err)
	}
	log.Println("Connected to PostgreSQL")

	rdb := redis.NewClient(&redis.Options{
		Addr:     fmt.Sprintf("%s:%s", getenv("REDIS_HOST", "localhost"), getenv("REDIS_PORT", "6379")),
		Password: getenv("REDIS_PASSWORD", ""),
		DB:       0,
	})
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Printf("Redis unavailable (%v) — running without cache", err)
		rdb = nil
	} else {
		log.Println("Connected to Redis")
	}

	app := &App{
		db:      db,
		redis:   rdb,
		ctx:     ctx,
		limiter: newRateLimiter(10, 30),
	}
	app.migrate()
	return app
}

func (a *App) migrate() {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id            BIGSERIAL PRIMARY KEY,
			email         VARCHAR(255) UNIQUE NOT NULL,
			password_hash TEXT NOT NULL,
			created_at    TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			token      VARCHAR(64) PRIMARY KEY,
			user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			expires_at TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS urls (
			id           BIGSERIAL PRIMARY KEY,
			user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			short_code   VARCHAR(32) UNIQUE NOT NULL DEFAULT '',
			original_url TEXT NOT NULL,
			clicks       BIGINT DEFAULT 0,
			created_at   TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			expires_at   TIMESTAMP NULL
		)`,
		`CREATE TABLE IF NOT EXISTS analytics (
			id          BIGSERIAL PRIMARY KEY,
			short_code  VARCHAR(32) NOT NULL,
			ip_address  VARCHAR(50),
			user_agent  TEXT,
			referrer    TEXT,
			device_type VARCHAR(16) DEFAULT 'Desktop',
			browser     VARCHAR(32) DEFAULT 'Other',
			clicked_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (short_code) REFERENCES urls(short_code) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_urls_short_code      ON urls(short_code)`,
		`CREATE INDEX IF NOT EXISTS idx_urls_user_id         ON urls(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_analytics_short_code ON analytics(short_code)`,
		`CREATE INDEX IF NOT EXISTS idx_analytics_clicked_at ON analytics(clicked_at)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_user_id     ON sessions(user_id)`,
	}
	for _, q := range stmts {
		if _, err := a.db.Exec(q); err != nil {
			log.Printf("migrate (skipped): %v", err)
		}
	}
	log.Println("Database schema ready")
}

type ctxKey string
const ctxUserID ctxKey = "userID"

func userIDFromCtx(r *http.Request) int64 {
	if v, ok := r.Context().Value(ctxUserID).(int64); ok {
		return v
	}
	return 0
}

func (a *App) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if c, err := r.Cookie(sessionCookie); err == nil {
			token = c.Value
		}
		if token == "" {
			if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
				token = strings.TrimPrefix(auth, "Bearer ")
			}
		}
		if token == "" {
			jsonError(w, "authentication required", http.StatusUnauthorized)
			return
		}
		var userID int64
		var expiresAt time.Time
		err := a.db.QueryRow(
			`SELECT user_id, expires_at FROM sessions WHERE token = $1`, token,
		).Scan(&userID, &expiresAt)
		if err != nil || time.Now().After(expiresAt) {
			http.SetCookie(w, &http.Cookie{Name: sessionCookie, MaxAge: -1, Path: "/"})
			jsonError(w, "session expired", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), ctxUserID, userID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (a *App) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := r.RemoteAddr
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			ip = strings.TrimSpace(strings.Split(fwd, ",")[0])
		}
		if !a.limiter.Allow(ip) {
			jsonError(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req AuthRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if !isValidEmail(req.Email) {
		jsonError(w, "invalid email address", http.StatusBadRequest)
		return
	}
	if len(req.Password) < 8 {
		jsonError(w, "password must be at least 8 characters", http.StatusBadRequest)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		jsonError(w, "server error", http.StatusInternalServerError)
		return
	}

	var userID int64
	err = a.db.QueryRow(
		`INSERT INTO users (email, password_hash) VALUES ($1, $2) RETURNING id`,
		req.Email, string(hash),
	).Scan(&userID)
	if err != nil {
		if strings.Contains(err.Error(), "unique") {
			jsonError(w, "email already registered", http.StatusConflict)
			return
		}
		jsonError(w, "database error", http.StatusInternalServerError)
		return
	}

	a.issueSession(w, userID)
	w.WriteHeader(http.StatusCreated)
	jsonOK(w, map[string]any{"message": "account created", "user_id": userID, "email": req.Email})
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req AuthRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))

	var userID int64
	var hash string
	err := a.db.QueryRow(
		`SELECT id, password_hash FROM users WHERE email = $1`, req.Email,
	).Scan(&userID, &hash)
	if err != nil {
		bcrypt.CompareHashAndPassword([]byte("$2a$10$dummyhashfortimingsafety12345678"), []byte(req.Password))
		jsonError(w, "invalid credentials", http.StatusUnauthorized)
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)); err != nil {
		jsonError(w, "invalid credentials", http.StatusUnauthorized)
		return
	}

	a.issueSession(w, userID)
	jsonOK(w, map[string]any{"message": "logged in", "user_id": userID, "email": req.Email})
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_, _ = a.db.Exec(`DELETE FROM sessions WHERE token = $1`, c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, MaxAge: -1, Path: "/"})
	jsonOK(w, map[string]string{"message": "logged out"})
}

func (a *App) handleMe(w http.ResponseWriter, r *http.Request) {
	userID := userIDFromCtx(r)
	var u User
	err := a.db.QueryRow(
		`SELECT id, email, created_at FROM users WHERE id = $1`, userID,
	).Scan(&u.ID, &u.Email, &u.CreatedAt)
	if err != nil {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	jsonOK(w, u)
}

func (a *App) issueSession(w http.ResponseWriter, userID int64) {
	token, err := generateToken()
	if err != nil {
		return
	}
	expiresAt := time.Now().Add(sessionTTL)
	_, _ = a.db.Exec(
		`INSERT INTO sessions (token, user_id, expires_at) VALUES ($1, $2, $3)`,
		token, userID, expiresAt,
	)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (a *App) handleShorten(w http.ResponseWriter, r *http.Request) {
	userID := userIDFromCtx(r)
	var req ShortenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.URL == "" {
		jsonError(w, "invalid request — 'url' required", http.StatusBadRequest)
		return
	}
	if !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {
		jsonError(w, "url must begin with http:// or https://", http.StatusBadRequest)
		return
	}

	var shortCode string

	if req.CustomSlug != "" {
		slug := strings.TrimSpace(req.CustomSlug)
		if !validSlugRe.MatchString(slug) {
			jsonError(w, "alias must be 3–32 chars: letters, digits, - or _ only", http.StatusBadRequest)
			return
		}
		if reservedSlugs[strings.ToLower(slug)] {
			jsonError(w, "that alias is reserved", http.StatusConflict)
			return
		}
		var exists int
		_ = a.db.QueryRow(`SELECT COUNT(1) FROM urls WHERE short_code = $1`, slug).Scan(&exists)
		if exists > 0 {
			jsonError(w, "alias already taken", http.StatusConflict)
			return
		}
		shortCode = slug
	}

	var expiresAt *time.Time
	if req.ExpiresIn != nil && *req.ExpiresIn > 0 {
		t := time.Now().Add(time.Duration(*req.ExpiresIn) * time.Hour)
		expiresAt = &t
	}

	if shortCode != "" {
		_, err := a.db.Exec(
			`INSERT INTO urls (user_id, short_code, original_url, expires_at) VALUES ($1, $2, $3, $4)`,
			userID, shortCode, req.URL, expiresAt,
		)
		if err != nil {
			log.Printf("insert custom slug: %v", err)
			jsonError(w, "database error", http.StatusInternalServerError)
			return
		}
	} else {
		var id int64
		err := a.db.QueryRow(
			`INSERT INTO urls (user_id, original_url, short_code, expires_at) VALUES ($1, $2, 'tmp', $3) RETURNING id`,
			userID, req.URL, expiresAt,
		).Scan(&id)
		if err != nil {
			log.Printf("insert url: %v", err)
			jsonError(w, "database error", http.StatusInternalServerError)
			return
		}
		shortCode = encodeBase62(id)
		if _, err := a.db.Exec(`UPDATE urls SET short_code = $1 WHERE id = $2`, shortCode, id); err != nil {
			log.Printf("update short_code: %v", err)
			jsonError(w, "database error", http.StatusInternalServerError)
			return
		}
	}

	if a.redis != nil {
		ttl := 24 * time.Hour
		if expiresAt != nil {
			ttl = time.Until(*expiresAt)
		}
		a.redis.Set(a.ctx, "url:"+shortCode, req.URL, ttl)
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	jsonOK(w, ShortenResponse{
		ShortCode:   shortCode,
		ShortURL:    fmt.Sprintf("%s://%s/%s", scheme, r.Host, shortCode),
		OriginalURL: req.URL,
		ExpiresAt:   expiresAt,
	})
}

func (a *App) handleRedirect(w http.ResponseWriter, r *http.Request) {
	shortCode := chi.URLParam(r, "shortCode")
	originalURL, expired := a.resolveURL(shortCode)
	if originalURL == "" {
		http.NotFound(w, r)
		return
	}
	if expired {
		http.Error(w, "410 Link Expired", http.StatusGone)
		return
	}
	go a.recordClick(shortCode, r)
	http.Redirect(w, r, originalURL, http.StatusFound) // 302: each click is counted
}

func (a *App) resolveURL(shortCode string) (url string, expired bool) {
	if a.redis != nil {
		if val, err := a.redis.Get(a.ctx, "url:"+shortCode).Result(); err == nil {
			return val, false
		}
	}
	var original string
	var expiresAt *time.Time
	err := a.db.QueryRow(
		`SELECT original_url, expires_at FROM urls WHERE short_code = $1`, shortCode,
	).Scan(&original, &expiresAt)
	if err != nil {
		return "", false
	}
	if expiresAt != nil && time.Now().After(*expiresAt) {
		return original, true
	}
	if a.redis != nil {
		ttl := 24 * time.Hour
		if expiresAt != nil {
			ttl = time.Until(*expiresAt)
		}
		a.redis.Set(a.ctx, "url:"+shortCode, original, ttl)
	}
	return original, false
}

func (a *App) recordClick(shortCode string, r *http.Request) {
	ip := r.RemoteAddr
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		ip = strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	ua := r.Header.Get("User-Agent")
	ref := r.Header.Get("Referer")
	if ref == "" {
		ref = r.Header.Get("Referrer")
	}

	_, _ = a.db.Exec(`UPDATE urls SET clicks = clicks + 1 WHERE short_code = $1`, shortCode)
	_, _ = a.db.Exec(
		`INSERT INTO analytics (short_code, ip_address, user_agent, referrer, device_type, browser)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		shortCode, ip, ua, ref, detectDevice(ua), detectBrowser(ua),
	)
}

func (a *App) handleStats(w http.ResponseWriter, r *http.Request) {
	userID := userIDFromCtx(r)
	shortCode := chi.URLParam(r, "shortCode")
	var u URL
	err := a.db.QueryRow(
		`SELECT id, user_id, short_code, original_url, clicks, created_at, expires_at
		 FROM urls WHERE short_code = $1 AND user_id = $2`,
		shortCode, userID,
	).Scan(&u.ID, &u.UserID, &u.ShortCode, &u.OriginalURL, &u.Clicks, &u.CreatedAt, &u.ExpiresAt)
	if err != nil {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	jsonOK(w, u)
}

func (a *App) handleAnalytics(w http.ResponseWriter, r *http.Request) {
	userID := userIDFromCtx(r)
	shortCode := chi.URLParam(r, "shortCode")

	var ownerID int64
	if err := a.db.QueryRow(`SELECT user_id FROM urls WHERE short_code = $1`, shortCode).Scan(&ownerID); err != nil {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	if ownerID != userID {
		jsonError(w, "forbidden", http.StatusForbidden)
		return
	}

	var s AnalyticsSummary
	_ = a.db.QueryRow(`SELECT clicks FROM urls WHERE short_code = $1`, shortCode).Scan(&s.TotalClicks)

	rows, _ := a.db.Query(`
		SELECT TO_CHAR(clicked_at::date, 'YYYY-MM-DD'), COUNT(*)
		FROM analytics WHERE short_code = $1 AND clicked_at >= NOW() - INTERVAL '30 days'
		GROUP BY 1 ORDER BY 1`, shortCode)
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var c ClicksByDay
			if err := rows.Scan(&c.Date, &c.Clicks); err == nil {
				s.ClicksByDay = append(s.ClicksByDay, c)
			}
		}
	}
	if s.ClicksByDay == nil {
		s.ClicksByDay = []ClicksByDay{}
	}

	rr, _ := a.db.Query(`
		SELECT COALESCE(NULLIF(referrer,''), 'Direct'), COUNT(*)
		FROM analytics WHERE short_code = $1
		GROUP BY 1 ORDER BY 2 DESC LIMIT 10`, shortCode)
	if rr != nil {
		defer rr.Close()
		for rr.Next() {
			var rc ReferrerCount
			if err := rr.Scan(&rc.Referrer, &rc.Count); err == nil {
				s.TopReferrers = append(s.TopReferrers, rc)
			}
		}
	}
	if s.TopReferrers == nil {
		s.TopReferrers = []ReferrerCount{}
	}

	dr, _ := a.db.Query(`
		SELECT device_type, COUNT(*) FROM analytics WHERE short_code = $1 GROUP BY 1 ORDER BY 2 DESC`,
		shortCode)
	if dr != nil {
		defer dr.Close()
		for dr.Next() {
			var kv KVCount
			if err := dr.Scan(&kv.Key, &kv.Count); err == nil {
				s.DeviceBreakdown = append(s.DeviceBreakdown, kv)
			}
		}
	}
	if s.DeviceBreakdown == nil {
		s.DeviceBreakdown = []KVCount{}
	}

	br, _ := a.db.Query(`
		SELECT browser, COUNT(*) FROM analytics WHERE short_code = $1 GROUP BY 1 ORDER BY 2 DESC`,
		shortCode)
	if br != nil {
		defer br.Close()
		for br.Next() {
			var kv KVCount
			if err := br.Scan(&kv.Key, &kv.Count); err == nil {
				s.BrowserBreakdown = append(s.BrowserBreakdown, kv)
			}
		}
	}
	if s.BrowserBreakdown == nil {
		s.BrowserBreakdown = []KVCount{}
	}

	cr, _ := a.db.Query(`
		SELECT id, short_code, ip_address, user_agent, referrer,
		       COALESCE(device_type,'Desktop'), COALESCE(browser,'Other'), clicked_at
		FROM analytics WHERE short_code = $1 ORDER BY clicked_at DESC LIMIT 50`,
		shortCode)
	if cr != nil {
		defer cr.Close()
		for cr.Next() {
			var rec AnalyticsRecord
			if err := cr.Scan(
				&rec.ID, &rec.ShortCode, &rec.IPAddress, &rec.UserAgent,
				&rec.Referrer, &rec.DeviceType, &rec.Browser, &rec.ClickedAt,
			); err == nil {
				s.RecentClicks = append(s.RecentClicks, rec)
			}
		}
	}
	if s.RecentClicks == nil {
		s.RecentClicks = []AnalyticsRecord{}
	}

	jsonOK(w, s)
}

func (a *App) handleListURLs(w http.ResponseWriter, r *http.Request) {
	userID := userIDFromCtx(r)
	rows, err := a.db.Query(
		`SELECT id, user_id, short_code, original_url, clicks, created_at, expires_at
		 FROM urls WHERE user_id = $1 ORDER BY created_at DESC`,
		userID,
	)
	if err != nil {
		jsonError(w, "database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var urls []URL
	for rows.Next() {
		var u URL
		if err := rows.Scan(&u.ID, &u.UserID, &u.ShortCode, &u.OriginalURL, &u.Clicks, &u.CreatedAt, &u.ExpiresAt); err == nil {
			urls = append(urls, u)
		}
	}
	if urls == nil {
		urls = []URL{}
	}
	jsonOK(w, urls)
}

func (a *App) handleDeleteURL(w http.ResponseWriter, r *http.Request) {
	userID := userIDFromCtx(r)
	shortCode := chi.URLParam(r, "shortCode")
	res, err := a.db.Exec(
		`DELETE FROM urls WHERE short_code = $1 AND user_id = $2`, shortCode, userID,
	)
	if err != nil {
		jsonError(w, "database error", http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		jsonError(w, "not found or not yours", http.StatusNotFound)
		return
	}
	if a.redis != nil {
		a.redis.Del(a.ctx, "url:"+shortCode)
	}
	jsonOK(w, map[string]string{"message": "deleted"})
}

func (a *App) handleDashboard(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "static/index.html")
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func isValidEmail(e string) bool {
	if len(e) < 3 || len(e) > 254 {
		return false
	}
	at := strings.LastIndex(e, "@")
	if at < 1 {
		return false
	}
	domain := e[at+1:]
	if !strings.Contains(domain, ".") || len(domain) < 3 {
		return false
	}
	for _, c := range e {
		if c > unicode.MaxASCII {
			return false
		}
	}
	return true
}

func main() {
	app := newApp()

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.RealIP)
	r.Use(middleware.Compress(5))
	r.Use(app.rateLimit)

	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
			next.ServeHTTP(w, r)
		})
	})

	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	r.Get("/", app.handleDashboard)

	r.Post("/api/auth/register", app.handleRegister)
	r.Post("/api/auth/login", app.handleLogin)
	r.Post("/api/auth/logout", app.handleLogout)

	r.Group(func(r chi.Router) {
		r.Use(app.requireAuth)
		r.Get("/api/me", app.handleMe)
		r.Post("/api/shorten", app.handleShorten)
		r.Get("/api/urls", app.handleListURLs)
		r.Get("/api/stats/{shortCode}", app.handleStats)
		r.Get("/api/analytics/{shortCode}", app.handleAnalytics)
		r.Delete("/api/urls/{shortCode}", app.handleDeleteURL)
	})

	r.Get("/{shortCode}", app.handleRedirect)

	port := getenv("PORT", "8080")
	log.Printf("🚀 Server listening on http://localhost:%s", port)
	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
