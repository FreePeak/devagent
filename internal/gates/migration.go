package gates

import (
	"fmt"
	"regexp"
	"strings"
)

// G3 static migration-analysis rule engine (src/validation/migration-rules.ts).
// Line-oriented heuristic SQL scanning: not a parser. String literals and
// comments are masked before dangerous-pattern matching so that SQL keywords
// appearing inside strings/comments never fire.

// MigrationFile mirrors src/validation/migration-rules.ts MigrationFile.
type MigrationFile struct {
	Path string
	SQL  string
}

// AnalyzeOptions mirrors src/validation/migration-rules.ts AnalyzeOptions.
// Dialect is "" (postgres) or "generic". DownMigrations set to a non-nil
// slice (even empty) enables the DA006 pairing check, mirroring the TS
// `opts.downMigrations` truthiness check.
type AnalyzeOptions struct {
	DownMigrations []string
	Dialect        string
}

// maskedSQL mirrors the TS MaskedSql helper: text with string literals and
// comments replaced by spaces (newlines kept), plus an offset→line lookup.
type maskedSQL struct {
	masked     string
	lineStarts []int
}

// maskable matches comments and single-quoted literals (” escape).
// Alternation order ensures a `--` inside a literal stays inside the literal
// match and vice versa.
var maskable = regexp.MustCompile(`--[^\n]*|/\*[\s\S]*?(?:\*/|$)|'(?:[^']|'')*(?:'|$)`)

func maskSQL(sql string) maskedSQL {
	masked := maskable.ReplaceAllStringFunc(sql, func(tok string) string {
		return strings.Map(func(r rune) rune {
			if r == '\n' {
				return '\n'
			}
			return ' '
		}, tok)
	})

	// prefix sum of newlines for offset -> line lookup
	lineStarts := []int{0}
	for k := 0; k < len(masked); k++ {
		if masked[k] == '\n' {
			lineStarts = append(lineStarts, k+1)
		}
	}
	return maskedSQL{masked: masked, lineStarts: lineStarts}
}

// lineOf mirrors MaskedSql.lineOf: greatest lineStarts[i] <= offset, 1-based.
func (m *maskedSQL) lineOf(offset int) int {
	lo, hi := 0, len(m.lineStarts)-1
	for lo < hi {
		mid := (lo + hi + 1) >> 1
		if m.lineStarts[mid] <= offset {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo + 1
}

var whitespaceRuns = regexp.MustCompile(`\s+`)

// normalize mirrors normalize(): collapse whitespace so multi-line statements
// match single regexes.
func normalize(masked string) string {
	return whitespaceRuns.ReplaceAllString(masked, " ")
}

func mkFinding(ruleID string, severity Severity, file string, line *int, message string) Finding {
	return Finding{RuleID: ruleID, Severity: severity, Message: message, File: file, Line: line}
}

func intPtr(n int) *int { return &n }

// ---------- per-file rules ----------

var dropRe = regexp.MustCompile(`(?i)\b(DROP\s+(TABLE|COLUMN|DATABASE|SCHEMA)|TRUNCATE\s+[^;]*CASCADE)\b`)

// statement mirrors the TS Statement: whitespace-normalized statement text
// and the offset in masked source where the statement begins.
type statement struct {
	text  string
	start int
}

// splitStatements mirrors splitStatements(): split on ';' outside masked
// regions, keeping only non-blank statements.
func splitStatements(masked string) []statement {
	out := []statement{}
	start := 0
	for {
		semi := strings.IndexByte(masked[start:], ';')
		hasSemi := semi >= 0
		end := len(masked)
		if hasSemi {
			end = start + semi
		}
		lead := strings.IndexFunc(masked[start:end], func(r rune) bool { return r != ' ' && r != '\t' && r != '\n' && r != '\r' })
		textStart := start
		if lead >= 0 {
			textStart = start + lead
		}
		norm := normalize(masked[textStart:end])
		if strings.TrimSpace(norm) != "" {
			out = append(out, statement{text: norm, start: textStart})
		}
		if !hasSemi {
			break
		}
		start = end + 1
	}
	return out
}

// firstToken mirrors firstToken(): trimmed statement, truncated with "..."
// past 60 chars.
func firstToken(stmt string) string {
	t := strings.TrimSpace(stmt)
	if len(t) > 60 {
		return t[:57] + "..."
	}
	return t
}

func ruleDa001(file MigrationFile, m *maskedSQL) []Finding {
	findings := []Finding{}
	for _, stmt := range splitStatements(m.masked) {
		loc := dropRe.FindStringIndex(stmt.text)
		if loc == nil {
			continue
		}
		findings = append(findings, mkFinding("DA001", SeverityCritical, file.Path,
			intPtr(m.lineOf(stmt.start+loc[0])),
			fmt.Sprintf("Destructive operation detected (%s): %s", strings.TrimSpace(stmt.text[loc[0]:loc[1]]), firstToken(stmt.text))))
	}
	return findings
}

// alterTypeRe ports the TS pattern with a lookahead via a lazy capture group
// followed by a consuming terminator alternation:
// /ALTER\s+COLUMN\s+(\w+)\s+TYPE\s+([\w\s()[\],]+?)(?=\b(?:USING|NOT\s+NULL|DEFAULT|COLLATE)\b|[;,]|$)/i
var alterTypeRe = regexp.MustCompile(`(?i)ALTER\s+COLUMN\s+(\w+)\s+TYPE\s+([\w\s()\[\],]*?)(?:\b(?:USING|NOT\s+NULL|DEFAULT|COLLATE)\b|[;,]|$)`)

var narrowingTypeRe = regexp.MustCompile(`(?i)^(smallint|tinyint)$`)

var varcharWidthRe = regexp.MustCompile(`(?i)^varchar\s*\(\s*(\d+)\s*\)$`)

func ruleDa002(file MigrationFile, m *maskedSQL, priorVarcharWidths map[string]int) []Finding {
	findings := []Finding{}
	for _, stmt := range splitStatements(m.masked) {
		match := alterTypeRe.FindStringSubmatchIndex(stmt.text)
		if match == nil {
			continue
		}
		column := stmt.text[match[2]:match[3]]
		targetType := strings.TrimSpace(normalize(stmt.text[match[4]:match[5]]))

		// explicit narrowing targets
		if narrowingTypeRe.MatchString(targetType) {
			findings = append(findings, mkFinding("DA002", SeverityCritical, file.Path,
				intPtr(m.lineOf(stmt.start)),
				fmt.Sprintf("Column %q narrowed to %s", column, targetType)))
			continue
		}
		// any numeric type narrowed to smallint/tinyint handled above; also
		// varchar shrink: look for a prior width known and larger.
		vm := varcharWidthRe.FindStringSubmatch(targetType)
		if vm == nil {
			continue
		}
		width := atoiMust(vm[1])
		prior, ok := priorVarcharWidths[strings.ToLower(column)]
		if ok && width < prior {
			findings = append(findings, mkFinding("DA002", SeverityCritical, file.Path,
				intPtr(m.lineOf(stmt.start)),
				fmt.Sprintf("varchar(%d) shrinks %q from varchar(%d)", width, column, prior)))
		}
	}
	return findings
}

func isPostgresPath(path string) bool {
	if strings.HasSuffix(strings.ToLower(path), ".sql") {
		return true
	}
	if containsFold(path, ".pg.") {
		return true
	}
	return regexp.MustCompile(`(?i)postgres|(^|/)pg(/|$)`).MatchString(path)
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

var createIndexPrefix = regexp.MustCompile(`(?i)CREATE\s+(?:UNIQUE\s+)?INDEX\s+`)

var concurrentlyPrefix = regexp.MustCompile(`(?i)^CONCURRENTLY`)

func ruleDa003(file MigrationFile, m *maskedSQL, opts AnalyzeOptions) []Finding {
	if opts.Dialect == "generic" {
		return []Finding{}
	}
	if !isPostgresPath(file.Path) {
		return []Finding{}
	}
	findings := []Finding{}
	for _, stmt := range splitStatements(m.masked) {
		loc := createIndexPrefix.FindStringIndex(stmt.text)
		if loc == nil {
			continue
		}
		// TS negative lookahead (?!CONCURRENTLY): only the first prefix match
		// is considered, and it must not be followed by CONCURRENTLY.
		if concurrentlyPrefix.MatchString(stmt.text[loc[1]:]) {
			continue
		}
		findings = append(findings, mkFinding("DA003", SeverityHigh, file.Path,
			intPtr(m.lineOf(stmt.start+loc[0])), "CREATE INDEX without CONCURRENTLY"))
	}
	return findings
}

const (
	// fkRePattern ports /ADD\s+(CONSTRAINT\s+\w+\s+)?FOREIGN\s+KEY\s*\(\s*([\w",\s]+?)\s*\)\s*REFERENCES\s+([\w."`[\]]+)/gi
	fkRePattern = `(?i)ADD\s+(CONSTRAINT\s+\w+\s+)?FOREIGN\s+KEY\s*\(\s*([\w",\s]+?)\s*\)\s*REFERENCES\s+([\w."` + "`" + `[\]]+)`
	// alterTablePattern ports /ALTER\s+TABLE\s+([\w."`[\]]+)/i
	alterTablePattern = `(?i)ALTER\s+TABLE\s+([\w."` + "`" + `[\]]+)`
)

var (
	fkRe        = regexp.MustCompile(fkRePattern)
	alterTable  = regexp.MustCompile(alterTablePattern)
	quoteJunkRe = regexp.MustCompile(`["` + "`" + `\[\]]`)
)

// stripQuoteChars removes the quote/bracket characters the TS rules strip
// with .replace(/["`[\]]/g, ”).
func stripQuoteChars(s string) string {
	return quoteJunkRe.ReplaceAllString(s, "")
}

func lastPathSegment(s string) string {
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func ruleDa004(file MigrationFile, m *maskedSQL, indexes map[string]bool) []Finding {
	findings := []Finding{}
	for _, stmt := range splitStatements(m.masked) {
		at := alterTable.FindStringSubmatch(stmt.text)
		sourceTable := ""
		if at != nil {
			sourceTable = lastPathSegment(stripQuoteChars(at[1]))
		}
		for _, loc := range fkRe.FindAllStringSubmatchIndex(stmt.text, -1) {
			columns := []string{}
			for _, c := range strings.Split(stmt.text[loc[4]:loc[5]], ",") {
				col := strings.TrimSpace(stripQuoteChars(c))
				if col != "" {
					columns = append(columns, col)
				}
			}
			table := sourceTable
			for _, col := range columns {
				if !indexes[strings.ToLower(table)+"."+strings.ToLower(col)] {
					findings = append(findings, mkFinding("DA004", SeverityHigh, file.Path,
						intPtr(m.lineOf(stmt.start+loc[0])),
						fmt.Sprintf("Foreign key on %q has no matching index", table+"."+col)))
				}
			}
		}
	}
	return findings
}

// addColumnRe ports /ADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?(?:(\w+)\.)?(\w+)\s+([\w()]+)([^;]*)/gi
var addColumnRe = regexp.MustCompile(`(?i)ADD\s+COLUMN\s+(?:IF\s+NOT\s+EXISTS\s+)?(?:(\w+)\.)?(\w+)\s+([\w()]+)([^;]*)`)

var notNullRe = regexp.MustCompile(`(?i)\bNOT\s+NULL\b`)

var defaultRe = regexp.MustCompile(`(?i)\bDEFAULT\b`)

func ruleDa005(file MigrationFile, m *maskedSQL) []Finding {
	findings := []Finding{}
	for _, stmt := range splitStatements(m.masked) {
		for _, match := range addColumnRe.FindAllStringSubmatch(stmt.text, -1) {
			rest := match[4]
			if notNullRe.MatchString(rest) && !defaultRe.MatchString(rest) {
				findings = append(findings, mkFinding("DA005", SeverityHigh, file.Path,
					intPtr(m.lineOf(stmt.start)),
					fmt.Sprintf("ADD COLUMN %q NOT NULL without DEFAULT", match[2])))
			}
		}
	}
	return findings
}

var setNotNullRe = regexp.MustCompile(`(?i)SET\s+NOT\s+NULL`)

var alterColumnRe = regexp.MustCompile(`(?i)ALTER\s+COLUMN\s+\w+`)

var backfillMarkerRe = regexp.MustCompile(`(?i)--\s*devagent:backfilled\b`)

func ruleDa007(file MigrationFile, m *maskedSQL) []Finding {
	findings := []Finding{}
	lines := strings.Split(file.SQL, "\n")
	for _, stmt := range splitStatements(m.masked) {
		if setNotNullRe.FindStringIndex(stmt.text) == nil {
			continue
		}
		if alterColumnRe.FindStringIndex(stmt.text) == nil {
			continue
		}
		// backfill marker may appear as a comment anywhere before the
		// statement in the file, or immediately above it. Heuristic: marker
		// present anywhere earlier in the file counts.
		upToStatementEnd := m.lineOf(stmt.start)
		sawMarker := false
		for ln := 0; ln < upToStatementEnd && ln < len(lines); ln++ {
			if backfillMarkerRe.MatchString(lines[ln]) {
				sawMarker = true
				break
			}
		}
		if !sawMarker {
			findings = append(findings, mkFinding("DA007", SeverityHigh, file.Path,
				intPtr(m.lineOf(stmt.start)),
				`SET NOT NULL without preceding "-- devagent:backfilled" marker`))
		}
	}
	return findings
}

var beginTxRe = regexp.MustCompile(`\bBEGIN\s*(TRANSACTION|WORK)?\s*;?\s*$`)

var commitRe = regexp.MustCompile(`\bCOMMIT\b`)

var concurrentlyIndexRe = regexp.MustCompile(`(?i)CREATE\s+INDEX\s+CONCURRENTLY`)

func ruleDa008(file MigrationFile, m *maskedSQL) []Finding {
	findings := []Finding{}
	lines := strings.Split(m.masked, "\n")
	inTx := false
	txStartLine := 0
	for i, line := range lines {
		upper := strings.ToUpper(line)
		if beginTxRe.MatchString(upper) && !inTx {
			inTx = true
			txStartLine = i + 1
		} else if commitRe.MatchString(upper) && inTx {
			inTx = false
		}
		if inTx && concurrentlyIndexRe.MatchString(line) {
			findings = append(findings, mkFinding("DA008", SeverityMedium, file.Path,
				intPtr(i+1),
				"CREATE INDEX CONCURRENTLY inside BEGIN/COMMIT block (starting line "+itoa(txStartLine)+")"))
		}
	}
	return findings
}

// ---------- DA006 ----------

// migrationBase mirrors base(): strip the final extension, lowercase.
func migrationBase(p string) string {
	if i := strings.LastIndexByte(p, '.'); i >= 0 && i < len(p)-1 {
		p = p[:i]
	}
	return strings.ToLower(p)
}

var downFileRe = regexp.MustCompile(`(?i)\.down\.`)

var upSuffixRe = regexp.MustCompile(`(?i)\.up$`)

func ruleDa006(files []MigrationFile, downMigrations []string) []Finding {
	findings := []Finding{}
	downBases := map[string]bool{}
	for _, d := range downMigrations {
		downBases[migrationBase(d)] = true
	}
	for _, file := range files {
		if downFileRe.MatchString(file.Path) {
			continue
		}
		b := migrationBase(file.Path)
		// matching-name convention: "<name>.down.sql" pairs with "<name>.up.sql"
		expectedDown := upSuffixRe.ReplaceAllString(b, "") + ".down"
		if !downBases[expectedDown] && !downBases[b+".down"] {
			findings = append(findings, mkFinding("DA006", SeverityMedium, file.Path, nil, "No corresponding down-migration found"))
		}
	}
	return findings
}

// ---------- entry point ----------

// indexRegistryRe ports the DA004 index-registry scanner:
// /CREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?(?:[\w."`[\]]+\s+ON\s+)?([\w."`[\]]+)\s*(?:USING\s+\w+\s*)?\(\s*([^)]+?)\s*\)/gi
var indexRegistryRe = regexp.MustCompile(`(?i)CREATE\s+(?:UNIQUE\s+)?INDEX\s+(?:CONCURRENTLY\s+)?(?:IF\s+NOT\s+EXISTS\s+)?(?:[\w."` + "`" + `[\]]+\s+ON\s+)?([\w."` + "`" + `[\]]+)\s*(?:USING\s+\w+\s*)?\(\s*([^)]+?)\s*\)`)

var ascDescSuffixRe = regexp.MustCompile(`(?i)\s+(ASC|DESC)\b.*$`)

var indexColJunkRe = regexp.MustCompile(`["` + "`" + `\[\]\s]`)

// collectVarcharWidths mirrors collectVarcharWidths(): scan all files for
// CREATE TABLE-style "<col> varchar(n)" definitions ahead of the per-file
// rules, so DA002 can detect shrinks within the file set.
func collectVarcharWidths(files []MigrationFile, allMasked map[string]*maskedSQL) map[string]int {
	priorVarcharWidths := map[string]int{}
	var createRe = regexp.MustCompile(`(?i)\b(\w+)\s+varchar\s*\(\s*(\d+)\s*\)`)
	for _, file := range files {
		m := allMasked[file.Path]
		if m == nil {
			continue
		}
		for _, stmt := range splitStatements(m.masked) {
			for _, cm := range createRe.FindAllStringSubmatch(stmt.text, -1) {
				priorVarcharWidths[strings.ToLower(cm[1])] = atoiMust(cm[2])
			}
		}
	}
	return priorVarcharWidths
}

// AnalyzeMigrations mirrors analyzeMigrations(): run DA001–DA008 over the
// given migration files, returning findings in the TS rule order.
func AnalyzeMigrations(files []MigrationFile, opts AnalyzeOptions) []Finding {
	findings := []Finding{}

	maskedByPath := map[string]*maskedSQL{}
	for _, file := range files {
		m := maskSQL(file.SQL)
		maskedByPath[file.Path] = &m
	}

	// cross-file index registry for DA004
	indexes := map[string]bool{}
	for _, file := range files {
		m := maskedByPath[file.Path]
		if m == nil {
			continue
		}
		for _, stmt := range splitStatements(m.masked) {
			for _, im := range indexRegistryRe.FindAllStringSubmatch(stmt.text, -1) {
				table := lastPathSegment(stripQuoteChars(im[1]))
				for _, col := range strings.Split(im[2], ",") {
					clean := indexColJunkRe.ReplaceAllString(ascDescSuffixRe.ReplaceAllString(col, ""), "")
					if clean != "" {
						indexes[strings.ToLower(table)+"."+strings.ToLower(clean)] = true
					}
				}
			}
		}
	}

	priorVarcharWidths := collectVarcharWidths(files, maskedByPath)

	for _, file := range files {
		m := maskedByPath[file.Path]
		if m == nil {
			continue
		}
		findings = append(findings, ruleDa001(file, m)...)
		findings = append(findings, ruleDa002(file, m, priorVarcharWidths)...)
		findings = append(findings, ruleDa003(file, m, opts)...)
		findings = append(findings, ruleDa004(file, m, indexes)...)
		findings = append(findings, ruleDa005(file, m)...)
		findings = append(findings, ruleDa007(file, m)...)
		findings = append(findings, ruleDa008(file, m)...)
	}

	if opts.DownMigrations != nil {
		findings = append(findings, ruleDa006(files, opts.DownMigrations)...)
	}

	return findings
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
