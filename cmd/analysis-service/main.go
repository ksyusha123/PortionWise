package main

import (
	"fooddiary/internal/analysis"
	"fooddiary/internal/core"
	"net/http"
	"time"
)

func main() {
	db, e := core.OpenDB(core.Env("DB_PATH", "data/analysis.db"), analysis.Schema)
	if e != nil {
		panic(e)
	}
	defer db.Close()
	var p analysis.Provider = &analysis.Yandex{URL: core.Env("YANDEX_BASE_URL", "https://ai.api.cloud.yandex.net/v1"), Key: core.Env("YANDEX_API_KEY", ""), Model: core.Env("YANDEX_MODEL", "gpt://b1gh4nec0stgefclroq9/deepseek-v4.1-flash/latest"), HTTP: &http.Client{Timeout: 85 * time.Second}}
	if core.Env("AI_MODE", "yandex") == "mock" {
		p = analysis.Mock{}
	}
	s := analysis.Service{DB: db, Secret: core.Secret("AUTH_SECRET"), UserURL: core.Env("USER_URL", "http://localhost:8081"), Client: core.NewClient(5 * time.Second), Provider: p}
	core.Serve(core.Env("ADDR", ":8082"), s.Routes())
}
