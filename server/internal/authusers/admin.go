package authusers

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Sentinel errors for the admin provisioning flows.
var (
	ErrEmailTaken   = errors.New("email already in use")
	ErrNotFound     = errors.New("not found")
	ErrSelfMutation = errors.New("admins change their own credential and status via the auth endpoints")
)

// Group is the cohort shape returned to clients.
type Group struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	MemberCount int       `json:"member_count"`
	CreatedAt   time.Time `json:"created_at"`
}

// UserPatch carries the optional PATCH /users/:id mutations; nil means
// "leave unchanged".
type UserPatch struct {
	FullName      *string
	Status        *string // active | disabled; disabling revokes every session
	ResetPassword bool    // issue a fresh first-login credential
}

// CreateUser provisions a teacher or student account with a generated
// first-login credential (docs/04-api.md: POST /users). The account starts
// with must_change_password=true, so the credential works exactly once
// before the user must choose their own. Returns the user and the one-time
// password; the password is never stored or logged in the clear.
func (s *Service) CreateUser(ctx context.Context, actor User, role, email, fullName string) (User, string, error) {
	password, err := generatePassword()
	if err != nil {
		return User{}, "", err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, "", fmt.Errorf("hash generated password: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, "", fmt.Errorf("begin create-user tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	u, _, err := scanUser(tx.QueryRowContext(ctx,
		`INSERT INTO users (role, email, password_hash, full_name, created_by)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING `+userColumns+`, password_hash`,
		role, email, hash, fullName, actor.ID))
	if isUniqueViolation(err) {
		return User{}, "", ErrEmailTaken
	}
	if err != nil {
		return User{}, "", fmt.Errorf("insert user: %w", err)
	}
	if err := writeAudit(ctx, tx, actor.ID, "users.created", "user", u.ID,
		map[string]any{"role": role, "email": email}); err != nil {
		return User{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return User{}, "", fmt.Errorf("commit create user: %w", err)
	}
	return u, password, nil
}

// UpdateUser applies an admin patch (docs/04-api.md: PATCH /users/:id).
// Disabling an account or resetting its credential revokes every session, so
// the change bites immediately, and each mutation writes its audit row in
// the same transaction. Admins cannot disable or reset themselves through
// this endpoint - that path is how you lock the last admin out.
func (s *Service) UpdateUser(ctx context.Context, actor User, id string, patch UserPatch) (User, string, error) {
	if actor.ID == id && (patch.Status != nil || patch.ResetPassword) {
		return User{}, "", ErrSelfMutation
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, "", fmt.Errorf("begin update-user tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	u, _, err := scanUser(tx.QueryRowContext(ctx,
		`SELECT `+userColumns+`, password_hash FROM users WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, "", ErrNotFound
	}
	if err != nil {
		return User{}, "", fmt.Errorf("load user: %w", err)
	}

	changes := map[string]any{}
	if patch.FullName != nil && *patch.FullName != u.FullName {
		u.FullName = *patch.FullName
		changes["full_name"] = *patch.FullName
	}
	revokeSessions := false
	if patch.Status != nil && *patch.Status != u.Status {
		u.Status = *patch.Status
		changes["status"] = *patch.Status
		// Disabling must kill live sessions, not wait for token expiry.
		revokeSessions = *patch.Status == "disabled"
	}
	password := ""
	newHash := ""
	if patch.ResetPassword {
		password, err = generatePassword()
		if err != nil {
			return User{}, "", err
		}
		newHash, err = HashPassword(password)
		if err != nil {
			return User{}, "", fmt.Errorf("hash reset password: %w", err)
		}
		u.MustChangePassword = true
		changes["password_reset"] = true
		revokeSessions = true
	}
	if len(changes) == 0 {
		return u, "", nil
	}

	if newHash != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET full_name = $1, status = $2, password_hash = $3,
			        must_change_password = true WHERE id = $4`,
			u.FullName, u.Status, newHash, id); err != nil {
			return User{}, "", fmt.Errorf("update user: %w", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET full_name = $1, status = $2 WHERE id = $3`,
			u.FullName, u.Status, id); err != nil {
			return User{}, "", fmt.Errorf("update user: %w", err)
		}
	}
	if revokeSessions {
		if _, err := tx.ExecContext(ctx,
			`UPDATE user_sessions SET revoked_at = now()
			 WHERE user_id = $1 AND revoked_at IS NULL`, id); err != nil {
			return User{}, "", fmt.Errorf("revoke sessions: %w", err)
		}
	}
	if err := writeAudit(ctx, tx, actor.ID, "users.updated", "user", id, changes); err != nil {
		return User{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return User{}, "", fmt.Errorf("commit update user: %w", err)
	}
	return u, password, nil
}

// ListUsers returns accounts newest-first, optionally filtered by role
// and/or status. The admin console's user table reads from this.
func (s *Service) ListUsers(ctx context.Context, role, status string) ([]User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+userColumns+` FROM users
		 WHERE ($1 = '' OR role = $1::user_role)
		   AND ($2 = '' OR status = $2::user_status)
		 ORDER BY created_at DESC, id`, role, status)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	users := []User{}
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Role, &u.Email, &u.FullName, &u.Status,
			&u.MustChangePassword, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// CreateGroup creates a cohort (docs/04-api.md: POST /groups).
func (s *Service) CreateGroup(ctx context.Context, actor User, name string) (Group, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Group{}, fmt.Errorf("begin create-group tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var g Group
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO groups (name, created_by) VALUES ($1, $2)
		 RETURNING id, name, created_at`, name, actor.ID,
	).Scan(&g.ID, &g.Name, &g.CreatedAt); err != nil {
		return Group{}, fmt.Errorf("insert group: %w", err)
	}
	if err := writeAudit(ctx, tx, actor.ID, "groups.created", "group", g.ID,
		map[string]any{"name": name}); err != nil {
		return Group{}, err
	}
	if err := tx.Commit(); err != nil {
		return Group{}, fmt.Errorf("commit create group: %w", err)
	}
	return g, nil
}

// SetGroupMembers replaces the group's membership with exactly studentIDs
// (docs/04-api.md: PUT /groups/:id/members). The whole set is validated -
// every id must be a student account - and swapped in one transaction, so a
// bad id changes nothing.
func (s *Service) SetGroupMembers(ctx context.Context, actor User, groupID string, studentIDs []string) (Group, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Group{}, fmt.Errorf("begin members tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var g Group
	err = tx.QueryRowContext(ctx,
		`SELECT id, name, created_at FROM groups WHERE id = $1 FOR UPDATE`, groupID,
	).Scan(&g.ID, &g.Name, &g.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Group{}, ErrNotFound
	}
	if err != nil {
		return Group{}, fmt.Errorf("load group: %w", err)
	}

	var students int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM users
		 WHERE role = 'student' AND id = ANY($1::uuid[])`,
		uuidArray(studentIDs)).Scan(&students); err != nil {
		return Group{}, fmt.Errorf("validate members: %w", err)
	}
	if students != len(studentIDs) {
		return Group{}, fmt.Errorf("%w: %d of %d ids are not student accounts",
			errNotStudents, len(studentIDs)-students, len(studentIDs))
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM group_members WHERE group_id = $1`, groupID); err != nil {
		return Group{}, fmt.Errorf("clear members: %w", err)
	}
	if len(studentIDs) > 0 {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO group_members (group_id, student_id)
			 SELECT $1, unnest($2::uuid[])`, groupID, uuidArray(studentIDs)); err != nil {
			return Group{}, fmt.Errorf("insert members: %w", err)
		}
	}
	if err := writeAudit(ctx, tx, actor.ID, "groups.members_set", "group", groupID,
		map[string]any{"member_count": len(studentIDs)}); err != nil {
		return Group{}, err
	}
	if err := tx.Commit(); err != nil {
		return Group{}, fmt.Errorf("commit members: %w", err)
	}
	g.MemberCount = len(studentIDs)
	return g, nil
}

// errNotStudents marks membership lists containing non-student (or unknown)
// ids; the HTTP layer maps it to VALIDATION_FAILED.
var errNotStudents = errors.New("group members must be student accounts")

// ListGroups returns all cohorts with their member counts, newest-first.
func (s *Service) ListGroups(ctx context.Context) ([]Group, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT g.id, g.name, g.created_at, count(m.student_id)
		 FROM groups g LEFT JOIN group_members m ON m.group_id = g.id
		 GROUP BY g.id ORDER BY g.created_at DESC, g.id`)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	defer rows.Close()

	groups := []Group{}
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Name, &g.CreatedAt, &g.MemberCount); err != nil {
			return nil, fmt.Errorf("scan group: %w", err)
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

// generatePassword returns a 16-character URL-safe first-login credential
// with 96 bits of randomness - long enough to shrug off online guessing for
// the minutes it lives before the forced reset replaces it.
func generatePassword() (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// uuidArray renders ids as a Postgres array literal for ANY($1::uuid[]).
// database/sql has no native slice binding; the text form is the portable
// way in and Postgres validates each element as a uuid.
func uuidArray(ids []string) string {
	return "{" + strings.Join(ids, ",") + "}"
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
