package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

func TestSendMessageOptionalKeyboard(t *testing.T) {
	for _, tc := range []struct {
		name     string
		keyboard *Keyboard
	}{
		{"no keyboard", nil},
		{"inline keyboard", menu()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tg := &Telegram{Token: "test", HTTP: &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
				var payload map[string]json.RawMessage
				if e := json.NewDecoder(r.Body).Decode(&payload); e != nil {
					t.Fatal(e)
				}
				markup, exists := payload["reply_markup"]
				if tc.keyboard == nil && exists {
					t.Fatalf("reply_markup must be omitted, got %s", markup)
				}
				if tc.keyboard != nil {
					if !exists || string(markup) == "null" {
						t.Fatal("keyboard missing")
					}
					var k Keyboard
					if e := json.Unmarshal(markup, &k); e != nil || len(k.Rows) == 0 {
						t.Fatal("invalid keyboard")
					}
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{}}`)), Header: make(http.Header)}, nil
			})}}
			if e := tg.send(context.Background(), 123, "Введите суточную норму", tc.keyboard); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestTelegramErrorDescriptionAndLogs(t *testing.T) {
	var logs bytes.Buffer
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(original)
	tg := &Telegram{Token: "private-bot-token", HTTP: &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"ok":false,"error_code":400,"description":"Bad Request: invalid keyboard private-bot-token","parameters":{"retry_after":2}}`)), Header: make(http.Header)}, nil
	})}}
	e := tg.send(context.Background(), 123, "text", nil)
	if e == nil || !strings.Contains(e.Error(), "Bad Request: invalid keyboard") || !strings.Contains(e.Error(), "retry_after=2") {
		t.Fatalf("description lost: %v", e)
	}
	if !strings.Contains(logs.String(), "method=sendMessage") || !strings.Contains(logs.String(), "error_code=400") || !strings.Contains(logs.String(), "Bad Request: invalid keyboard") {
		t.Fatalf("diagnostic fields missing: %s", logs.String())
	}
	if strings.Contains(logs.String(), tg.Token) || strings.Contains(e.Error(), tg.Token) {
		t.Fatal("bot token exposed")
	}
}
