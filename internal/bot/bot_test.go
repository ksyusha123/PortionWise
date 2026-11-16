package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"fooddiary/internal/analysis"
	"fooddiary/internal/core"
	"fooddiary/internal/diary"
	"fooddiary/internal/users"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestTelegramConversationPersistenceAndPhoto(t *testing.T) {
	const secret = "test-secret-with-at-least-32-characters"
	udb, e := core.OpenDB(filepath.Join(t.TempDir(), "users.db"), users.Schema)
	if e != nil {
		t.Fatal(e)
	}
	defer udb.Close()
	us := &users.Service{DB: udb, Secret: secret, InternalKey: secret}
	u := httptest.NewServer(us.Routes())
	defer u.Close()
	adb, e := core.OpenDB(filepath.Join(t.TempDir(), "analysis.db"), analysis.Schema)
	if e != nil {
		t.Fatal(e)
	}
	defer adb.Close()
	as := &analysis.Service{DB: adb, Secret: secret, UserURL: u.URL, Client: core.NewClient(time.Second), Provider: analysis.Mock{}}
	a := httptest.NewServer(as.Routes())
	defer a.Close()
	ddb, e := core.OpenDB(filepath.Join(t.TempDir(), "diary.db"), diary.Schema)
	if e != nil {
		t.Fatal(e)
	}
	defer ddb.Close()
	ds := &diary.Service{DB: ddb, Secret: secret, UserURL: u.URL, AnalysisURL: a.URL, Client: core.NewClient(time.Second)}
	d := httptest.NewServer(ds.Routes())
	defer d.Close()
	bdb, e := core.OpenDB(filepath.Join(t.TempDir(), "bot.db"), Schema)
	if e != nil {
		t.Fatal(e)
	}
	defer bdb.Close()
	var sent []string
	var photo bytes.Buffer
	_ = png.Encode(&photo, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	tg := &Telegram{Token: "test", HTTP: &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		payload := `{"ok":true,"result":{}}`
		if strings.HasSuffix(r.URL.Path, "getFile") {
			payload = `{"ok":true,"result":{"file_path":"photos/test.png","file_size":100}}`
		} else if strings.HasPrefix(r.URL.Path, "/file/") {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(photo.Bytes())), Header: make(http.Header)}, nil
		} else if strings.HasSuffix(r.URL.Path, "sendMessage") {
			var v struct {
				Text string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&v)
			sent = append(sent, v.Text)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(payload)), Header: make(http.Header)}, nil
	})}}
	b := &Bot{DB: bdb, TG: tg, Client: core.NewClient(time.Second), UserURL: u.URL, AnalysisURL: a.URL, DiaryURL: d.URL, InternalKey: secret}
	ctx := context.Background()
	id := int64(0)
	send := func(text, action string, isPhoto bool) {
		t.Helper()
		id++
		m := &Message{From: TGUser{ID: 123, FirstName: "Анна"}, Text: text}
		m.Chat.ID = 123
		m.Chat.Type = "private"
		if isPhoto {
			m.Photo = append(m.Photo, struct {
				ID   string `json:"file_id"`
				Size int    `json:"file_size"`
			}{"photo", 100})
		}
		update := Update{ID: id, Message: m}
		if action != "" {
			update.Message = nil
			update.Callback = &struct {
				ID      string   `json:"id"`
				From    TGUser   `json:"from"`
				Message *Message `json:"message"`
				Data    string   `json:"data"`
			}{ID: "cb", From: m.From, Message: m, Data: action}
		}
		if e := b.handle(ctx, update); e != nil {
			t.Fatal(e)
		}
	}
	send("/start", "", false)
	send("", "manual", false)
	send("Йогурт", "", false)
	// Recreate the bot process object: dialog is recovered from its own SQLite storage.
	copyBot := *b
	b = &copyBot
	send("120 8 4 13", "", false)
	s, e := b.load(ctx, 123)
	if e != nil || s.Draft == nil || s.Step != "draft" {
		t.Fatal("draft not persisted")
	}
	send("", "save", false)
	s, _ = b.load(ctx, 123)
	if s.Draft != nil {
		t.Fatal("saved draft still active")
	}
	var day core.Day
	date := time.Now().UTC().Format("2006-01-02")
	if e = b.api(ctx, "GET", d.URL+"/days/"+date, s.Token, nil, &day); e != nil || len(day.Meals) != 1 {
		t.Fatalf("manual meal missing: %v %+v", e, day)
	}
	send("", "edit:"+day.Meals[0].ID, false)
	send("", "nut", false)
	send("150 10 5 16", "", false)
	send("", "save", false)
	send("", "photo", false)
	send("", "", true)
	s, _ = b.load(ctx, 123)
	if s.Draft == nil || s.Draft.SourceAnalysisID == "" {
		t.Fatalf("photo draft missing; messages=%v", sent)
	}
	send("", "weights", false)
	send("1 200", "", false)
	s, _ = b.load(ctx, 123)
	if s.Draft.Food.Items[0].Grams != 200 {
		t.Fatal("weight not edited")
	}
	send("", "save", false)
	s, _ = b.load(ctx, 123)
	if e = b.api(ctx, "GET", d.URL+"/days/"+date, s.Token, nil, &day); e != nil || len(day.Meals) != 2 {
		t.Fatal("photo meal missing")
	}
	send("", "confirmdelete:"+day.Meals[0].ID, false)
	if e = b.api(ctx, "GET", d.URL+"/days/"+date, s.Token, nil, &day); e != nil || len(day.Meals) != 1 {
		t.Fatal("delete failed")
	}
	send("", "today", false)
	if len(sent) == 0 || !strings.Contains(sent[len(sent)-1], "Всего:") {
		t.Fatal("day UI missing")
	}
}
