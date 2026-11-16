package tests

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fooddiary/internal/analysis"
	"fooddiary/internal/core"
	"fooddiary/internal/diary"
	"fooddiary/internal/users"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const secret = "test-secret-with-at-least-32-characters"

type fixture struct {
	t               *testing.T
	user, ai, diary *httptest.Server
	analysisDB      *sql.DB
	token           string
	uid             string
}

func db(t *testing.T, name, schema string) *sql.DB {
	t.Helper()
	d, e := core.OpenDB(filepath.Join(t.TempDir(), name+".db"), schema)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { d.Close() })
	return d
}
func setup(t *testing.T, p analysis.Provider) *fixture {
	t.Helper()
	us := &users.Service{DB: db(t, "user", users.Schema), Secret: secret, InternalKey: secret}
	u := httptest.NewServer(us.Routes())
	t.Cleanup(u.Close)
	ad := db(t, "analysis", analysis.Schema)
	as := &analysis.Service{DB: ad, Secret: secret, UserURL: u.URL, Client: core.NewClient(time.Second), Provider: p}
	a := httptest.NewServer(as.Routes())
	t.Cleanup(a.Close)
	ds := &diary.Service{DB: db(t, "diary", diary.Schema), Secret: secret, UserURL: u.URL, AnalysisURL: a.URL, Client: core.NewClient(time.Second)}
	d := httptest.NewServer(ds.Routes())
	t.Cleanup(d.Close)
	f := &fixture{t: t, user: u, ai: a, diary: d, analysisDB: ad}
	f.token, f.uid = f.login(101)
	return f
}
func (f *fixture) login(id int64) (string, string) {
	var out struct {
		Token string    `json:"token"`
		User  core.User `json:"user"`
	}
	raw, _ := json.Marshal(map[string]any{"telegram_id": id, "name": "Тест"})
	req, _ := http.NewRequest("POST", f.user.URL+"/internal/auth/telegram", bytes.NewReader(raw))
	req.Header.Set("X-Internal-Key", secret)
	res, e := http.DefaultClient.Do(req)
	if e != nil {
		f.t.Fatal(e)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		f.t.Fatal(res.Status)
	}
	if e = json.NewDecoder(res.Body).Decode(&out); e != nil {
		f.t.Fatal(e)
	}
	return out.Token, out.User.ID
}
func (f *fixture) request(method, url, token string, in, out any) int {
	f.t.Helper()
	var data []byte
	if in != nil {
		data, _ = json.Marshal(in)
	}
	req, _ := http.NewRequest(method, url, bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+token)
	res, e := http.DefaultClient.Do(req)
	if e != nil {
		f.t.Fatal(e)
	}
	defer res.Body.Close()
	if out != nil {
		if e = json.NewDecoder(res.Body).Decode(out); e != nil {
			f.t.Fatal(e)
		}
	}
	return res.StatusCode
}
func photoRequest(url, token, key string) *http.Request {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, _ := w.CreateFormFile("photo", "test.png")
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.White)
	_ = png.Encode(part, img)
	_ = w.Close()
	req, _ := http.NewRequest("POST", url+"/analyses", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", key)
	return req
}
func (f *fixture) analyze(key string) (int, core.Analysis) {
	f.t.Helper()
	res, e := http.DefaultClient.Do(photoRequest(f.ai.URL, f.token, key))
	if e != nil {
		f.t.Fatal(e)
	}
	defer res.Body.Close()
	var a core.Analysis
	if res.StatusCode == 200 {
		if e = json.NewDecoder(res.Body).Decode(&a); e != nil {
			f.t.Fatal(e)
		}
	}
	return res.StatusCode, a
}
func TestFullFlowAndOwnership(t *testing.T) {
	f := setup(t, analysis.Mock{})
	_, sameID := f.login(101)
	if sameID != f.uid {
		t.Fatal("telegram identity created duplicate account")
	}
	var user core.User
	f.request("GET", f.user.URL+"/me", f.token, nil, &user)
	user.Timezone = "Asia/Yekaterinburg"
	target := core.Nutrition{Calories: 2000, Protein: 120, Fat: 65, Carbs: 230}
	user.Target = &target
	if f.request("PUT", f.user.URL+"/me", f.token, user, nil) != 200 {
		t.Fatal("profile update")
	}
	status, a := f.analyze("one")
	if status != 200 || a.Food == nil || a.Remaining != 4 {
		t.Fatalf("analysis: %d %+v", status, a)
	}
	if status, replay := f.analyze("one"); status != 200 || replay.ID != a.ID || replay.Remaining != 4 {
		t.Fatal("analysis replay consumed quota")
	}
	food := *a.Food
	food.Items[0].Grams = 200
	if e := food.Validate(); e != nil {
		t.Fatal(e)
	}
	meal := core.Meal{RequestKey: "meal-one", EatenAt: "2026-11-16T23:30:00+05:00", SourceAnalysisID: a.ID, Food: food}
	var saved core.Meal
	if f.request("POST", f.diary.URL+"/meals", f.token, meal, &saved) != 201 {
		t.Fatal("save")
	}
	if saved.AnalysisSnapshot == nil || saved.AnalysisSnapshot.Food.Items[0].Grams != 150 {
		t.Fatal("original snapshot missing")
	}
	var replay core.Meal
	if f.request("POST", f.diary.URL+"/meals", f.token, meal, &replay) != 200 || replay.ID != saved.ID {
		t.Fatal("duplicate save")
	}
	other, _ := f.login(202)
	if f.request("GET", f.diary.URL+"/meals/"+saved.ID, other, nil, nil) != 404 {
		t.Fatal("meal ownership leak")
	}
	if f.request("GET", f.ai.URL+"/analyses/"+a.ID, other, nil, nil) != 404 {
		t.Fatal("analysis ownership leak")
	}
	attack := meal
	attack.RequestKey = "stolen"
	if f.request("POST", f.diary.URL+"/meals", other, attack, nil) != 404 {
		t.Fatal("foreign analysis saved")
	}
	var day core.Day
	if f.request("GET", f.diary.URL+"/days/2026-11-16", f.token, nil, &day) != 200 || len(day.Meals) != 1 || day.Total.Calories != food.Nutrition.Calories {
		t.Fatalf("wrong local day: %+v", day)
	}
	if day.Remaining == nil || day.Remaining.Calories != 2000-food.Nutrition.Calories {
		t.Fatal("remaining")
	}
	saved.Food.Name = "Исправлено"
	saved.AnalysisSnapshot = nil
	if f.request("PUT", f.diary.URL+"/meals/"+saved.ID, f.token, saved, &saved) != 200 || saved.AnalysisSnapshot == nil {
		t.Fatal("edit lost snapshot")
	}
	f.ai.Close()
	manual := core.Meal{RequestKey: "manual", EatenAt: "2026-11-16T10:00:00Z", Food: core.Food{Name: "Йогурт", Nutrition: core.Nutrition{Calories: 100, Protein: 8, Fat: 3, Carbs: 10}}}
	if f.request("POST", f.diary.URL+"/meals", f.token, manual, nil) != 201 {
		t.Fatal("manual needs AI")
	}
	if f.request("GET", f.diary.URL+"/meals/"+saved.ID, f.token, nil, nil) != 200 {
		t.Fatal("saved diary needs AI")
	}
	if f.request("DELETE", f.diary.URL+"/meals/"+saved.ID, other, nil, nil) != 404 {
		t.Fatal("foreign delete")
	}
	if f.request("DELETE", f.diary.URL+"/meals/"+saved.ID, f.token, nil, nil) != 204 {
		t.Fatal("delete")
	}
	if f.request("GET", f.diary.URL+"/days/2026-11-16", "", nil, nil) != 401 {
		t.Fatal("anonymous access")
	}
}
func TestConcurrentQuota(t *testing.T) {
	f := setup(t, analysis.Mock{})
	var wg sync.WaitGroup
	statuses := make(chan int, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := photoRequest(f.ai.URL, f.token, core.ID())
			res, e := http.DefaultClient.Do(req)
			if e != nil {
				statuses <- 0
				return
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			statuses <- res.StatusCode
		}()
	}
	wg.Wait()
	close(statuses)
	ok, limited := 0, 0
	for s := range statuses {
		switch s {
		case 200:
			ok++
		case 429:
			limited++
		default:
			t.Errorf("unexpected status %d", s)
		}
	}
	if ok != 5 || limited != 7 {
		t.Fatalf("quota raced: ok=%d limited=%d", ok, limited)
	}
}

type failing struct{}

func (failing) Analyze(context.Context, []byte, string) (*core.Food, error) {
	return nil, errors.New("provider failed")
}
func TestFailedAnalysesRefundQuota(t *testing.T) {
	f := setup(t, failing{})
	for i := 0; i < 7; i++ {
		if s, _ := f.analyze(core.ID()); s != 502 {
			t.Fatalf("status %d", s)
		}
	}
	var q struct {
		Remaining int `json:"remaining"`
	}
	f.request("GET", f.ai.URL+"/quota", f.token, nil, &q)
	if q.Remaining != 5 {
		t.Fatal("failed analysis charged")
	}
}
func TestDiaryDuringUserOutage(t *testing.T) {
	f := setup(t, analysis.Mock{})
	f.user.Close()
	var day core.Day
	if f.request("GET", f.diary.URL+"/days/2026-11-16?timezone=Asia%2FYekaterinburg", f.token, nil, &day) != 200 || !day.TargetUnavailable {
		t.Fatal("diary unavailable without user service")
	}
	if s, _ := f.analyze("outage"); s != 503 {
		t.Fatalf("AI must fail safely without profile: %d", s)
	}
}
