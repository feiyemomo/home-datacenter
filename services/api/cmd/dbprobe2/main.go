package main

import (
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: probe <db>")
		os.Exit(1)
	}
	db, _ := sql.Open("sqlite", os.Args[1])
	defer db.Close()
	fmt.Println("=== users ===")
	rows, err := db.Query("SELECT id, name, is_admin FROM users")
	if err != nil {
		fmt.Println("users err:", err)
	} else {
		for rows.Next() {
			var id int64
			var name string
			var admin bool
			rows.Scan(&id, &name, &admin)
			fmt.Printf("  id=%d name=%q admin=%v\n", id, name, admin)
		}
		rows.Close()
	}
	fmt.Println("=== devices ===")
	var cols []string
	crows, _ := db.Query("PRAGMA table_info(devices)")
	for crows.Next() {
		var cid int
		var n, t string
		var nn, pk int
		var d sql.NullString
		crows.Scan(&cid, &n, &t, &nn, &d, &pk)
		cols = append(cols, n)
	}
	crows.Close()
	fmt.Println("  columns:", cols)
	hasTV := false
	for _, c := range cols {
		if c == "token_version" {
			hasTV = true
		}
	}
	sel := "SELECT id, user_id FROM devices LIMIT 5"
	if hasTV {
		sel = "SELECT id, user_id, token_version FROM devices LIMIT 5"
	}
	drows, err := db.Query(sel)
	if err != nil {
		fmt.Println("dev err:", err)
		return
	}
	for drows.Next() {
		if hasTV {
			var id, uid, tv int64
			drows.Scan(&id, &uid, &tv)
			fmt.Printf("  id=%d user=%d token_version=%d\n", id, uid, tv)
		} else {
			var id, uid int64
			drows.Scan(&id, &uid)
			fmt.Printf("  id=%d user=%d\n", id, uid)
		}
	}
	drows.Close()
}