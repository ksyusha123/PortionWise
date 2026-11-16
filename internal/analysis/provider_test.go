package analysis

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestYandexImageContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer key" {
			t.Error("bad upstream request")
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("request JSON")
		}
		messages := body["messages"].([]any)
		parts := messages[1].(map[string]any)["content"].([]any)
		if parts[1].(map[string]any)["type"] != "image_url" {
			t.Error("missing image")
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": `{"is_food":true,"name":"Рис","items":[{"name":"Рис","grams":200,"per_100g":{"calories":130,"protein":2.7,"fat":0.3,"carbs":28}}]}`}}}})
	}))
	defer server.Close()
	p := Yandex{URL: server.URL, Key: "key", Model: "test", HTTP: server.Client()}
	f, e := p.Analyze(context.Background(), []byte("image"), "image/png")
	if e != nil {
		t.Fatal(e)
	}
	if f.Nutrition.Calories != 260 {
		t.Fatal("portion calculation")
	}
}
