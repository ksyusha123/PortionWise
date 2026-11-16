package diary

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fooddiary/internal/core"
	"net/http"
	"time"
)

const Schema = `CREATE TABLE IF NOT EXISTS meals(id TEXT PRIMARY KEY,user_id TEXT NOT NULL,eaten_at TEXT NOT NULL,source_id TEXT,request_key TEXT NOT NULL,payload TEXT NOT NULL,UNIQUE(user_id,source_id),UNIQUE(user_id,request_key));CREATE INDEX IF NOT EXISTS meals_user_time ON meals(user_id,eaten_at);`

type Service struct {
	DB                           *sql.DB
	Secret, UserURL, AnalysisURL string
	Client                       *core.Client
}

func (s *Service) Routes() *http.ServeMux {
	m := http.NewServeMux()
	for p, h := range map[string]http.HandlerFunc{"POST /meals": s.create, "GET /meals/{id}": s.get, "PUT /meals/{id}": s.update, "DELETE /meals/{id}": s.remove, "GET /days/{date}": s.day} {
		m.Handle(p, core.Auth(s.Secret, h))
	}
	return m
}
func (s *Service) load(r *http.Request, id string) (core.Meal, error) {
	var m core.Meal
	var b string
	e := s.DB.QueryRowContext(r.Context(), "SELECT payload FROM meals WHERE id=? AND user_id=?", id, core.UserID(r)).Scan(&b)
	if e == nil {
		e = json.Unmarshal([]byte(b), &m)
	}
	return m, e
}
func validate(m *core.Meal) error {
	if e := m.Food.Validate(); e != nil {
		return e
	}
	t, e := time.Parse(time.RFC3339, m.EatenAt)
	if e != nil {
		return errors.New("нужны дата и время RFC3339")
	}
	if t.Year() < 2000 || t.Year() > 2100 {
		return errors.New("дата вне диапазона 2000–2100")
	}
	m.EatenAt = t.UTC().Format(time.RFC3339)
	return nil
}
func (s *Service) get(w http.ResponseWriter, r *http.Request) {
	m, e := s.load(r, r.PathValue("id"))
	if errors.Is(e, sql.ErrNoRows) {
		core.Error(w, 404, "запись не найдена")
		return
	}
	if e != nil {
		core.Error(w, 500, "ошибка БД")
		return
	}
	core.JSON(w, 200, m)
}
func (s *Service) create(w http.ResponseWriter, r *http.Request) {
	var m core.Meal
	if !core.Decode(w, r, &m) {
		return
	}
	if e := validate(&m); e != nil {
		core.Error(w, 400, e.Error())
		return
	}
	if m.RequestKey == "" || len(m.RequestKey) > 128 {
		core.Error(w, 400, "нужен request_key до 128 символов")
		return
	}
	m.ID = core.ID()
	m.UserID = core.UserID(r)
	m.AnalysisSnapshot = nil
	// Check replays before calling the analysis dependency: already-saved meals remain available during an outage.
	var old string
	e := s.DB.QueryRowContext(r.Context(), "SELECT payload FROM meals WHERE user_id=? AND (request_key=? OR (source_id IS NOT NULL AND source_id=?))", m.UserID, m.RequestKey, m.SourceAnalysisID).Scan(&old)
	if e == nil {
		var saved core.Meal
		if json.Unmarshal([]byte(old), &saved) != nil {
			core.Error(w, 500, "ошибка БД")
			return
		}
		core.JSON(w, 200, saved)
		return
	}
	if !errors.Is(e, sql.ErrNoRows) {
		core.Error(w, 500, "ошибка БД")
		return
	}
	if m.SourceAnalysisID != "" {
		var a core.Analysis
		if e = s.Client.Do(r.Context(), "GET", s.AnalysisURL+"/analyses/"+m.SourceAnalysisID, core.Bearer(r), nil, &a); e != nil {
			core.ForwardError(w, e)
			return
		}
		if a.Status != "done" || a.Food == nil || a.UserID != m.UserID {
			core.Error(w, 400, "анализ не готов или принадлежит другому пользователю")
			return
		}
		m.AnalysisSnapshot = &a
	}
	var source any
	if m.SourceAnalysisID != "" {
		source = m.SourceAnalysisID
	}
	b, _ := json.Marshal(m)
	_, e = s.DB.ExecContext(r.Context(), "INSERT INTO meals(id,user_id,eaten_at,source_id,request_key,payload) VALUES(?,?,?,?,?,?)", m.ID, m.UserID, m.EatenAt, source, m.RequestKey, string(b))
	if e != nil {
		// Concurrent confirmations can hit either unique key; return the committed original.
		if s.DB.QueryRowContext(r.Context(), "SELECT payload FROM meals WHERE user_id=? AND (request_key=? OR source_id=?)", m.UserID, m.RequestKey, m.SourceAnalysisID).Scan(&old) == nil {
			var saved core.Meal
			_ = json.Unmarshal([]byte(old), &saved)
			core.JSON(w, 200, saved)
			return
		}
		core.Error(w, 500, "не удалось сохранить запись")
		return
	}
	core.JSON(w, 201, m)
}
func (s *Service) update(w http.ResponseWriter, r *http.Request) {
	old, e := s.load(r, r.PathValue("id"))
	if e != nil {
		core.Error(w, 404, "запись не найдена")
		return
	}
	var m core.Meal
	if !core.Decode(w, r, &m) {
		return
	}
	if e = validate(&m); e != nil {
		core.Error(w, 400, e.Error())
		return
	}
	m.ID = old.ID
	m.UserID = old.UserID
	m.RequestKey = old.RequestKey
	m.SourceAnalysisID = old.SourceAnalysisID
	m.AnalysisSnapshot = old.AnalysisSnapshot
	b, _ := json.Marshal(m)
	res, e := s.DB.ExecContext(r.Context(), "UPDATE meals SET eaten_at=?,payload=? WHERE id=? AND user_id=?", m.EatenAt, string(b), m.ID, m.UserID)
	if e != nil {
		core.Error(w, 500, "ошибка БД")
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		core.Error(w, 404, "запись удалена")
		return
	}
	core.JSON(w, 200, m)
}
func (s *Service) remove(w http.ResponseWriter, r *http.Request) {
	res, e := s.DB.ExecContext(r.Context(), "DELETE FROM meals WHERE id=? AND user_id=?", r.PathValue("id"), core.UserID(r))
	if e != nil {
		core.Error(w, 500, "ошибка БД")
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		core.Error(w, 404, "запись не найдена")
		return
	}
	core.JSON(w, 204, nil)
}
func (s *Service) day(w http.ResponseWriter, r *http.Request) {
	date := r.PathValue("date")
	if _, e := time.Parse("2006-01-02", date); e != nil {
		core.Error(w, 400, "дата в формате ГГГГ-ММ-ДД")
		return
	}
	// Bot passes the profile timezone so saved diary entries remain readable if User Service is down.
	var u core.User
	e := s.Client.Do(r.Context(), "GET", s.UserURL+"/me", core.Bearer(r), nil, &u)
	unavailable := e != nil
	if unavailable {
		tz := r.URL.Query().Get("timezone")
		if _, err := time.LoadLocation(tz); err != nil || tz == "" {
			core.Error(w, 503, "профиль недоступен; нужен параметр timezone")
			return
		}
		u.Timezone = tz
	}
	start, _ := time.ParseInLocation("2006-01-02", date, u.Location())
	end := start.AddDate(0, 0, 1)
	rows, e := s.DB.QueryContext(r.Context(), "SELECT payload FROM meals WHERE user_id=? AND eaten_at>=? AND eaten_at<? ORDER BY eaten_at,id", core.UserID(r), start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339))
	if e != nil {
		core.Error(w, 500, "ошибка БД")
		return
	}
	defer rows.Close()
	d := core.Day{Date: date, Meals: []core.Meal{}, Target: u.Target, TargetUnavailable: unavailable}
	for rows.Next() {
		var b string
		var m core.Meal
		if rows.Scan(&b) != nil || json.NewDecoder(bytes.NewBufferString(b)).Decode(&m) != nil {
			core.Error(w, 500, "ошибка чтения дневника")
			return
		}
		d.Meals = append(d.Meals, m)
		d.Total = d.Total.Add(m.Food.Nutrition)
	}
	if rows.Err() != nil {
		core.Error(w, 500, "ошибка БД")
		return
	}
	if u.Target != nil {
		n := u.Target.Sub(d.Total)
		d.Remaining = &n
	}
	core.JSON(w, 200, d)
}
