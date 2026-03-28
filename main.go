package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-redis/redis/v8"
	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
)

type URL struct {
	ID          int64     `json:"id"`
	ShortCode   string    `json:"short_code"`
	OriginalURL string    `json:"original_url"`
	Clicks      int64     `json:"clicks"`
	CreatedAt   time.Time `json:"created_at"`
}

type AnalyticsRecord struct {
	ID        int64     `json:"id"`
	ShortCode string    `json:"short_code"`
	IPAddress string    `json:"ip_address"`
	UserAgent string    `json:"user_agent"`
	Referrer  string    `json:"referrer"`
	ClickedAt time.Time `json:"clicked_at"`
}

type ShortenRequest struct {
	URL string `json:"url"`
}

type ShortenResponse struct {
	ShortCode   string `json:"short_code"`
	ShortURL    string `json:"short_url"`
	OriginalURL string `json:"original_url"`
}

const base62Chars = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

func encodeBase62(n int64) string {
	if n == 0 {
		return "0"
	}
	var sb strings.Builder
	for n > 0 {
		sb.WriteByte(base62Chars[n%62])
		n /= 62
	}
	s := sb.String()
	runes := []byte(s)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}

type App struct {
	db    *sql.DB
	redis *redis.Client
	ctx   context.Context
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

	app := &App{db: db, redis: rdb, ctx: ctx}
	app.migrate()
	return app
}

func (a *App) migrate() {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS urls (
			id         BIGSERIAL PRIMARY KEY,
			short_code VARCHAR(20) UNIQUE NOT NULL DEFAULT '',
			original_url TEXT NOT NULL,
			clicks     INTEGER DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS analytics (
			id         BIGSERIAL PRIMARY KEY,
			short_code VARCHAR(20) NOT NULL,
			ip_address VARCHAR(50),
			user_agent TEXT,
			referrer   TEXT,
			clicked_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (short_code) REFERENCES urls(short_code)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_urls_short_code ON urls(short_code)`,
		`CREATE INDEX IF NOT EXISTS idx_analytics_short_code ON analytics(short_code)`,
	}
	for _, q := range queries {
		if _, err := a.db.Exec(q); err != nil {
			log.Fatalf("migrate: %v", err)
		}
	}
	log.Println("Database schema ready")
}

func (a *App) handleShorten(w http.ResponseWriter, r *http.Request) {
	var req ShortenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.URL == "" {
		jsonError(w, "invalid request body — 'url' required", http.StatusBadRequest)
		return
	}

	if !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {
		jsonError(w, "url must start with http:// or https://", http.StatusBadRequest)
		return
	}

	var id int64
	err := a.db.QueryRow(
		`INSERT INTO urls (original_url, short_code) VALUES ($1, $2) RETURNING id`,
		req.URL, "tmp",
	).Scan(&id)
	if err != nil {
		log.Printf("insert url: %v", err)
		jsonError(w, "database error", http.StatusInternalServerError)
		return
	}

	shortCode := encodeBase62(id)

	if _, err := a.db.Exec(`UPDATE urls SET short_code = $1 WHERE id = $2`, shortCode, id); err != nil {
		log.Printf("update short_code: %v", err)
		jsonError(w, "database error", http.StatusInternalServerError)
		return
	}

	if a.redis != nil {
		a.redis.Set(a.ctx, "url:"+shortCode, req.URL, 24*time.Hour)
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host

	jsonOK(w, ShortenResponse{
		ShortCode:   shortCode,
		ShortURL:    fmt.Sprintf("%s://%s/%s", scheme, host, shortCode),
		OriginalURL: req.URL,
	})
}

func (a *App) handleRedirect(w http.ResponseWriter, r *http.Request) {
	shortCode := chi.URLParam(r, "shortCode")

	originalURL := a.resolveURL(shortCode)
	if originalURL == "" {
		http.NotFound(w, r)
		return
	}

	go a.recordClick(shortCode, r)

	http.Redirect(w, r, originalURL, http.StatusMovedPermanently)
}

func (a *App) resolveURL(shortCode string) string {
	if a.redis != nil {
		if val, err := a.redis.Get(a.ctx, "url:"+shortCode).Result(); err == nil {
			return val
		}
	}
	var original string
	if err := a.db.QueryRow(`SELECT original_url FROM urls WHERE short_code = $1`, shortCode).Scan(&original); err != nil {
		return ""
	}
	if a.redis != nil {
		a.redis.Set(a.ctx, "url:"+shortCode, original, 24*time.Hour)
	}
	return original
}

func (a *App) recordClick(shortCode string, r *http.Request) {
	ip := r.RemoteAddr
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		ip = strings.Split(fwd, ",")[0]
	}
	ua := r.Header.Get("User-Agent")
	ref := r.Header.Get("Referer")

	_, _ = a.db.Exec(`UPDATE urls SET clicks = clicks + 1 WHERE short_code = $1`, shortCode)
	_, _ = a.db.Exec(
		`INSERT INTO analytics (short_code, ip_address, user_agent, referrer) VALUES ($1,$2,$3,$4)`,
		shortCode, ip, ua, ref,
	)
}

func (a *App) handleStats(w http.ResponseWriter, r *http.Request) {
	shortCode := chi.URLParam(r, "shortCode")
	var u URL
	err := a.db.QueryRow(
		`SELECT id, short_code, original_url, clicks, created_at FROM urls WHERE short_code = $1`,
		shortCode,
	).Scan(&u.ID, &u.ShortCode, &u.OriginalURL, &u.Clicks, &u.CreatedAt)
	if err != nil {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	jsonOK(w, u)
}

func (a *App) handleAnalytics(w http.ResponseWriter, r *http.Request) {
	shortCode := chi.URLParam(r, "shortCode")
	rows, err := a.db.Query(
		`SELECT id, short_code, ip_address, user_agent, referrer, clicked_at
		 FROM analytics WHERE short_code = $1 ORDER BY clicked_at DESC LIMIT 100`,
		shortCode,
	)
	if err != nil {
		jsonError(w, "database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var records []AnalyticsRecord
	for rows.Next() {
		var rec AnalyticsRecord
		if err := rows.Scan(&rec.ID, &rec.ShortCode, &rec.IPAddress, &rec.UserAgent, &rec.Referrer, &rec.ClickedAt); err == nil {
			records = append(records, rec)
		}
	}
	if records == nil {
		records = []AnalyticsRecord{}
	}
	jsonOK(w, records)
}

func (a *App) handleListURLs(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(
		`SELECT id, short_code, original_url, clicks, created_at FROM urls ORDER BY created_at DESC`,
	)
	if err != nil {
		jsonError(w, "database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var urls []URL
	for rows.Next() {
		var u URL
		if err := rows.Scan(&u.ID, &u.ShortCode, &u.OriginalURL, &u.Clicks, &u.CreatedAt); err == nil {
			urls = append(urls, u)
		}
	}
	if urls == nil {
		urls = []URL{}
	}
	jsonOK(w, urls)
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

func main() {
	app := newApp()

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.RealIP)

	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	})

	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	r.Get("/", app.handleDashboard)
	r.Post("/api/shorten", app.handleShorten)
	r.Get("/api/urls", app.handleListURLs)
	r.Get("/api/stats/{shortCode}", app.handleStats)
	r.Get("/api/analytics/{shortCode}", app.handleAnalytics)
	r.Get("/{shortCode}", app.handleRedirect)

	port := getenv("PORT", "8080")
	log.Printf("Server listening on http://localhost:%s", port)
	if err := http.ListenAndServe(":"+port, r); err != nil {
		log.Fatal(err)
	}
}