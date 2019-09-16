package main

import (
	"fmt"
	"html/template"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"strconv"

	_ "github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
	goji "goji.io"
	"goji.io/pat"
)

func init() {
	store = sessions.NewCookieStore([]byte("abc"))

	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	templates = template.Must(template.ParseFiles(
		"../public/index.html",
	))
}

func main() {
	go func() { log.Println(http.ListenAndServe(":9876", nil)) }()
	host := os.Getenv("MYSQL_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port := os.Getenv("MYSQL_PORT")
	if port == "" {
		port = "3306"
	}
	_, err := strconv.Atoi(port)
	if err != nil {
		log.Fatalf("failed to read DB port number from an environment variable MYSQL_PORT.\nError: %s", err.Error())
	}
	user := os.Getenv("MYSQL_USER")
	if user == "" {
		user = "isucari"
	}
	dbname := os.Getenv("MYSQL_DBNAME")
	if dbname == "" {
		dbname = "isucari"
	}
	password := os.Getenv("MYSQL_PASS")
	if password == "" {
		password = "isucari"
	}

	dsn := fmt.Sprintf(
		"%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=true&loc=Local",
		user,
		password,
		host,
		port,
		dbname,
	)

	dbx, err = sqlx.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("failed to connect to DB: %s.", err.Error())
	}
	defer dbx.Close()

	dbx.SetMaxOpenConns(8)
	dbx.SetMaxIdleConns(8)
	dbx.SetConnMaxLifetime(30 * time.Second)

	mux := goji.NewMux()

	// API
	mux.HandleFunc(pat.Post("/initialize"), postInitialize)
	mux.HandleFunc(pat.Get("/new_items.json"), getNewItems)
	mux.HandleFunc(pat.Get("/new_items/:root_category_id.json"), getNewCategoryItems)
	mux.HandleFunc(pat.Get("/users/transactions.json"), getTransactions)
	mux.HandleFunc(pat.Get("/users/:user_id.json"), getUserItems)
	mux.HandleFunc(pat.Get("/items/:item_id.json"), getItem)
	mux.HandleFunc(pat.Post("/items/edit"), postItemEdit)
	mux.HandleFunc(pat.Post("/buy"), postBuy)
	mux.HandleFunc(pat.Post("/sell"), postSell)
	mux.HandleFunc(pat.Post("/ship"), postShip)
	mux.HandleFunc(pat.Post("/ship_done"), postShipDone)
	mux.HandleFunc(pat.Post("/complete"), postComplete)
	mux.HandleFunc(pat.Get("/transactions/:transaction_evidence_id.png"), getQRCode)
	mux.HandleFunc(pat.Post("/bump"), postBump)
	mux.HandleFunc(pat.Get("/settings"), getSettings)
	mux.HandleFunc(pat.Post("/login"), postLogin)
	mux.HandleFunc(pat.Post("/register"), postRegister)
	mux.HandleFunc(pat.Get("/reports.json"), getReports)
	// Frontend
	mux.HandleFunc(pat.Get("/"), getIndex)
	mux.HandleFunc(pat.Get("/login"), getIndex)
	mux.HandleFunc(pat.Get("/register"), getIndex)
	mux.HandleFunc(pat.Get("/timeline"), getIndex)
	mux.HandleFunc(pat.Get("/categories/:category_id/items"), getIndex)
	mux.HandleFunc(pat.Get("/sell"), getIndex)
	mux.HandleFunc(pat.Get("/items/:item_id"), getIndex)
	mux.HandleFunc(pat.Get("/items/:item_id/edit"), getIndex)
	mux.HandleFunc(pat.Get("/items/:item_id/buy"), getIndex)
	mux.HandleFunc(pat.Get("/buy/complete"), getIndex)
	mux.HandleFunc(pat.Get("/transactions/:transaction_id"), getIndex)
	mux.HandleFunc(pat.Get("/users/:user_id"), getIndex)
	mux.HandleFunc(pat.Get("/users/setting"), getIndex)
	// Assets
	mux.Handle(pat.Get("/*"), http.FileServer(http.Dir("../public")))
	log.Fatal(http.ListenAndServe(":8000", mux))
}
