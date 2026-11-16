package analysis

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fooddiary/internal/core"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"time"
)

const Schema = `CREATE TABLE IF NOT EXISTS analyses(id TEXT PRIMARY KEY,user_id TEXT NOT NULL,request_key TEXT NOT NULL,day TEXT NOT NULL,status TEXT NOT NULL,food TEXT,created_at TEXT NOT NULL,UNIQUE(user_id,request_key));
CREATE INDEX IF NOT EXISTS analyses_quota ON analyses(user_id,day,status);`

type Service struct {
	DB              *sql.DB
	Secret, UserURL string
	Client          *core.Client
	Provider        Provider
	Now             func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
func (s *Service) Routes() *http.ServeMux {
	m := http.NewServeMux()
	m.Handle("POST /analyses", core.Auth(s.Secret, http.HandlerFunc(s.create)))
	m.Handle("GET /analyses/{id}", core.Auth(s.Secret, http.HandlerFunc(s.get)))
	m.Handle("GET /quota", core.Auth(s.Secret, http.HandlerFunc(s.quota)))
	return m
}
func (s *Service) profile(r *http.Request) (core.User, error) {
	var u core.User
	e := s.Client.Do(r.Context(), "GET", s.UserURL+"/me", core.Bearer(r), nil, &u)
	return u, e
}
func (s *Service) remaining(ctx context.Context, user, day string) int {
	var n int
	if s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM analyses WHERE user_id=? AND day=? AND status!='failed'", user, day).Scan(&n) != nil {
		return 0
	}
	return max(0, 5-n)
}
func (s *Service) load(ctx context.Context, user, id string) (core.Analysis, error) {
	var a core.Analysis
	var food sql.NullString
	e := s.DB.QueryRowContext(ctx, "SELECT id,user_id,day,status,food,created_at FROM analyses WHERE id=? AND user_id=?", id, user).Scan(&a.ID, &a.UserID, &a.Day, &a.Status, &food, &a.CreatedAt)
	if e == nil && food.Valid {
		e = json.Unmarshal([]byte(food.String), &a.Food)
	}
	if e == nil {
		a.Remaining = s.remaining(ctx, user, a.Day)
	}
	return a, e
}
func (s *Service) get(w http.ResponseWriter, r *http.Request) {
	a, e := s.load(r.Context(), core.UserID(r), r.PathValue("id"))
	if errors.Is(e, sql.ErrNoRows) {
		core.Error(w, 404, "анализ не найден")
		return
	}
	if e != nil {
		core.Error(w, 500, "ошибка БД")
		return
	}
	core.JSON(w, 200, a)
}
func (s *Service) quota(w http.ResponseWriter, r *http.Request) {
	u, e := s.profile(r)
	if e != nil {
		core.ForwardError(w, e)
		return
	}
	day := s.now().In(u.Location()).Format("2006-01-02")
	core.JSON(w, 200, map[string]any{"date": day, "limit": 5, "remaining": s.remaining(r.Context(), u.ID, day)})
}

// reserve serializes count + insert in a SQLite transaction. Pending analyses consume a slot.
func (s *Service) reserve(ctx context.Context, user, key, day string) (string, bool, error) {
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return "", false, e
	}
	defer tx.Rollback()
	_, e = tx.ExecContext(ctx, "UPDATE analyses SET status='failed' WHERE status='pending' AND created_at<?", s.now().UTC().Add(-3*time.Minute).Format(time.RFC3339))
	if e != nil {
		return "", false, e
	}
	var id string
	e = tx.QueryRowContext(ctx, "SELECT id FROM analyses WHERE user_id=? AND request_key=?", user, key).Scan(&id)
	if e == nil {
		return id, false, tx.Commit()
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return "", false, e
	}
	var count int
	if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM analyses WHERE user_id=? AND day=? AND status!='failed'", user, day).Scan(&count); e != nil {
		return "", false, e
	}
	if count >= 5 {
		return "", false, &core.APIError{Status: 429, Message: "дневной лимит: 5 анализов. Ручное добавление доступно без ограничений"}
	}
	id = core.ID()
	_, e = tx.ExecContext(ctx, "INSERT INTO analyses(id,user_id,request_key,day,status,created_at) VALUES(?,?,?,?,'pending',?)", id, user, key, day, s.now().UTC().Format(time.RFC3339))
	if e != nil {
		return "", false, e
	}
	return id, true, tx.Commit()
}
func (s *Service) create(w http.ResponseWriter, r *http.Request) {
	u, e := s.profile(r)
	if e != nil {
		core.ForwardError(w, e)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 128 {
		core.Error(w, 400, "нужен Idempotency-Key до 128 символов")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 6<<20)
	if e = r.ParseMultipartForm(6 << 20); e != nil {
		core.Error(w, 400, "загрузите JPEG или PNG до 5 МБ")
		return
	}
	defer r.MultipartForm.RemoveAll()
	f, _, e := r.FormFile("photo")
	if e != nil {
		core.Error(w, 400, "нужно поле photo")
		return
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, (5<<20)+1))
	if e != nil || len(b) == 0 || len(b) > 5<<20 {
		core.Error(w, 400, "фото должно быть до 5 МБ")
		return
	}
	mime := http.DetectContentType(b)
	if mime != "image/jpeg" && mime != "image/png" {
		core.Error(w, 400, "поддерживаются JPEG и PNG")
		return
	}
	config, _, imageErr := image.DecodeConfig(bytes.NewReader(b))
	if imageErr != nil || config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 25000000 {
		core.Error(w, 400, "некорректное фото или разрешение больше 25 мегапикселей")
		return
	}
	day := s.now().In(u.Location()).Format("2006-01-02")
	id, fresh, e := s.reserve(r.Context(), u.ID, key, day)
	if e != nil {
		core.ForwardError(w, e)
		return
	}
	if fresh {
		// A client disconnect must not leave a charged pending slot; provider has its own deadline.
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		food, err := s.Provider.Analyze(ctx, b, mime)
		status := "done"
		var data any
		if err != nil {
			status = "failed"
		} else if food == nil {
			status = "no_food"
		} else {
			raw, _ := json.Marshal(food)
			data = string(raw)
		}
		saveCtx, saveCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer saveCancel()
		if _, e = s.DB.ExecContext(saveCtx, "UPDATE analyses SET status=?,food=? WHERE id=? AND status='pending'", status, data, id); e != nil {
			core.Error(w, 500, "не удалось сохранить анализ")
			return
		}
		if err != nil {
			core.Error(w, 502, err.Error()+". Попытка возвращена; отправьте фото заново.")
			return
		}
	}
	a, e := s.load(r.Context(), u.ID, id)
	if e != nil {
		core.Error(w, 500, "ошибка БД")
		return
	}
	if a.Status == "pending" {
		core.Error(w, 409, "этот анализ ещё выполняется")
		return
	}
	if a.Status == "failed" {
		core.Error(w, 502, "анализ завершился ошибкой; отправьте фото заново")
		return
	}
	core.JSON(w, 200, a)
}
