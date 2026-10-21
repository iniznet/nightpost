package store

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"strconv"
	"strings"
)

// Single-admin dashboard accounts : one tier, PBKDF2 hashes, no roles.
// The users table is configuration — retention never prunes it.

const pbkdf2Iter = 150_000
const pbkdf2KeyLen = 32

// HashPassword renders the self-describing format
// "pbkdf2-sha256$<iter>$<saltB64>$<dkB64>" so the cost parameter can be
// raised later without a migration (old hashes verify, new ones strengthen).
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iter, pbkdf2KeyLen)
	if err != nil {
		return "", err
	}
	return "pbkdf2-sha256$" + strconv.Itoa(pbkdf2Iter) + "$" +
		base64.RawStdEncoding.EncodeToString(salt) + "$" +
		base64.RawStdEncoding.EncodeToString(dk), nil
}

// VerifyPassword recomputes the derived key and compares in constant time.
func VerifyPassword(hashed, password string) bool {
	parts := strings.Split(hashed, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// timingDummyHash is structurally valid and never matches a real attempt;
// unknown emails verify against it so login failures leak no account info.
const timingDummyHash = "pbkdf2-sha256$150000$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// SessionEpoch returns the earliest issue-time a session cookie may carry
// for email; logout and password resets push the epoch forward, which
// revokes every older stateless cookie. Unknown emails return MaxInt64 so
// their sessions die with the account.
func (s *Store) SessionEpoch(email string) (int64, error) {
	var epoch int64
	err := s.rdb.QueryRow(`SELECT sessions_valid_after FROM users WHERE email = ?`, email).Scan(&epoch)
	if err == sql.ErrNoRows {
		return int64(^uint64(0) >> 1), nil
	}
	return epoch, err
}

// RevokeSessions invalidates every session issued before now.
func (s *Store) RevokeSessions(email string) error {
	_, err := s.wdb.Exec(`UPDATE users SET sessions_valid_after = CAST(strftime('%s','now') AS INTEGER) WHERE email = ?`, email)
	return err
}

// UserCount reports whether first-run setup is still open.
func (s *Store) UserCount() (int, error) {
	var n int
	err := s.rdb.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CreateUserIfNone inserts only while the table is empty; the guarded
// INSERT...SELECT makes the first writer win even if two setup posts race
// (SQLite's single writer serializes them). Returns false when locked.
func (s *Store) CreateUserIfNone(email, pwHash string) (bool, error) {
	res, err := s.wdb.Exec(`INSERT INTO users (email, pw_hash, created_at)
		SELECT ?, ?, CAST(strftime('%s','now') AS INTEGER)
		WHERE (SELECT COUNT(*) FROM users) = 0`, email, pwHash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n > 0 {
		// a new dashboard account owns the default org from the start.
		if _, merr := s.wdb.Exec(`INSERT OR IGNORE INTO org_members (org_id, user_id, role, created_at)
			SELECT ?, id, ?, CAST(strftime('%s','now') AS INTEGER) FROM users WHERE email = ?`,
			DefaultOrgID, RoleOwner, email); merr != nil {
			return true, merr
		}
	}
	return n > 0, nil
}

// VerifyUser checks credentials; unknown emails burn a comparable PBKDF2
// round before failing.
func (s *Store) VerifyUser(email, password string) (bool, error) {
	var hashed string
	err := s.rdb.QueryRow(`SELECT pw_hash FROM users WHERE email = ?`, email).Scan(&hashed)
	if err == sql.ErrNoRows {
		VerifyPassword(timingDummyHash, password)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return VerifyPassword(hashed, password), nil
}

// ResetAdminPassword upserts a fresh random password for email (creating
// the account when absent) and returns it once — backs -reset-admin .
func (s *Store) ResetAdminPassword(email string) (string, error) {
	pw, err := randomPassword(20)
	if err != nil {
		return "", err
	}
	hashed, err := HashPassword(pw)
	if err != nil {
		return "", err
	}
	_, err = s.wdb.Exec(`INSERT INTO users (email, pw_hash, created_at, sessions_valid_after)
		VALUES (?, ?, CAST(strftime('%s','now') AS INTEGER), CAST(strftime('%s','now') AS INTEGER))
		ON CONFLICT(email) DO UPDATE SET pw_hash = excluded.pw_hash, sessions_valid_after = excluded.sessions_valid_after`, email, hashed)
	if err != nil {
		return "", err
	}
	// -reset-admin may create the account, so it must own the default
	// org too — otherwise the recovered login would see no projects at all.
	if _, merr := s.wdb.Exec(`INSERT OR IGNORE INTO org_members (org_id, user_id, role, created_at)
		SELECT ?, id, ?, CAST(strftime('%s','now') AS INTEGER) FROM users WHERE email = ?`,
		DefaultOrgID, RoleOwner, email); merr != nil {
		return "", merr
	}
	return pw, nil
}

// randomPassword draws from an ambiguous-character-free alphabet.
func randomPassword(n int) (string, error) {
	const alphabet = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789"
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i := range buf {
		buf[i] = alphabet[int(buf[i])%len(alphabet)]
	}
	return string(buf), nil
}
