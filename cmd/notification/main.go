package main

import (
	"database/sql"
	"encoding/json"
	"log"
	"os"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq"

	"github.com/T490-hue/Payment-Saga/internal/mq"
	"github.com/T490-hue/Payment-Saga/internal/saga"
)

func main() {
	db, err := sql.Open("postgres", os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}

	// HTTP server for health + query
	r := gin.New()
	r.Use(gin.Recovery())
	r.GET("/health", func(c *gin.Context) { c.JSON(200, gin.H{"status": "ok"}) })
	r.GET("/v1/notifications/:saga_id", func(c *gin.Context) {
		var eventType, payload string
		var createdAt string
		if err := db.QueryRow(`SELECT event_type, payload, created_at FROM notifications WHERE saga_id=$1 ORDER BY created_at DESC LIMIT 1`,
			c.Param("saga_id")).Scan(&eventType, &payload, &createdAt); err != nil {
			c.JSON(404, gin.H{"error": "notification not found"})
			return
		}
		c.JSON(200, gin.H{"saga_id": c.Param("saga_id"), "event": eventType, "at": createdAt})
	})

	go func() {
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080"
		}
		r.Run(":" + port)
	}()

	// RabbitMQ consumer — dedups on saga_id + event_type so redeliveries are safe
	log.Println("notification consumer starting")
	if err := mq.StartConsumer(os.Getenv("RABBITMQ_URL"), func(routingKey string, body []byte) error {
		var t saga.Transfer
		if err := json.Unmarshal(body, &t); err != nil {
			return err
		}
		// Use routing key as event_type (transfer.committed or transfer.failed)
		eventType := routingKey
		if eventType == "" {
			eventType = "transfer.committed"
		}
		_, err := db.Exec(`
			INSERT INTO notifications (saga_id, event_type, payload)
			VALUES ($1,$2,$3)
			ON CONFLICT (saga_id, event_type) DO NOTHING`,
			t.ID, eventType, string(body),
		)
		return err
	}); err != nil {
		log.Fatalf("consumer: %v", err)
	}
}
