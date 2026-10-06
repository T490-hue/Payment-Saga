package main

import (
	"database/sql"
	"log"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
)

var db *sql.DB

func main() {
	var err error
	db, err = sql.Open("postgres", os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)

	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/health", func(c *gin.Context) { c.JSON(200, gin.H{"status": "ok"}) })

	// All /v1/* routes require internal auth
	v1 := r.Group("/v1", internalAuth())
	v1.GET("/accounts/:id", getAccount)
	v1.POST("/reserve", reserve)
	v1.POST("/commit", commit)
	v1.POST("/release", release)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Fatal(r.Run(":" + port))
}

func internalAuth() gin.HandlerFunc {
	token := os.Getenv("INTERNAL_TOKEN")
	return func(c *gin.Context) {
		if token != "" && c.GetHeader("X-Internal-Token") != token {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		c.Next()
	}
}

func getAccount(c *gin.Context) {
	id := c.Param("id")
	var available, reserved int64
	if err := db.QueryRow(`SELECT available_cents, reserved_cents FROM accounts WHERE id=$1`, id).
		Scan(&available, &reserved); err != nil {
		c.JSON(404, gin.H{"error": "account not found"})
		return
	}
	c.JSON(200, gin.H{"id": id, "available_cents": available, "reserved_cents": reserved})
}

func reserve(c *gin.Context) {
	var req struct {
		SagaID    string `json:"saga_id"`
		AccountID string `json:"account_id"`
		Amount    int64  `json:"amount"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	// Idempotent: if a hold already exists for this saga, return it
	var existing int64
	if err := db.QueryRow(`SELECT amount FROM reservations WHERE saga_id=$1`, req.SagaID).
		Scan(&existing); err == nil {
		c.JSON(200, gin.H{"saga_id": req.SagaID, "reserved": existing})
		return
	}

	tx, err := db.Begin()
	if err != nil {
		c.JSON(500, gin.H{"error": "db error"})
		return
	}
	defer tx.Rollback()

	// SELECT FOR UPDATE serializes concurrent transfers on the same account
	var available int64
	if err := tx.QueryRow(`SELECT available_cents FROM accounts WHERE id=$1 FOR UPDATE`, req.AccountID).
		Scan(&available); err != nil {
		c.JSON(422, gin.H{"error": "account not found"})
		return
	}
	if available < req.Amount {
		c.JSON(422, gin.H{"error": "insufficient funds"})
		return
	}

	if _, err := tx.Exec(`UPDATE accounts SET available_cents=available_cents-$1, reserved_cents=reserved_cents+$1 WHERE id=$2`, req.Amount, req.AccountID); err != nil {
		c.JSON(500, gin.H{"error": "db error"})
		return
	}
	if _, err := tx.Exec(`INSERT INTO reservations (saga_id, account_id, amount) VALUES ($1,$2,$3)`, req.SagaID, req.AccountID, req.Amount); err != nil {
		c.JSON(500, gin.H{"error": "db error"})
		return
	}
	if _, err := tx.Exec(`INSERT INTO ledger_entries (saga_id, account_id, amount, entry_type) VALUES ($1,$2,$3,'hold')`, req.SagaID, req.AccountID, req.Amount); err != nil {
		c.JSON(500, gin.H{"error": "db error"})
		return
	}
	if err := tx.Commit(); err != nil {
		c.JSON(500, gin.H{"error": "commit failed"})
		return
	}
	c.JSON(201, gin.H{"saga_id": req.SagaID, "reserved": req.Amount})
}

func commit(c *gin.Context) {
	var req struct {
		SagaID    string `json:"saga_id"`
		AccountID string `json:"account_id"` // from
		ToAccount string `json:"to_account"`
		Amount    int64  `json:"amount"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	// Idempotent commit
	var status string
	if err := db.QueryRow(`SELECT status FROM reservations WHERE saga_id=$1`, req.SagaID).Scan(&status); err != nil {
		c.JSON(404, gin.H{"error": "reservation not found"})
		return
	}
	if status == "committed" {
		c.JSON(200, gin.H{"saga_id": req.SagaID, "status": "committed"})
		return
	}
	if status == "released" {
		c.JSON(409, gin.H{"error": "cannot commit a released reservation"})
		return
	}

	tx, err := db.Begin()
	if err != nil {
		c.JSON(500, gin.H{"error": "db error"})
		return
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`UPDATE accounts SET reserved_cents=reserved_cents-$1 WHERE id=$2`, req.Amount, req.AccountID); err != nil {
		c.JSON(500, gin.H{"error": "db error"})
		return
	}
	if _, err := tx.Exec(`UPDATE accounts SET available_cents=available_cents+$1 WHERE id=$2`, req.Amount, req.ToAccount); err != nil {
		c.JSON(500, gin.H{"error": "db error"})
		return
	}
	if _, err := tx.Exec(`UPDATE reservations SET status='committed' WHERE saga_id=$1`, req.SagaID); err != nil {
		c.JSON(500, gin.H{"error": "db error"})
		return
	}
	if _, err := tx.Exec(`INSERT INTO ledger_entries (saga_id, account_id, amount, entry_type) VALUES ($1,$2,$3,'commit_debit')`, req.SagaID, req.AccountID, req.Amount); err != nil {
		c.JSON(500, gin.H{"error": "db error"})
		return
	}
	if _, err := tx.Exec(`INSERT INTO ledger_entries (saga_id, account_id, amount, entry_type) VALUES ($1,$2,$3,'commit_credit')`, req.SagaID, req.ToAccount, req.Amount); err != nil {
		c.JSON(500, gin.H{"error": "db error"})
		return
	}
	if err := tx.Commit(); err != nil {
		c.JSON(500, gin.H{"error": "commit failed"})
		return
	}
	c.JSON(200, gin.H{"saga_id": req.SagaID, "status": "committed"})
}

func release(c *gin.Context) {
	var req struct {
		SagaID    string `json:"saga_id"`
		AccountID string `json:"account_id"`
		Amount    int64  `json:"amount"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	var status string
	if err := db.QueryRow(`SELECT status FROM reservations WHERE saga_id=$1`, req.SagaID).Scan(&status); err != nil {
		c.JSON(404, gin.H{"error": "reservation not found"})
		return
	}
	if status == "committed" {
		c.JSON(409, gin.H{"error": "cannot release a committed reservation"})
		return
	}
	if status == "released" {
		c.JSON(200, gin.H{"saga_id": req.SagaID, "status": "released"})
		return
	}

	tx, err := db.Begin()
	if err != nil {
		c.JSON(500, gin.H{"error": "db error"})
		return
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`UPDATE accounts SET available_cents=available_cents+$1, reserved_cents=reserved_cents-$1 WHERE id=$2`, req.Amount, req.AccountID); err != nil {
		c.JSON(500, gin.H{"error": "db error"})
		return
	}
	if _, err := tx.Exec(`UPDATE reservations SET status='released' WHERE saga_id=$1`, req.SagaID); err != nil {
		c.JSON(500, gin.H{"error": "db error"})
		return
	}
	if err := tx.Commit(); err != nil {
		c.JSON(500, gin.H{"error": "commit failed"})
		return
	}
	c.JSON(200, gin.H{"saga_id": req.SagaID, "status": "released"})
}
