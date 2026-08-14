package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "github.com/glebarez/sqlite"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: q <dbpath>")
		os.Exit(1)
	}
	db, err := sql.Open("sqlite", os.Args[1])
	if err != nil {
		fmt.Println("open err:", err)
		os.Exit(1)
	}
	defer db.Close()
	rows, err := db.Query(
		"select id,event_type,level,message from system_logs "+
			"where event_type='system.recordings_size' order by id desc limit 10")
	if err != nil {
		fmt.Println("query err:", err)
		os.Exit(1)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var et, lvl, msg string
		if err := rows.Scan(&id, &et, &lvl, &msg); err != nil {
			fmt.Println("scan err:", err)
			os.Exit(1)
		}
		fmt.Printf("id=%d level=%s type=%s msg=%s\n", id, lvl, et, msg)
	}
}