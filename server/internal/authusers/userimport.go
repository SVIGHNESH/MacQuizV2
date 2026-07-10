package authusers

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"strings"

	"golang.org/x/sync/errgroup"

	"macquiz/server/internal/tabular"
)

// Roster import limits (docs/04-api.md: POST /users/import). A school year's
// intake fits comfortably; anything bigger should arrive as several files so
// one bad row does not hold five thousand accounts hostage.
const (
	MaxUserImportBytes = 1 << 20 // 1 MB
	MaxUserImportRows  = 500
)

// userImportColumns is the fixed roster header: one row per account, the
// same three fields single-account provisioning takes.
var userImportColumns = []string{"role", "email", "full_name"}

// UserImportRowError is one validation failure against a specific row/column
// of a roster file, the synchronous sibling of quiz.ImportRowError.
type UserImportRowError struct {
	Row     int    `json:"row"` // 1-based, header excluded
	Column  string `json:"column"`
	Message string `json:"message"`
}

// UserImportRow is one parsed, not-yet-validated-against-the-database roster
// row.
type UserImportRow struct {
	Row      int
	Role     string
	Email    string
	FullName string
}

// ImportedUser pairs a created account with its generated one-time
// credential, which exists only in the import response - never stored or
// logged in the clear, same contract as CreateUser.
type ImportedUser struct {
	User            User
	InitialPassword string
}

// ParseUserImportFile parses a roster upload (CSV or XLSX, told apart by the
// file's own bytes) into rows plus a per-row/column error report. The
// returned error is non-nil only when the file is unreadable as a roster at
// all (malformed, missing a required column); every per-row problem surfaces
// as a UserImportRowError instead. Rows whose cells are all empty are
// skipped - spreadsheet software pads sheets with phantom trailing rows -
// but still counted, so reported row numbers match what the admin sees.
func ParseUserImportFile(r io.Reader) ([]UserImportRow, []UserImportRowError, error) {
	records, err := tabular.Records(r)
	if err != nil {
		return nil, nil, err
	}

	col := make(map[string]int, len(records[0]))
	for i, h := range records[0] {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	for _, want := range userImportColumns {
		if _, ok := col[want]; !ok {
			return nil, nil, fmt.Errorf("missing required column %q", want)
		}
	}
	get := func(rec []string, name string) string {
		i := col[name]
		if i >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[i])
	}

	var rows []UserImportRow
	var errs []UserImportRowError
	seenEmails := map[string]int{} // lowercased email -> first row

	rowNum := 0
	for _, rec := range records[1:] {
		rowNum++
		if rowNum > MaxUserImportRows {
			errs = append(errs, UserImportRowError{
				Row:     rowNum,
				Column:  "file",
				Message: fmt.Sprintf("file exceeds the %d row limit", MaxUserImportRows),
			})
			break
		}

		// An unquoted comma splits a cell and shifts every later column
		// right; name that instead of blaming the shifted cells.
		if msg, misaligned := tabular.ExcessCells(rec, len(records[0])); misaligned {
			errs = append(errs, UserImportRowError{Row: rowNum, Column: "row", Message: msg})
			continue
		}

		role := strings.ToLower(get(rec, "role"))
		email := get(rec, "email")
		fullName := get(rec, "full_name")
		if role == "" && email == "" && fullName == "" {
			continue
		}

		var rowErrs []UserImportRowError
		// Admin accounts are created only by the operator bootstrap, never
		// over the API - the same rule as single-account provisioning.
		if role != "teacher" && role != "student" {
			rowErrs = append(rowErrs, UserImportRowError{Row: rowNum, Column: "role", Message: "must be teacher or student"})
		}
		if !emailShape.MatchString(email) {
			rowErrs = append(rowErrs, UserImportRowError{Row: rowNum, Column: "email", Message: "must be a valid email address"})
		} else {
			// users.email is citext, so the file-level duplicate check is
			// case-insensitive too.
			norm := strings.ToLower(email)
			if first, dup := seenEmails[norm]; dup {
				rowErrs = append(rowErrs, UserImportRowError{Row: rowNum, Column: "email", Message: fmt.Sprintf("duplicate of row %d", first)})
			} else {
				seenEmails[norm] = rowNum
			}
		}
		if fullName == "" {
			rowErrs = append(rowErrs, UserImportRowError{Row: rowNum, Column: "full_name", Message: "required"})
		}

		if len(rowErrs) > 0 {
			errs = append(errs, rowErrs...)
			continue
		}
		rows = append(rows, UserImportRow{Row: rowNum, Role: role, Email: email, FullName: fullName})
	}

	return rows, errs, nil
}

// ImportUsers provisions every roster row in one transaction - one taken
// email and nothing is created, matching the question import's
// all-or-nothing contract, so a re-upload of the fixed file never has to
// reason about which rows already went through. Emails already in use come
// back as row errors (not an error return), one per offending row.
func (s *Service) ImportUsers(ctx context.Context, actor User, rows []UserImportRow) ([]ImportedUser, []UserImportRowError, error) {
	// Argon2id is deliberately expensive (~19 MiB and tens of milliseconds
	// per hash), so a full roster is hashed across cores before the
	// transaction opens - the database never waits on key derivation.
	type credential struct{ password, hash string }
	creds := make([]credential, len(rows))
	g, _ := errgroup.WithContext(ctx)
	g.SetLimit(runtime.GOMAXPROCS(0))
	for i := range rows {
		g.Go(func() error {
			password, err := generatePassword()
			if err != nil {
				return err
			}
			hash, err := HashPassword(password)
			if err != nil {
				return fmt.Errorf("hash generated password: %w", err)
			}
			creds[i] = credential{password: password, hash: hash}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin import-users tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	// Pre-check taken emails inside the tx so each one becomes a
	// row-targeted error the admin can act on. emailShape admits no
	// whitespace, so a newline join is an unambiguous array encoding
	// (database/sql has no native slice binding).
	emails := make([]string, len(rows))
	for i, row := range rows {
		emails[i] = row.Email
	}
	taken := map[string]bool{}
	takenRows, err := tx.QueryContext(ctx,
		`SELECT lower(email::text) FROM users
		 WHERE email = ANY(string_to_array($1, E'\n')::citext[])`,
		strings.Join(emails, "\n"))
	if err != nil {
		return nil, nil, fmt.Errorf("check roster emails: %w", err)
	}
	for takenRows.Next() {
		var email string
		if err := takenRows.Scan(&email); err != nil {
			takenRows.Close()
			return nil, nil, fmt.Errorf("scan taken email: %w", err)
		}
		taken[email] = true
	}
	if err := takenRows.Close(); err != nil {
		return nil, nil, fmt.Errorf("check roster emails: %w", err)
	}
	var errs []UserImportRowError
	for _, row := range rows {
		if taken[strings.ToLower(row.Email)] {
			errs = append(errs, UserImportRowError{Row: row.Row, Column: "email", Message: "already in use"})
		}
	}
	if len(errs) > 0 {
		return nil, errs, nil
	}

	created := make([]ImportedUser, 0, len(rows))
	for i, row := range rows {
		u, _, err := scanUser(tx.QueryRowContext(ctx,
			`INSERT INTO users (role, email, password_hash, full_name, created_by)
			 VALUES ($1, $2, $3, $4, $5)
			 RETURNING `+userColumns+`, password_hash`,
			row.Role, row.Email, creds[i].hash, row.FullName, actor.ID))
		// A unique violation here means the email check above lost a race
		// with a concurrent create; the whole file rolls back.
		if isUniqueViolation(err) {
			return nil, nil, ErrEmailTaken
		}
		if err != nil {
			return nil, nil, fmt.Errorf("insert roster user (row %d): %w", row.Row, err)
		}
		if err := writeAudit(ctx, tx, actor.ID, "users.created", "user", u.ID,
			map[string]any{"role": row.Role, "email": row.Email, "via": "roster_import"}); err != nil {
			return nil, nil, err
		}
		created = append(created, ImportedUser{User: u, InitialPassword: creds[i].password})
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit import users: %w", err)
	}
	return created, nil, nil
}
