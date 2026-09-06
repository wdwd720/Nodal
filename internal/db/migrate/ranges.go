package migrate

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Range is a numbering band from docs/architecture/CONVENTIONS.md.
type Range struct {
	Name string
	Lo   int64
	Hi   int64
	// Protected ranges get a non-destructive Down section in their skeleton and
	// sit at or above ProtectedVersion.
	Protected bool
}

// Ranges lists every numbering band, in order.
var Ranges = []Range{
	{Name: "foundation", Lo: 1, Hi: 99},
	{Name: "financial", Lo: 100, Hi: 199, Protected: true},
	{Name: "instruments", Lo: 200, Hi: 299},
	{Name: "execution", Lo: 300, Hi: 399},
	{Name: "funding", Lo: 400, Hi: 499},
	{Name: "strategy", Lo: 500, Hi: 599},
	{Name: "reality", Lo: 600, Hi: 699},
	{Name: "audit", Lo: 700, Hi: 799},
}

// ErrRangeFull is returned by NextVersion when every number is used.
var ErrRangeFull = errors.New("migrate: numbering range is full")

// RangeByName looks a band up by its name (case-insensitive).
func RangeByName(name string) (Range, bool) {
	for _, r := range Ranges {
		if strings.EqualFold(r.Name, name) {
			return r, true
		}
	}
	return Range{}, false
}

// RangeNames returns the band names for help text.
func RangeNames() []string {
	out := make([]string, 0, len(Ranges))
	for _, r := range Ranges {
		out = append(out, r.Name)
	}
	return out
}

// MaxVersion is the largest version the five-digit file name format can hold.
const MaxVersion int64 = 99999

// NextVersion returns the next migration version: one more than the highest
// existing migration file, regardless of r. goose applies migrations in
// version order and refuses out-of-order versions below the database's
// current version, so new files must always take the highest number
// (DECISION_REGISTER D-015 addendum 2). r is kept for the skeleton hints
// only; when no file exists the first version is r.Lo (1 for foundation).
func NextVersion(names []string, r Range) (int64, error) {
	next := r.Lo
	if next < 1 {
		next = 1
	}
	for _, n := range names {
		v, ok := parseVersion(n)
		if !ok {
			continue
		}
		if v+1 > next {
			next = v + 1
		}
	}
	if next > MaxVersion {
		return 0, fmt.Errorf("%w: version %d exceeds %d", ErrRangeFull, next, MaxVersion)
	}
	return next, nil
}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// ValidateName enforces lower_snake_case migration names.
func ValidateName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("migrate: invalid migration name %q: use lower_snake_case starting with a letter (max 64 chars)", name)
	}
	return nil
}

// Filename renders NNNNN_name.sql.
func Filename(version int64, name string) string {
	return fmt.Sprintf("%05d_%s.sql", version, name)
}

// Skeleton renders a new migration file body with goose annotations. Protected
// ranges receive the mandated non-destructive Down section.
func Skeleton(name string, r Range) string {
	var b strings.Builder
	b.WriteString("-- +goose Up\n")
	fmt.Fprintf(&b, "-- %s (%s range %d-%d)\n", name, r.Name, r.Lo, r.Hi)
	b.WriteString("-- Every table: id uuid PRIMARY KEY, created_at timestamptz NOT NULL DEFAULT now(),\n")
	b.WriteString("-- updated_at where mutable. Wrap plpgsql bodies in -- +goose StatementBegin/End.\n")
	if r.Protected {
		b.WriteString("-- Posted ledger rows are immutable: attach forbid_mutation() BEFORE UPDATE OR DELETE\n")
		b.WriteString("-- and grant cp_app SELECT, INSERT only.\n")
	}
	b.WriteString("\n\n-- +goose Down\n")
	if r.Protected {
		b.WriteString("SELECT 1; -- ledger history is never dropped\n")
	} else {
		b.WriteString("-- Reverse the Up section here.\n")
	}
	return b.String()
}
