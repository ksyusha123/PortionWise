package core

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	_ "modernc.org/sqlite"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"
)

func Env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func Secret(k string) string {
	v := os.Getenv(k)
	if len(v) < 32 {
		panic(k + " must contain at least 32 characters")
	}
	return v
}
func ID() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func OpenDB(path, schema string) (*sql.DB, error) {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return nil, e
	}
	db, e := sql.Open("sqlite", path)
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	if _, e = db.Exec("PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;" + schema); e != nil {
		db.Close()
		return nil, e
	}
	return db, nil
}
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}
func Error(w http.ResponseWriter, status int, msg string) {
	JSON(w, status, map[string]string{"error": msg})
}
func Decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		Error(w, 400, "некорректный JSON")
		return false
	}
	if d.Decode(new(any)) != io.EOF {
		Error(w, 400, "ожидался один JSON объект")
		return false
	}
	return true
}

type claims struct {
	UserID string `json:"sub"`
	Exp    int64  `json:"exp"`
}

func Token(secret, id string) string {
	b, _ := json.Marshal(claims{id, time.Now().Add(24 * time.Hour).Unix()})
	p := base64.RawURLEncoding.EncodeToString(b)
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(p))
	return p + "." + base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

type contextKey struct{}

func UserID(r *http.Request) string { v, _ := r.Context().Value(contextKey{}).(string); return v }
func Auth(secret string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
		if len(p) != 2 {
			Error(w, 401, "требуется авторизация")
			return
		}
		sig, e := base64.RawURLEncoding.DecodeString(p[1])
		h := hmac.New(sha256.New, []byte(secret))
		h.Write([]byte(p[0]))
		if e != nil || !hmac.Equal(sig, h.Sum(nil)) {
			Error(w, 401, "недействительный токен")
			return
		}
		b, e := base64.RawURLEncoding.DecodeString(p[0])
		var c claims
		if e != nil || json.Unmarshal(b, &c) != nil || c.UserID == "" || c.Exp <= time.Now().Unix() {
			Error(w, 401, "токен истёк")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, c.UserID)))
	})
}
func Internal(secret string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !hmac.Equal([]byte(r.Header.Get("X-Internal-Key")), []byte(secret)) {
			Error(w, 401, "недоступно")
			return
		}
		next(w, r)
	}
}

type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return e.Message }

type Client struct{ HTTP *http.Client }

func NewClient(timeout time.Duration) *Client { return &Client{&http.Client{Timeout: timeout}} }
func (c *Client) Do(ctx context.Context, method, url, token string, body io.Reader, out any) error {
	req, e := http.NewRequestWithContext(ctx, method, url, body)
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, e := c.HTTP.Do(req)
	if e != nil {
		return &APIError{503, "сервис временно недоступен"}
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		var v struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(res.Body, 65536)).Decode(&v)
		if v.Error == "" {
			v.Error = "ошибка сервиса"
		}
		return &APIError{res.StatusCode, v.Error}
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(out)
	}
	return nil
}
func ForwardError(w http.ResponseWriter, e error) {
	var a *APIError
	if errors.As(e, &a) {
		Error(w, a.Status, a.Message)
	} else {
		Error(w, 503, "сервис временно недоступен")
	}
}
func Bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}
func Serve(addr string, mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { JSON(w, 200, map[string]string{"status": "ok"}) })
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() != nil {
				slog.Error("request panic", "path", r.URL.Path)
				Error(w, 500, "внутренняя ошибка")
			}
		}()
		mux.ServeHTTP(w, r)
	})
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 110 * time.Second, IdleTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(c)
	}()
	slog.Info("listening", "address", addr)
	if e := srv.ListenAndServe(); e != nil && e != http.ErrServerClosed {
		panic(fmt.Errorf("serve: %w", e))
	}
}
