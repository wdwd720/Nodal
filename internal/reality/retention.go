package reality

import (
	"time"

	"github.com/nodal/controlplane/internal/archive"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/errs"
)

// RetentionClass is one of the six configurable classes (PART 122).
type RetentionClass string

// Retention classes.
const (
	RetentionFinancialRecord RetentionClass = "FINANCIAL_RECORD"
	RetentionSecurityAudit   RetentionClass = "SECURITY_AUDIT"
	RetentionRawMarketData   RetentionClass = "RAW_MARKET_DATA"
	RetentionSocialData      RetentionClass = "SOCIAL_DATA"
	RetentionModelIO         RetentionClass = "MODEL_IO"
	RetentionOperationalLog  RetentionClass = "OPERATIONAL_LOG"
)

// RetentionClasses lists every class in declaration order.
func RetentionClasses() []RetentionClass {
	return []RetentionClass{
		RetentionFinancialRecord, RetentionSecurityAudit, RetentionRawMarketData,
		RetentionSocialData, RetentionModelIO, RetentionOperationalLog,
	}
}

// ParseRetentionClass fails closed on unknown values.
func ParseRetentionClass(s string) (RetentionClass, error) {
	for _, c := range RetentionClasses() {
		if string(c) == s {
			return c, nil
		}
	}
	return "", errs.New(errs.CodeValidationFailed, "reality: unknown retention class").WithField("class", s)
}

// Valid reports whether c is declared.
func (c RetentionClass) Valid() bool {
	_, err := ParseRetentionClass(string(c))
	return err == nil
}

// LockRequired reports whether objects of the class are written with an
// Object Lock retention (the audit classes).
func (c RetentionClass) LockRequired() bool {
	return c == RetentionFinancialRecord || c == RetentionSecurityAudit
}

// RetentionPolicy maps classes to configured days. It is built from
// config.RetentionConfig, which is the only source of these numbers: there
// is no hardcoded forever value, and config.Validate refuses zero for the
// audit classes in STAGING/PROD.
type RetentionPolicy struct {
	days map[RetentionClass]int
}

// NewRetentionPolicy builds the policy. Negative days are rejected; zero
// means "not configured" and makes Until fail for that class.
func NewRetentionPolicy(cfg config.RetentionConfig) (RetentionPolicy, error) {
	days := map[RetentionClass]int{
		RetentionFinancialRecord: cfg.FinancialRecordDays,
		RetentionSecurityAudit:   cfg.SecurityAuditDays,
		RetentionRawMarketData:   cfg.RawMarketDataDays,
		RetentionSocialData:      cfg.SocialDataDays,
		RetentionModelIO:         cfg.ModelIODays,
		RetentionOperationalLog:  cfg.OperationalLogDays,
	}
	for c, d := range days {
		if d < 0 {
			return RetentionPolicy{}, errs.New(errs.CodeValidationFailed, "reality: negative retention").WithField("class", string(c))
		}
	}
	return RetentionPolicy{days: days}, nil
}

// Days returns the configured days of a class (0 when unconfigured).
func (p RetentionPolicy) Days(c RetentionClass) (int, error) {
	if !c.Valid() {
		return 0, errs.New(errs.CodeValidationFailed, "reality: unknown retention class").WithField("class", string(c))
	}
	return p.days[c], nil
}

// Until returns from + days(class). An unconfigured class is an error: the
// engine never invents a retention.
func (p RetentionPolicy) Until(c RetentionClass, from time.Time) (time.Time, error) {
	d, err := p.Days(c)
	if err != nil {
		return time.Time{}, err
	}
	if d <= 0 {
		return time.Time{}, errs.New(errs.CodeValidationFailed, "reality: retention not configured for class").WithField("class", string(c))
	}
	return from.UTC().AddDate(0, 0, d), nil
}

// RetentionUntil computes from + days for a per-source configured value.
func RetentionUntil(days int, from time.Time) (time.Time, error) {
	if days <= 0 {
		return time.Time{}, errs.New(errs.CodeValidationFailed, "reality: retention days must be > 0")
	}
	return from.UTC().AddDate(0, 0, days), nil
}

// BucketFor maps a retention class to the archive bucket that holds it and
// whether the write carries an Object Lock retention (POINT_IN_TIME.md §2:
// buckets use Object Lock per retention class).
func BucketFor(c RetentionClass, a config.ArchiveConfig) (bucket string, locked bool, err error) {
	switch c {
	case RetentionFinancialRecord, RetentionSecurityAudit:
		bucket, locked = a.AuditBucket, true
	case RetentionRawMarketData, RetentionSocialData:
		bucket = a.RawBucket
	case RetentionModelIO, RetentionOperationalLog:
		bucket = a.EvidenceBucket
	default:
		return "", false, errs.New(errs.CodeValidationFailed, "reality: unknown retention class").WithField("class", string(c))
	}
	if bucket == "" {
		return "", false, errs.New(errs.CodeValidationFailed, "reality: no archive bucket configured for retention class").WithField("class", string(c))
	}
	return bucket, locked, nil
}

// RetentionFor builds the archive retention of a locked class.
func RetentionFor(c RetentionClass, until time.Time) *archive.Retention {
	if !c.LockRequired() {
		return nil
	}
	return &archive.Retention{Mode: archive.RetentionCompliance, Until: until.UTC()}
}
