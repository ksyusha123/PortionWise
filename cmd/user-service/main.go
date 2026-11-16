package main

import (
	"fooddiary/internal/core"
	"fooddiary/internal/users"
)

func main() {
	db, e := core.OpenDB(core.Env("DB_PATH", "data/users.db"), users.Schema)
	if e != nil {
		panic(e)
	}
	defer db.Close()
	s := users.Service{DB: db, Secret: core.Secret("AUTH_SECRET"), InternalKey: core.Secret("INTERNAL_KEY")}
	core.Serve(core.Env("ADDR", ":8081"), s.Routes())
}
