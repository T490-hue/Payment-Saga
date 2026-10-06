// Package saga defines the states and types shared across all four services.
package saga

// Status values for the sagas table. The orchestrator persists the current
// status before making each downstream call, so a crash leaves exactly one
// row showing where to resume.
const (
	StatusStarted     = "started"
	StatusReserved    = "reserved"
	StatusRiskApproved = "risk_approved"
	StatusCommitted   = "committed"
	StatusCompensating = "compensating"
	StatusCompensated = "compensated"
	StatusFailed      = "failed"
)

// Transfer is the public shape returned by the orchestrator.
type Transfer struct {
	ID            string `json:"id"`
	FromAccountID string `json:"from_account_id"`
	ToAccountID   string `json:"to_account_id"`
	AmountCents   int64  `json:"amount_cents"`
	Currency      string `json:"currency"`
	Status        string `json:"status"`
	IdempotencyKey string `json:"idempotency_key"`
}
