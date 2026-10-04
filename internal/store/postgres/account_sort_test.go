package postgres

import (
	"strings"
	"testing"
)

func TestAccountOrderNewestUsesCreateTimeNotUpdatedAt(t *testing.T) {
	sql := accountOrderSQL("newest")
	if !strings.Contains(sql, "create_time") {
		t.Fatalf("newest should use payload create_time: %s", sql)
	}
	// Primary sort key must be create_time expression, not bare a.updated_at DESC.
	if strings.HasPrefix(strings.TrimSpace(sql), "a.updated_at") {
		t.Fatalf("newest must not primary-sort by updated_at: %s", sql)
	}
	if !strings.Contains(sql, "DESC") {
		t.Fatalf("newest should be DESC: %s", sql)
	}
}

func TestAccountOrderOldestUsesCreateTime(t *testing.T) {
	sql := accountOrderSQL("oldest")
	if !strings.Contains(sql, "create_time") {
		t.Fatalf("oldest should use create_time: %s", sql)
	}
	if strings.HasPrefix(strings.TrimSpace(sql), "a.updated_at") {
		t.Fatalf("oldest must not primary-sort by updated_at: %s", sql)
	}
	if !strings.Contains(sql, "ASC") {
		t.Fatalf("oldest should be ASC: %s", sql)
	}
}

func TestNormalizeAccountSortOldest(t *testing.T) {
	if got := normalizeAccountSort("oldest"); got != "oldest" {
		t.Fatalf("oldest -> %q", got)
	}
	if got := normalizeAccountSort("newest"); got != "newest" {
		t.Fatalf("newest -> %q", got)
	}
	if got := normalizeAccountSort("email_asc"); got != "email_asc" {
		t.Fatalf("email_asc -> %q", got)
	}
	if got := normalizeAccountSort("old"); got != "oldest" {
		t.Fatalf("old -> %q", got)
	}
}
