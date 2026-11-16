package users

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fooddiary/internal/core"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const Schema = `CREATE TABLE IF NOT EXISTS users(id TEXT PRIMARY KEY,name TEXT NOT NULL,timezone TEXT NOT NULL,target TEXT);
CREATE TABLE IF NOT EXISTS auth_identities(provider TEXT NOT NULL,subject TEXT NOT NULL,user_id TEXT NOT NULL REFERENCES users(id),PRIMARY KEY(provider,subject));`

type Service struct {
	DB                  *sql.DB
	Secret, InternalKey string
}

func (s *Service) Routes() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("POST /internal/auth/telegram", core.Internal(s.InternalKey, s.telegram))
	m.Handle("GET /me", core.Auth(s.Secret, http.HandlerFunc(s.get)))
	m.Handle("PUT /me", core.Auth(s.Secret, http.HandlerFunc(s.update)))
	return m
}
func (s *Service) telegram(w http.ResponseWriter, r *http.Request) {
	var v struct {
		TelegramID int64  `json:"telegram_id"`
		Name       string `json:"name"`
	}
	if !core.Decode(w, r, &v) {
		return
	}
	if v.TelegramID <= 0 || len([]rune(v.Name)) > 200 {
		core.Error(w, 400, "некорректный пользователь")
		return
	}
	tx, e := s.DB.BeginTx(r.Context(), nil)
	if e != nil {
		core.Error(w, 500, "ошибка БД")
		return
	}
	defer tx.Rollback()
	var id string
	e = tx.QueryRowContext(r.Context(), "SELECT user_id FROM auth_identities WHERE provider='telegram' AND subject=?", strconv.FormatInt(v.TelegramID, 10)).Scan(&id)
	if errors.Is(e, sql.ErrNoRows) {
		id = core.ID()
		_, e = tx.ExecContext(r.Context(), "INSERT INTO users(id,name,timezone) VALUES(?,?,?)", id, v.Name, "UTC")
		if e == nil {
			_, e = tx.ExecContext(r.Context(), "INSERT INTO auth_identities(provider,subject,user_id) VALUES('telegram',?,?)", strconv.FormatInt(v.TelegramID, 10), id)
		}
	}
	if e != nil {
		core.Error(w, 500, "ошибка создания профиля")
		return
	}
	if tx.Commit() != nil {
		core.Error(w, 500, "ошибка БД")
		return
	}
	u, e := s.user(r, id)
	if e != nil {
		core.Error(w, 500, "ошибка БД")
		return
	}
	core.JSON(w, 200, map[string]any{"user": u, "token": core.Token(s.Secret, id)})
}
func (s *Service) user(r *http.Request, id string) (core.User, error) {
	var u core.User
	var target sql.NullString
	e := s.DB.QueryRowContext(r.Context(), "SELECT id,name,timezone,target FROM users WHERE id=?", id).Scan(&u.ID, &u.Name, &u.Timezone, &target)
	if e == nil && target.Valid {
		e = json.Unmarshal([]byte(target.String), &u.Target)
	}
	return u, e
}
func (s *Service) get(w http.ResponseWriter, r *http.Request) {
	u, e := s.user(r, core.UserID(r))
	if e != nil {
		core.Error(w, 404, "пользователь не найден")
		return
	}
	core.JSON(w, 200, u)
}
func (s *Service) update(w http.ResponseWriter, r *http.Request) {
	var u core.User
	if !core.Decode(w, r, &u) {
		return
	}
	u.Name = strings.TrimSpace(u.Name)
	if u.Name == "" || len([]rune(u.Name)) > 200 {
		core.Error(w, 400, "укажите имя до 200 символов")
		return
	}
	if _, e := time.LoadLocation(u.Timezone); e != nil {
		core.Error(w, 400, "неизвестный часовой пояс, пример: Asia/Yekaterinburg")
		return
	}
	if u.Target != nil && (!u.Target.Valid() || u.Target.Calories <= 0) {
		core.Error(w, 400, "проверьте целевое КБЖУ")
		return
	}
	var target any
	if u.Target != nil {
		b, _ := json.Marshal(u.Target)
		target = string(b)
	}
	res, e := s.DB.ExecContext(r.Context(), "UPDATE users SET name=?,timezone=?,target=? WHERE id=?", u.Name, u.Timezone, target, core.UserID(r))
	if e != nil {
		core.Error(w, 500, "ошибка БД")
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		core.Error(w, 404, "пользователь не найден")
		return
	}
	u.ID = core.UserID(r)
	core.JSON(w, 200, u)
}
