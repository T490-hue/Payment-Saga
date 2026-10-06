// Package mq wraps the message broker behind a small interface so the
// orchestrator doesn't depend on a specific broker. RabbitMQ is the
// implemented backend; the same seam could host SQS.
package mq

import (
	"encoding/json"
	"fmt"
	"log"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	ExchangeName = "payments"
	DLXName      = "payments.dlx"
	QueueName    = "notification.transfer"
	DLQName      = "notification.transfer.dlq"
)

// Publisher publishes events to the broker.
type Publisher interface {
	Publish(eventType string, payload interface{}) error
}

// RabbitMQ implements Publisher.
type RabbitMQ struct {
	conn *amqp.Connection
	ch   *amqp.Channel
}

func NewRabbitMQ(url string) (*RabbitMQ, error) {
	conn, err := amqp.Dial(url)
	if err != nil {
		return nil, fmt.Errorf("rabbitmq dial: %w", err)
	}
	ch, err := conn.Channel()
	if err != nil {
		return nil, fmt.Errorf("rabbitmq channel: %w", err)
	}

	// Dead-letter exchange
	if err := ch.ExchangeDeclare(DLXName, "fanout", true, false, false, false, nil); err != nil {
		return nil, err
	}
	// DLQ
	if _, err := ch.QueueDeclare(DLQName, true, false, false, false, nil); err != nil {
		return nil, err
	}
	if err := ch.QueueBind(DLQName, "", DLXName, false, nil); err != nil {
		return nil, err
	}
	// Main exchange
	if err := ch.ExchangeDeclare(ExchangeName, "topic", true, false, false, false, nil); err != nil {
		return nil, err
	}
	// Main queue with DLX and retry limit
	args := amqp.Table{
		"x-dead-letter-exchange": DLXName,
		"x-max-delivery-count":   5, // after 5 nacks → DLQ
	}
	if _, err := ch.QueueDeclare(QueueName, true, false, false, false, args); err != nil {
		return nil, err
	}
	if err := ch.QueueBind(QueueName, "transfer.*", ExchangeName, false, nil); err != nil {
		return nil, err
	}

	return &RabbitMQ{conn: conn, ch: ch}, nil
}

func (r *RabbitMQ) Publish(eventType string, payload interface{}) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return r.ch.Publish(ExchangeName, eventType, false, false, amqp.Publishing{
		ContentType:  "application/json",
		DeliveryMode: amqp.Persistent,
		Body:         body,
	})
}

// StartConsumer reads from the main queue and calls handler per message.
// The routing key (e.g. "transfer.committed", "transfer.failed") is passed
// so the handler can distinguish event types.
// Acks on success, nacks (requeue=false) on failure so RabbitMQ
// handles retries and eventual dead-lettering.
func StartConsumer(url string, handler func(routingKey string, body []byte) error) error {
	conn, err := amqp.Dial(url)
	if err != nil {
		return err
	}
	ch, err := conn.Channel()
	if err != nil {
		return err
	}
	msgs, err := ch.Consume(QueueName, "", false, false, false, false, nil)
	if err != nil {
		return err
	}
	for d := range msgs {
		if err := handler(d.RoutingKey, d.Body); err != nil {
			log.Printf("notification handler error: %v — nacking", err)
			d.Nack(false, false) // requeue=false → retry via RabbitMQ
		} else {
			d.Ack(false)
		}
	}
	return nil
}
