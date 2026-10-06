package main

import (
	"database/sql"
	"log"
	"os"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"
)

func main() {
	db, _ := sql.Open("postgres", os.Getenv("DATABASE_URL"))

	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/health", func(c *gin.Context) { c.JSON(200, gin.H{"status": "ok"}) })

	r.POST("/v1/check", func(c *gin.Context) {
		var req struct {
			SagaID      string `json:"saga_id"`
			Amount      int64  `json:"amount"`
			FromAccount string `json:"from_account"`
			ToAccount   string `json:"to_account"`
		}
		c.ShouldBindJSON(&req)

		// Rules: decline fraud account, amounts > 1,000,000 cents, or force_decline
		declined := req.FromAccount == "fraud" || req.ToAccount == "fraud" || req.Amount > 1_000_000

		db.Exec(`INSERT INTO risk_checks (saga_id, amount, from_account, approved) VALUES ($1,$2,$3,$4)`,
			req.SagaID, req.Amount, req.FromAccount, !declined)

		c.JSON(200, gin.H{"saga_id": req.SagaID, "approved": !declined})
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Fatal(r.Run(":" + port))
}
