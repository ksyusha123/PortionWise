package main

import (
	"fooddiary/internal/core"
	"fooddiary/internal/diary"
	"time"
)

func main() {
	db, e := core.OpenDB(core.Env("DB_PATH", "data/diary.db"), diary.Schema)
	if e != nil {
		panic(e)
	}
	defer db.Close()
	s := diary.Service{DB: db, Secret: core.Secret("AUTH_SECRET"), UserURL: core.Env("USER_URL", "http://localhost:8081"), AnalysisURL: core.Env("ANALYSIS_URL", "http://localhost:8082"), Client: core.NewClient(5 * time.Second)}
	core.Serve(core.Env("ADDR", ":8083"), s.Routes())
}
