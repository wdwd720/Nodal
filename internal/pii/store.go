package pii

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db"
)

// Column names, which are part of what each ciphertext is bound to.
const (
	columnEmail     = "email"
	columnLegalName = "legal_name"
	columnDOB       = "dob"
)

// Record is one user's personal data, decrypted.
type Record struct {
	Email       string
	LegalName   string
	DOB         string
	CountryCode string
	RegionCode  string
	// KeyVersion is the version the row's ciphertexts are sealed under.
	KeyVersion int
}

// Patch is a partial update. A nil field is left as it is; a pointer to the
// empty string clears it.
type Patch struct {
	Email       *string
	LegalName   *string
	DOB         *string
	CountryCode *string
	RegionCode  *string
}

// Store reads and writes identity_pii through a Keyring. It is the only
// writer of that table in the repository, and test/security asserts that.
type Store struct {
	keys *Keyring
}

// NewStore builds a Store over a keyring.
func NewStore(keys *Keyring) *Store {
	if keys == nil {
		return nil
	}
	return &Store{keys: keys}
}

// Keyring exposes the ring, for the startup log to name its versions.
func (s *Store) Keyring() *Keyring { return s.keys }

type row struct {
	email, legalName, dob []byte
	country, region       *string
	version               int
}

// Upsert merges patch into the user's row and writes every column back
// sealed under the active key version.
//
// The whole row is re-sealed on every write, not only the patched column:
// `key_version` is a row-level fact, so a row must not carry ciphertexts
// under two versions. It is one row and three short values; the cost is
// nothing and the alternative is a schema that lies about itself.
//
// Takes the row FOR UPDATE, so two concurrent writers to the same user
// serialise instead of one silently discarding the other's column.
func (s *Store) Upsert(ctx context.Context, q db.Querier, userID string, patch Patch) error {
	if s == nil {
		return errors.New("pii: no store is configured, so personal data cannot be written")
	}
	if userID == "" {
		return errors.New("pii: user id is required")
	}
	cur, found, err := s.load(ctx, q, userID, true)
	if err != nil {
		return err
	}
	rec := Record{}
	if found {
		if rec, err = s.decrypt(userID, cur); err != nil {
			return err
		}
	}
	apply := func(dst, src *string) {
		if src != nil {
			*dst = *src
		}
	}
	apply(&rec.Email, patch.Email)
	apply(&rec.LegalName, patch.LegalName)
	apply(&rec.DOB, patch.DOB)
	apply(&rec.CountryCode, patch.CountryCode)
	apply(&rec.RegionCode, patch.RegionCode)
	return s.write(ctx, q, userID, rec, found)
}

// Reseal re-encrypts the user's row under the active key version without
// changing its contents. It is how a rotation is completed; a row already at
// the active version is rewritten anyway, which is harmless and keeps the
// operation idempotent.
func (s *Store) Reseal(ctx context.Context, q db.Querier, userID string) error {
	if s == nil {
		return errors.New("pii: no store is configured")
	}
	cur, found, err := s.load(ctx, q, userID, true)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	rec, err := s.decrypt(userID, cur)
	if err != nil {
		return err
	}
	return s.write(ctx, q, userID, rec, true)
}

// EnsureEmail records the address if the row has none, and leaves a row that
// already has one alone. It mirrors users.email_hash's rule -- written once,
// filling an absence rather than following a changing address -- so a login
// costs one read and no write once the value is in place.
func (s *Store) EnsureEmail(ctx context.Context, q db.Querier, userID, email string) error {
	if s == nil {
		return errors.New("pii: no store is configured, so personal data cannot be written")
	}
	if email == "" {
		return nil
	}
	rec, found, err := s.Read(ctx, q, userID)
	if err != nil {
		return err
	}
	if found && rec.Email != "" {
		return nil
	}
	return s.Upsert(ctx, q, userID, Patch{Email: &email})
}

// Read returns the user's personal data, or found=false when there is no
// row.
func (s *Store) Read(ctx context.Context, q db.Querier, userID string) (Record, bool, error) {
	if s == nil {
		return Record{}, false, errors.New("pii: no store is configured, so personal data cannot be read")
	}
	cur, found, err := s.load(ctx, q, userID, false)
	if err != nil || !found {
		return Record{}, found, err
	}
	rec, err := s.decrypt(userID, cur)
	return rec, true, err
}

func (s *Store) load(ctx context.Context, q db.Querier, userID string, forUpdate bool) (row, bool, error) {
	sql := `SELECT email_encrypted, legal_name_encrypted, dob_encrypted, country_code, region_code, key_version
	          FROM identity_pii WHERE user_id = $1::uuid`
	if forUpdate {
		sql += " FOR UPDATE"
	}
	var r row
	err := q.QueryRow(ctx, sql, userID).Scan(&r.email, &r.legalName, &r.dob, &r.country, &r.region, &r.version)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return row{}, false, nil
	case err != nil:
		return row{}, false, fmt.Errorf("pii: load: %w", err)
	}
	return r, true, nil
}

func (s *Store) decrypt(userID string, r row) (Record, error) {
	rec := Record{KeyVersion: r.version}
	open := func(column string, ct []byte) (string, error) {
		if ct == nil {
			return "", nil
		}
		pt, err := s.keys.Open(userID, column, r.version, ct)
		if err != nil {
			return "", err
		}
		return string(pt), nil
	}
	var err error
	if rec.Email, err = open(columnEmail, r.email); err != nil {
		return Record{}, err
	}
	if rec.LegalName, err = open(columnLegalName, r.legalName); err != nil {
		return Record{}, err
	}
	if rec.DOB, err = open(columnDOB, r.dob); err != nil {
		return Record{}, err
	}
	if r.country != nil {
		rec.CountryCode = *r.country
	}
	if r.region != nil {
		rec.RegionCode = *r.region
	}
	return rec, nil
}

func (s *Store) write(ctx context.Context, q db.Querier, userID string, rec Record, exists bool) error {
	seal := func(column, value string) ([]byte, error) {
		if value == "" {
			return nil, nil // an absent value is NULL, not the encryption of ""
		}
		ct, _, err := s.keys.Seal(userID, column, []byte(value))
		return ct, err
	}
	email, err := seal(columnEmail, rec.Email)
	if err != nil {
		return err
	}
	legal, err := seal(columnLegalName, rec.LegalName)
	if err != nil {
		return err
	}
	dob, err := seal(columnDOB, rec.DOB)
	if err != nil {
		return err
	}
	var country, region *string
	if rec.CountryCode != "" {
		country = &rec.CountryCode
	}
	if rec.RegionCode != "" {
		region = &rec.RegionCode
	}
	version := s.keys.Active()
	if exists {
		_, err = q.Exec(ctx, `UPDATE identity_pii
		   SET email_encrypted = $2, legal_name_encrypted = $3, dob_encrypted = $4,
		       country_code = $5, region_code = $6, key_version = $7
		 WHERE user_id = $1::uuid`, userID, email, legal, dob, country, region, version)
	} else {
		_, err = q.Exec(ctx, `INSERT INTO identity_pii
		   (user_id, email_encrypted, legal_name_encrypted, dob_encrypted, country_code, region_code, key_version)
		   VALUES ($1::uuid, $2, $3, $4, $5, $6, $7)`, userID, email, legal, dob, country, region, version)
	}
	if err != nil {
		return fmt.Errorf("pii: write: %w", err)
	}
	return nil
}
