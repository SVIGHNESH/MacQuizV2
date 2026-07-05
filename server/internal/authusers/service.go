package authusers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"macquiz/server/internal/audit"
)

// Sentinel errors the HTTP layer maps onto the docs/04-api.md vocabulary.
var (
	// ErrInvalidCredentials covers unknown email, wrong password, and
	// disabled accounts alike, so responses never reveal which it was.
	ErrInvalidCredentials = errors.New("invalid credentials")
	// ErrSessionInvalid covers unknown, expired, reused, and revoked
	// refresh tokens; the client's only recovery is a fresh login.
	ErrSessionInvalid = errors.New("session invalid")
)

// User is the account shape returned to clients. password_hash never leaves
// the package.
type User struct {
	ID                 string    `json:"id"`
	Role               string    `json:"role"`
	Email              string    `json:"email"`
	FullName           string    `json:"full_name"`
	Status             string    `json:"status"`
	MustChangePassword bool      `json:"must_change_password"`
	CreatedAt          time.Time `json:"created_at"`
}

// Service owns accounts and sessions. All queries go through the shared
// *sql.DB pool; multi-statement writes use explicit transactions.
type Service struct {
	db     *sql.DB
	secret []byte
	log    *slog.Logger
}

// NewService wires the auth service. secret signs access tokens (HS256).
func NewService(db *sql.DB, secret string, log *slog.Logger) *Service {
	return &Service{db: db, secret: []byte(secret), log: log}
}

const userColumns = `id, role, email, full_name, status, must_change_password, created_at`

func scanUser(row *sql.Row) (User, string, error) {
	var u User
	var hash string
	err := row.Scan(&u.ID, &u.Role, &u.Email, &u.FullName, &u.Status,
		&u.MustChangePassword, &u.CreatedAt, &hash)
	return u, hash, err
}

// UserByID loads one account; sql.ErrNoRows when absent.
func (s *Service) UserByID(ctx context.Context, id string) (User, error) {
	u, _, err := scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+`, password_hash FROM users WHERE id = $1`, id))
	return u, err
}

// Login verifies the credential and opens a new session family. It returns
// the user, the signed access token, and the opaque refresh token.
func (s *Service) Login(ctx context.Context, email, password string) (User, string, string, error) {
	u, hash, err := scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+`, password_hash FROM users WHERE email = $1`, email))
	if errors.Is(err, sql.ErrNoRows) {
		// Burn comparable time so a missing account is not distinguishable
		// from a wrong password by response latency.
		_, _ = VerifyPassword(password, timingDecoyHash)
		return User{}, "", "", ErrInvalidCredentials
	}
	if err != nil {
		return User{}, "", "", fmt.Errorf("load user by email: %w", err)
	}
	ok, err := VerifyPassword(password, hash)
	if err != nil {
		return User{}, "", "", fmt.Errorf("verify password: %w", err)
	}
	if !ok || u.Status != "active" {
		return User{}, "", "", ErrInvalidCredentials
	}

	now := time.Now().UTC()
	refresh, refreshHash, err := newRefreshToken()
	if err != nil {
		return User{}, "", "", err
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO user_sessions (user_id, family_id, token_hash, expires_at)
		 VALUES ($1, gen_random_uuid(), $2, $3)`,
		u.ID, refreshHash, now.Add(RefreshTokenTTL)); err != nil {
		return User{}, "", "", fmt.Errorf("create session: %w", err)
	}
	access, err := signAccessToken(s.secret, u.ID, u.Role, now)
	if err != nil {
		return User{}, "", "", fmt.Errorf("sign access token: %w", err)
	}
	return u, access, refresh, nil
}

// Refresh rotates the presented refresh token: the old token is stamped used,
// a successor is inserted in the same family, and a fresh access token is
// signed. Presenting a token that was already used or revoked means the token
// leaked, so the whole family is revoked (docs/08-security.md section 1).
func (s *Service) Refresh(ctx context.Context, refreshToken string) (User, string, string, error) {
	tokenHash := hashRefreshToken(refreshToken)
	now := time.Now().UTC()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, "", "", fmt.Errorf("begin refresh tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var sessionID, familyID, userID string
	var expiresAt time.Time
	var usedAt, revokedAt sql.NullTime
	err = tx.QueryRowContext(ctx,
		`SELECT id, family_id, user_id, expires_at, used_at, revoked_at
		 FROM user_sessions WHERE token_hash = $1 FOR UPDATE`, tokenHash,
	).Scan(&sessionID, &familyID, &userID, &expiresAt, &usedAt, &revokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, "", "", ErrSessionInvalid
	}
	if err != nil {
		return User{}, "", "", fmt.Errorf("load session: %w", err)
	}

	if usedAt.Valid || revokedAt.Valid {
		if _, err := tx.ExecContext(ctx,
			`UPDATE user_sessions SET revoked_at = $1
			 WHERE family_id = $2 AND revoked_at IS NULL`, now, familyID); err != nil {
			return User{}, "", "", fmt.Errorf("revoke family on reuse: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return User{}, "", "", fmt.Errorf("commit reuse revocation: %w", err)
		}
		s.log.Warn("refresh token reuse detected; session family revoked",
			"user_id", userID, "family_id", familyID)
		return User{}, "", "", ErrSessionInvalid
	}
	if now.After(expiresAt) {
		return User{}, "", "", ErrSessionInvalid
	}

	u, _, err := scanUser(tx.QueryRowContext(ctx,
		`SELECT `+userColumns+`, password_hash FROM users WHERE id = $1`, userID))
	if err != nil {
		return User{}, "", "", fmt.Errorf("load session user: %w", err)
	}
	if u.Status != "active" {
		return User{}, "", "", ErrSessionInvalid
	}

	refresh, refreshHash, err := newRefreshToken()
	if err != nil {
		return User{}, "", "", err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE user_sessions SET used_at = $1 WHERE id = $2`, now, sessionID); err != nil {
		return User{}, "", "", fmt.Errorf("mark session used: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO user_sessions (user_id, family_id, token_hash, expires_at)
		 VALUES ($1, $2, $3, $4)`,
		userID, familyID, refreshHash, now.Add(RefreshTokenTTL)); err != nil {
		return User{}, "", "", fmt.Errorf("insert rotated session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return User{}, "", "", fmt.Errorf("commit rotation: %w", err)
	}

	access, err := signAccessToken(s.secret, u.ID, u.Role, now)
	if err != nil {
		return User{}, "", "", fmt.Errorf("sign access token: %w", err)
	}
	return u, access, refresh, nil
}

// Logout revokes the whole family of the presented refresh token. Unknown
// tokens are a no-op: logout must always succeed from the client's view.
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE user_sessions SET revoked_at = now()
		 WHERE revoked_at IS NULL AND family_id = (
		   SELECT family_id FROM user_sessions WHERE token_hash = $1)`,
		hashRefreshToken(refreshToken))
	if err != nil {
		return fmt.Errorf("revoke session family: %w", err)
	}
	return nil
}

// ChangePassword verifies the current credential, stores the new Argon2id
// hash, clears must_change_password, revokes every existing session (all
// devices re-authenticate), and writes the audit row - one transaction.
func (s *Service) ChangePassword(ctx context.Context, userID, current, next string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin password tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var hash string
	if err := tx.QueryRowContext(ctx,
		`SELECT password_hash FROM users WHERE id = $1 FOR UPDATE`, userID,
	).Scan(&hash); err != nil {
		return fmt.Errorf("load password hash: %w", err)
	}
	ok, err := VerifyPassword(current, hash)
	if err != nil {
		return fmt.Errorf("verify current password: %w", err)
	}
	if !ok {
		return ErrInvalidCredentials
	}
	newHash, err := HashPassword(next)
	if err != nil {
		return fmt.Errorf("hash new password: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET password_hash = $1, must_change_password = false
		 WHERE id = $2`, newHash, userID); err != nil {
		return fmt.Errorf("store new password: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE user_sessions SET revoked_at = now()
		 WHERE user_id = $1 AND revoked_at IS NULL`, userID); err != nil {
		return fmt.Errorf("revoke sessions: %w", err)
	}
	if err := writeAudit(ctx, tx, userID, "auth.password_changed", "user", userID, nil); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit password change: %w", err)
	}
	return nil
}

// EnsureBootstrapAdmin idempotently creates the first admin account. The
// bootstrap admin is self-referencing (created_by = id) because every later
// account requires an existing creator, and skips the forced reset because
// the operator chose this password themselves.
func (s *Service) EnsureBootstrapAdmin(ctx context.Context, email, password, fullName string) error {
	var admins int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM users WHERE role = 'admin'`).Scan(&admins); err != nil {
		return fmt.Errorf("count admins: %w", err)
	}
	if admins > 0 {
		s.log.Info("bootstrap: admin already present, nothing to do")
		return nil
	}
	hash, err := HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash bootstrap password: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin bootstrap tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var id string
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO users (id, role, email, password_hash, full_name, created_by, must_change_password)
		 SELECT id, 'admin', $1, $2, $3, id, false
		 FROM (SELECT gen_random_uuid() AS id) seed
		 RETURNING id`, email, hash, fullName,
	).Scan(&id); err != nil {
		return fmt.Errorf("insert bootstrap admin: %w", err)
	}
	if err := writeAudit(ctx, tx, id, "users.bootstrap_admin", "user", id,
		map[string]any{"email": email}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit bootstrap: %w", err)
	}
	s.log.Info("bootstrap: admin created", "email", email, "id", id)
	return nil
}

// writeAudit appends one audit_log row inside the caller's transaction, so a
// mutation and its audit trail commit or roll back together
// (docs/08-security.md section 7).
func writeAudit(ctx context.Context, tx *sql.Tx, actorID, action, resourceType, resourceID string, detail map[string]any) error {
	return audit.Write(ctx, tx, actorID, action, resourceType, resourceID, detail)
}

// newRefreshToken returns (token, sha256(token)). The token is 256 bits of
// randomness; only the hash touches the database.
func newRefreshToken() (string, []byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("generate refresh token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	return token, hashRefreshToken(token), nil
}

func hashRefreshToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// timingDecoyHash is a valid Argon2id hash of an unguessable throwaway value,
// verified against when the email does not exist so both login failure paths
// cost one Argon2id computation.
const timingDecoyHash = "$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$m0iEHb1lFPtHOu6DIRxJIWZuqmj9BKB2b7d94dVLQ0M"
