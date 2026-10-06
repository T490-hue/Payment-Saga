package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"

	"github.com/T490-hue/Payment-Saga/internal/mq"
	"github.com/T490-hue/Payment-Saga/internal/saga"
	"github.com/T490-hue/Payment-Saga/internal/store"
)

var (
	db        *sql.DB
	idem      *store.IdempotencyStore
	queue     mq.Publisher
	walletURL string
	riskURL   string
)

func main() {
	var err error
	db, err = sql.Open("postgres", os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(25)

	rdb := redis.NewClient(&redis.Options{Addr: os.Getenv("REDIS_ADDR"), PoolSize: 20})
	idem = store.NewIdempotencyStore(db, rdb)

	queue, err = mq.NewRabbitMQ(os.Getenv("RABBITMQ_URL"))
	if err != nil {
		log.Fatalf("rabbitmq: %v", err)
	}

	walletURL = os.Getenv("WALLET_URL")
	riskURL = os.Getenv("RISK_URL")

	// On startup, resume any non-terminal sagas that were in-flight before a crash
	go recoverSagas()

	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/health", func(c *gin.Context) { c.JSON(200, gin.H{"status": "ok"}) })
	r.POST("/v1/transfers", handleTransfer)
	r.GET("/v1/transfers", listTransfers)
	r.GET("/v1/transfers/:id", getTransfer)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("orchestrator starting on :%s  idempotency_cache=%s", port, os.Getenv("IDEMPOTENCY_CACHE"))
	log.Fatal(r.Run(":" + port))
}

func handleTransfer(c *gin.Context) {
	key := c.GetHeader("Idempotency-Key")
	if key == "" {
		c.JSON(400, gin.H{"error": "Idempotency-Key header required"})
		return
	}

	// Check idempotency — Redis first (if mode=redis), else Postgres directly
	if cached, ok := idem.Get(context.Background(), key); ok {
		var t saga.Transfer
		json.Unmarshal([]byte(cached), &t)
		c.JSON(200, t)
		return
	}

	var req struct {
		FromAccountID string `json:"from_account_id" binding:"required"`
		ToAccountID   string `json:"to_account_id" binding:"required"`
		AmountCents   int64  `json:"amount_cents" binding:"required,gt=0"`
		Currency      string `json:"currency" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if req.FromAccountID == req.ToAccountID {
		c.JSON(400, gin.H{"error": "from and to accounts must differ"})
		return
	}

	id := uuid.New().String()
	t := saga.Transfer{
		ID: id, FromAccountID: req.FromAccountID,
		ToAccountID: req.ToAccountID, AmountCents: req.AmountCents,
		Currency: req.Currency, Status: saga.StatusStarted,
		IdempotencyKey: key,
	}

	// Persist saga row — this is what lets us resume after a crash
	if err := persistSaga(t); err != nil {
		c.JSON(500, gin.H{"error": "db error"})
		return
	}

	result, httpStatus := runSaga(t)
	idem.Set(context.Background(), key, result)
	c.JSON(httpStatus, result)
}

// runSaga walks the saga state machine. Every step updates the saga row
// before making a downstream call, so a crash always has a row to resume from.
func runSaga(t saga.Transfer) (saga.Transfer, int) {
	// Step 1: Reserve
	if err := callWallet("reserve", t); err != nil {
		t.Status = saga.StatusFailed
		updateSagaStatus(t.ID, t.Status)
		return t, 422
	}
	t.Status = saga.StatusReserved
	updateSagaStatus(t.ID, t.Status)

	// Support crash simulation: pause after hold so demo can kill the process
	if os.Getenv("PAUSE_AFTER") == "reserved" {
		log.Printf("PAUSE_AFTER=reserved — sleeping 30s to allow crash simulation")
		time.Sleep(30 * time.Second)
	}

	// Step 2: Risk check
	approved, err := callRisk(t)
	if err != nil {
		// Risk unreachable — keep hold, return 202 so client can retry
		return t, 202
	}
	if !approved {
		// Risk declined — compensate
		t.Status = saga.StatusCompensating
		updateSagaStatus(t.ID, t.Status)
		callWallet("release", t)
		t.Status = saga.StatusCompensated
		updateSagaStatus(t.ID, t.Status)
		publishEvent("transfer.failed", t)
		return t, 422
	}
	t.Status = saga.StatusRiskApproved
	updateSagaStatus(t.ID, t.Status)

	// Step 3: Commit — write outbox row in same transaction as saga status
	if err := commitWithOutbox(t); err != nil {
		callWallet("release", t)
		t.Status = saga.StatusCompensated
		updateSagaStatus(t.ID, t.Status)
		return t, 500
	}
	t.Status = saga.StatusCommitted
	return t, 201
}

// recoverSagas finds sagas left in non-terminal states after a crash and resumes them.
func recoverSagas() {
	time.Sleep(2 * time.Second)
	rows, err := db.Query(`
		SELECT id, from_account_id, to_account_id, amount_cents, currency, status, idempotency_key
		FROM sagas WHERE status NOT IN ($1,$2,$3,$4)`,
		saga.StatusCommitted, saga.StatusCompensated, saga.StatusFailed, saga.StatusStarted,
	)
	if err != nil {
		log.Printf("recovery: query error: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var t saga.Transfer
		rows.Scan(&t.ID, &t.FromAccountID, &t.ToAccountID, &t.AmountCents, &t.Currency, &t.Status, &t.IdempotencyKey)
		log.Printf("recovery: resuming saga %s from status=%s", t.ID, t.Status)
		switch t.Status {
		case saga.StatusReserved:
			// Resume from risk check
			approved, err := callRisk(t)
			if err == nil && approved {
				commitWithOutbox(t)
			} else if err == nil && !approved {
				callWallet("release", t)
				updateSagaStatus(t.ID, saga.StatusCompensated)
			}
		case saga.StatusRiskApproved:
			commitWithOutbox(t)
		case saga.StatusCompensating:
			callWallet("release", t)
			updateSagaStatus(t.ID, saga.StatusCompensated)
		}
	}
}

// --- helpers ---

func callWallet(action string, t saga.Transfer) error {
	payload, _ := json.Marshal(map[string]interface{}{
		"saga_id":    t.ID,
		"account_id": t.FromAccountID,
		"to_account": t.ToAccountID,
		"amount":     t.AmountCents,
	})
	url := fmt.Sprintf("%s/v1/%s", walletURL, action)
	token := os.Getenv("INTERNAL_TOKEN")

	// Retry with exponential backoff — fresh request each attempt
	// because http.Client.Do() consumes the request body
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(500*(1<<attempt)) * time.Millisecond)
		}
		req, err := http.NewRequest("POST", url, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", t.ID+"-"+action)
		req.Header.Set("X-Internal-Token", token)

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == 200 || resp.StatusCode == 201 {
			return nil
		}
		lastErr = fmt.Errorf("wallet %s returned %d", action, resp.StatusCode)
	}
	return lastErr
}

func callRisk(t saga.Transfer) (bool, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"saga_id":      t.ID,
		"amount":       t.AmountCents,
		"from_account": t.FromAccountID,
		"to_account":   t.ToAccountID,
	})
	resp, err := http.Post(riskURL+"/v1/check", "application/json", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 503 {
		return false, fmt.Errorf("risk unavailable")
	}
	var result struct{ Approved bool `json:"approved"` }
	json.NewDecoder(resp.Body).Decode(&result)
	return result.Approved, nil
}

func commitWithOutbox(t saga.Transfer) error {
	// Call wallet commit FIRST — moves money before we record success
	if err := callWallet("commit", t); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	body, _ := json.Marshal(t)
	_, err = tx.Exec(`UPDATE sagas SET status=$1 WHERE id=$2`, saga.StatusCommitted, t.ID)
	if err != nil {
		tx.Rollback()
		return err
	}
	_, err = tx.Exec(`INSERT INTO outbox (event_type, payload) VALUES ($1,$2)`,
		"transfer.committed", string(body))
	if err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Best-effort publish — outbox row guarantees eventual delivery even on crash
	publishEvent("transfer.committed", t)
	return nil
}

func publishEvent(eventType string, t saga.Transfer) {
	if err := queue.Publish(eventType, t); err != nil {
		log.Printf("publish %s: %v (outbox row exists, relay will retry)", eventType, err)
	}
}

func persistSaga(t saga.Transfer) error {
	_, err := db.Exec(`
		INSERT INTO sagas (id, from_account_id, to_account_id, amount_cents, currency, status, idempotency_key)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		t.ID, t.FromAccountID, t.ToAccountID, t.AmountCents, t.Currency, t.Status, t.IdempotencyKey,
	)
	return err
}

func updateSagaStatus(id, status string) {
	if _, err := db.Exec(`UPDATE sagas SET status=$1, updated_at=now() WHERE id=$2`, status, id); err != nil {
		log.Printf("updateSagaStatus(%s, %s): %v", id, status, err)
	}
}

func listTransfers(c *gin.Context) {
	rows, err := db.Query(`SELECT id, from_account_id, to_account_id, amount_cents, currency, status FROM sagas ORDER BY created_at DESC LIMIT 50`)
	if err != nil {
		c.JSON(500, gin.H{"error": "db error"})
		return
	}
	defer rows.Close()
	var ts []saga.Transfer
	for rows.Next() {
		var t saga.Transfer
		rows.Scan(&t.ID, &t.FromAccountID, &t.ToAccountID, &t.AmountCents, &t.Currency, &t.Status)
		ts = append(ts, t)
	}
	if ts == nil {
		ts = []saga.Transfer{}
	}
	c.JSON(200, ts)
}

func getTransfer(c *gin.Context) {
	var t saga.Transfer
	err := db.QueryRow(`SELECT id, from_account_id, to_account_id, amount_cents, currency, status FROM sagas WHERE id=$1`,
		c.Param("id")).Scan(&t.ID, &t.FromAccountID, &t.ToAccountID, &t.AmountCents, &t.Currency, &t.Status)
	if err == sql.ErrNoRows {
		c.JSON(404, gin.H{"error": "transfer not found"})
		return
	}
	if err != nil {
		c.JSON(500, gin.H{"error": "db error"})
		return
	}
	c.JSON(200, t)
}
