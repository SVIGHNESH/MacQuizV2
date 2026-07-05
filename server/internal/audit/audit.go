// Package audit appends rows to the append-only audit_log table
// (docs/08-security.md section 7). Every module writes its audit row inside
// the same transaction as the mutation it records, so the change and its
// trail commit or roll back together. The table itself rejects UPDATE and
// DELETE via triggers, so a row, once committed, is permanent.
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// Write appends one audit_log row inside the caller's transaction.
func Write(ctx context.Context, tx *sql.Tx, actorID, action, resourceType, resourceID string, detail map[string]any) error {
	var detailJSON any
	if detail != nil {
		b, err := json.Marshal(detail)
		if err != nil {
			return fmt.Errorf("marshal audit detail: %w", err)
		}
		detailJSON = b
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_log (actor_id, action, resource_type, resource_id, detail)
		 VALUES ($1, $2, $3, $4, $5)`,
		actorID, action, resourceType, resourceID, detailJSON); err != nil {
		return fmt.Errorf("write audit row: %w", err)
	}
	return nil
}
