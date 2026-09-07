package gates

import (
	"strings"
	"testing"
)

func ruleIDs(findings []Finding) []string {
	out := []string{}
	for _, f := range findings {
		out = append(out, f.RuleID)
	}
	return out
}

func hasRule(findings []Finding, ruleID string) bool {
	for _, id := range ruleIDs(findings) {
		if id == ruleID {
			return true
		}
	}
	return false
}

func countRule(findings []Finding, ruleID string) int {
	n := 0
	for _, id := range ruleIDs(findings) {
		if id == ruleID {
			n++
		}
	}
	return n
}

func one(sql string) MigrationFile {
	return MigrationFile{Path: "migrations/001_up.sql", SQL: sql}
}

func TestDA001DestructiveOperations(t *testing.T) {
	f := AnalyzeMigrations([]MigrationFile{one("DROP TABLE users;")}, AnalyzeOptions{})
	if !hasRule(f, "DA001") {
		t.Fatalf("expected DA001, got %v", ruleIDs(f))
	}
	for _, x := range f {
		if x.RuleID == "DA001" && x.Severity != SeverityCritical {
			t.Errorf("DA001 severity = %q, want critical", x.Severity)
		}
	}
}

func TestDA001IgnoresStringLiterals(t *testing.T) {
	f := AnalyzeMigrations([]MigrationFile{one("INSERT INTO audit_log (msg) VALUES ('planned DROP TABLE users later');")}, AnalyzeOptions{})
	if len(f) != 0 {
		t.Errorf("expected no findings, got %+v", f)
	}
}

func TestDA002ColumnNarrowing(t *testing.T) {
	f := AnalyzeMigrations([]MigrationFile{one("ALTER TABLE orders ALTER COLUMN qty TYPE smallint;")}, AnalyzeOptions{})
	if !hasRule(f, "DA002") {
		t.Errorf("expected DA002, got %v", ruleIDs(f))
	}

	widening := AnalyzeMigrations([]MigrationFile{
		one("ALTER TABLE orders ALTER COLUMN qty TYPE bigint;"),
		{Path: "migrations/001_up.sql", SQL: "ALTER TABLE orders ALTER COLUMN note TYPE text;"},
	}, AnalyzeOptions{})
	if n := countRule(widening, "DA002"); n != 0 {
		t.Errorf("widening should not fire DA002, got %d", n)
	}
}

func TestDA002VarcharShrink(t *testing.T) {
	sql := strings.Join([]string{
		"CREATE TABLE users (name varchar(255));",
		"ALTER TABLE users ALTER COLUMN name TYPE varchar(50);",
	}, "\n")
	f := AnalyzeMigrations([]MigrationFile{one(sql)}, AnalyzeOptions{})
	if countRule(f, "DA002") < 1 {
		t.Errorf("expected DA002 varchar shrink, got %v", ruleIDs(f))
	}
}

func TestDA003CreateIndexWithoutConcurrently(t *testing.T) {
	f := AnalyzeMigrations([]MigrationFile{one("CREATE INDEX idx_users_email ON users (email);")}, AnalyzeOptions{})
	if !hasRule(f, "DA003") {
		t.Errorf("expected DA003, got %v", ruleIDs(f))
	}

	a := AnalyzeMigrations([]MigrationFile{one("CREATE INDEX CONCURRENTLY idx_users_email ON users (email);")}, AnalyzeOptions{})
	if n := countRule(a, "DA003"); n != 0 {
		t.Errorf("CONCURRENTLY should not fire DA003, got %d", n)
	}

	b := AnalyzeMigrations([]MigrationFile{one("CREATE INDEX idx_x ON t (c);")}, AnalyzeOptions{Dialect: "generic"})
	if n := countRule(b, "DA003"); n != 0 {
		t.Errorf("generic dialect should not fire DA003, got %d", n)
	}
}

func TestDA004ForeignKeyWithoutIndex(t *testing.T) {
	f := AnalyzeMigrations([]MigrationFile{one("ALTER TABLE orders ADD CONSTRAINT fk_customer FOREIGN KEY (customer_id) REFERENCES customers (id);")}, AnalyzeOptions{})
	if !hasRule(f, "DA004") {
		t.Errorf("expected DA004, got %v", ruleIDs(f))
	}

	files := []MigrationFile{
		one("ALTER TABLE orders ADD FOREIGN KEY (customer_id) REFERENCES customers (id);"),
		{Path: "migrations/002_idx.sql", SQL: "CREATE INDEX idx_orders_customer ON orders (customer_id);"},
	}
	g := AnalyzeMigrations(files, AnalyzeOptions{})
	// DA003 fires on the second file but DA004 must not.
	if n := countRule(g, "DA004"); n != 0 {
		t.Errorf("indexed FK should not fire DA004, got %d", n)
	}
	if !hasRule(g, "DA003") {
		t.Errorf("expected DA003 on the plain index file, got %v", ruleIDs(g))
	}
}

func TestDA005NotNullWithoutDefault(t *testing.T) {
	f := AnalyzeMigrations([]MigrationFile{one("ALTER TABLE users ADD COLUMN status varchar(32) NOT NULL;")}, AnalyzeOptions{})
	if !hasRule(f, "DA005") {
		t.Errorf("expected DA005, got %v", ruleIDs(f))
	}

	g := AnalyzeMigrations([]MigrationFile{one("ALTER TABLE users ADD COLUMN status varchar(32) NOT NULL DEFAULT 'active';")}, AnalyzeOptions{})
	if n := countRule(g, "DA005"); n != 0 {
		t.Errorf("DEFAULT should not fire DA005, got %d", n)
	}
}

func TestDA006MissingDownMigration(t *testing.T) {
	f := AnalyzeMigrations([]MigrationFile{{Path: "migrations/007_add_index.up.sql", SQL: "SELECT 1;"}}, AnalyzeOptions{DownMigrations: []string{}})
	if !hasRule(f, "DA006") {
		t.Errorf("expected DA006, got %v", ruleIDs(f))
	}

	g := AnalyzeMigrations([]MigrationFile{{Path: "migrations/007_add_index.up.sql", SQL: "SELECT 1;"}},
		AnalyzeOptions{DownMigrations: []string{"migrations/007_add_index.down.sql"}})
	if n := countRule(g, "DA006"); n != 0 {
		t.Errorf("paired down should not fire DA006, got %d", n)
	}
}

func TestDA007SetNotNullWithoutBackfillMarker(t *testing.T) {
	f := AnalyzeMigrations([]MigrationFile{one("ALTER TABLE users ALTER COLUMN email SET NOT NULL;")}, AnalyzeOptions{})
	if !hasRule(f, "DA007") {
		t.Errorf("expected DA007, got %v", ruleIDs(f))
	}

	g := AnalyzeMigrations([]MigrationFile{one("-- devagent:backfilled\nALTER TABLE users ALTER COLUMN email SET NOT NULL;")}, AnalyzeOptions{})
	if n := countRule(g, "DA007"); n != 0 {
		t.Errorf("marker should suppress DA007, got %d", n)
	}
}

func TestDA008ConcurrentlyInsideTransaction(t *testing.T) {
	sql := "BEGIN;\nCREATE INDEX CONCURRENTLY idx_a ON t (a);\nCOMMIT;"
	f := AnalyzeMigrations([]MigrationFile{one(sql)}, AnalyzeOptions{})
	if !hasRule(f, "DA008") {
		t.Errorf("expected DA008, got %v", ruleIDs(f))
	}

	g := AnalyzeMigrations([]MigrationFile{one("CREATE INDEX CONCURRENTLY idx_a ON t (a);")}, AnalyzeOptions{})
	if n := countRule(g, "DA008"); n != 0 {
		t.Errorf("outside a tx should not fire DA008, got %d", n)
	}
}

func TestMigrationMaskingAndMultiLine(t *testing.T) {
	commented := strings.Join([]string{
		"/*",
		"DROP TABLE legacy;",
		"*/",
		"UPDATE notes SET body = 'see DROP COLUMN old_col discussion' WHERE id = 1;",
	}, "\n")
	if f := AnalyzeMigrations([]MigrationFile{one(commented)}, AnalyzeOptions{}); len(f) != 0 {
		t.Errorf("masked keywords should produce no findings, got %+v", f)
	}

	multiLine := strings.Join([]string{
		"ALTER TABLE invoices",
		"  ALTER COLUMN total TYPE smallint,",
		"  ADD COLUMN memo text;",
	}, "\n")
	found := ruleIDs(AnalyzeMigrations([]MigrationFile{one(multiLine)}, AnalyzeOptions{}))
	if !hasRule([]Finding{{RuleID: "x"}}, "x") {
		t.Fatal("sanity")
	}
	if !contains(found, "DA002") {
		t.Errorf("expected DA002 across lines, got %v", found)
	}
	if contains(found, "DA005") {
		t.Errorf("DA005 should not fire on the nullable memo column, got %v", found)
	}

	sql := strings.Join([]string{"SELECT 1;", "", "DROP TABLE users;"}, "\n")
	for _, f := range AnalyzeMigrations([]MigrationFile{one(sql)}, AnalyzeOptions{}) {
		if f.RuleID == "DA001" {
			if f.Line == nil || *f.Line != 3 {
				t.Errorf("DA001 line = %v, want 3", f.Line)
			}
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
