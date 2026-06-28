package utils

import (
	"database/sql/driver"
	"fmt"
	"time"
)

// NullTime is a nullable time.Time that survives scans from pure-Go
// SQLite drivers (modernc.org/sqlite used by glebarez/sqlite).
//
// Background: SQLite has no native DATETIME type, so GORM stores
// *time.Time fields as TEXT columns. Unlike mattn/go-sqlite3 (which
// can parse them back via parseTime=true), modernc returns these TEXT
// values as plain strings, and database/sql cannot convert string ->
// *time.Time on its own, producing:
//
//	Scan error: revoked_at string -> *time.Time
//
// NullTime implements sql.Scanner to accept nil, time.Time, string
// and []byte, and driver.Valuer so that an invalid value is stored
// as SQL NULL.
type NullTime struct {
	Time  time.Time
	Valid bool
}

// Scan implements sql.Scanner.
//
// Accepted value types:
//   - nil           -> invalid (NULL)
//   - time.Time     -> valid
//   - string        -> parsed against common SQLite datetime layouts
//   - []byte        -> same as string
//   - "" (empty)    -> treated as NULL (defensive)
func (n *NullTime) Scan(value interface{}) error {
	if value == nil {
		n.Time, n.Valid = time.Time{}, false
		return nil
	}

	switch v := value.(type) {
	case time.Time:
		n.Time, n.Valid = v, true
		return nil
	case string:
		if v == "" {
			n.Time, n.Valid = time.Time{}, false
			return nil
		}
		t, err := parseSQLiteTime(v)
		if err != nil {
			return err
		}
		n.Time, n.Valid = t, true
		return nil
	case []byte:
		s := string(v)
		if s == "" {
			n.Time, n.Valid = time.Time{}, false
			return nil
		}
		t, err := parseSQLiteTime(s)
		if err != nil {
			return err
		}
		n.Time, n.Valid = t, true
		return nil
	}

	return fmt.Errorf("NullTime: unsupported scan source type %T", value)
}

// Value implements driver.Valuer.
// Returns nil when invalid so the column stores NULL instead of a
// zero time string.
func (n NullTime) Value() (driver.Value, error) {
	if !n.Valid {
		return nil, nil
	}
	return n.Time, nil
}

// MarshalJSON renders null when invalid, RFC3339 otherwise.
// Keeps JSON responses clean without exposing Valid/Time fields.
func (n NullTime) MarshalJSON() ([]byte, error) {
	if !n.Valid {
		return []byte("null"), nil
	}
	return n.Time.MarshalJSON()
}

// parseSQLiteTime tries the datetime layouts SQLite/GORM commonly emit.
// Order matters: most precise / standard first.
func parseSQLiteTime(s string) (time.Time, error) {
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999-07:00",
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("NullTime: cannot parse %q", s)
}
