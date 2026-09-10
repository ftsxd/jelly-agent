package session

import (
	"os"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"google.golang.org/adk/session/database"
)

// A probe, not a unit test: it runs ADK's own AutoMigrate against a real
// PostgreSQL and prints what it built. Skipped unless JELLY_PG_DSN is set.
//
// It answers the question the migration plan rests on and could not answer
// from source alone — what ADK's four tables actually become on PG, and
// whether the composite string primary keys that only barely fit MySQL's
// 3072-byte index limit are a problem here.
func TestProbeADKAutoMigrateOnPostgres(t *testing.T) {
	dsn := os.Getenv("JELLY_PG_DSN")
	if dsn == "" {
		t.Skip("set JELLY_PG_DSN to probe a real PostgreSQL")
	}
	exclusive(t, dsn)
	svc, err := database.NewSessionService(postgres.Open(dsn),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := database.AutoMigrate(svc); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}

	db, err := gorm.Open(postgres.Open(dsn),
		&gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	type col struct {
		Table, Column, Type string
		Len                 *int
	}
	var cols []col
	if err := db.Raw(`
		SELECT table_name AS "table", column_name AS "column",
		       data_type AS "type", character_maximum_length AS "len"
		FROM information_schema.columns
		WHERE table_schema='public'
		  AND table_name IN ('sessions','events','app_states','user_states')
		ORDER BY table_name, ordinal_position`).Scan(&cols).Error; err != nil {
		t.Fatal(err)
	}
	if len(cols) == 0 {
		t.Fatal("AutoMigrate 报成功，但一张表都没建出来")
	}
	for _, c := range cols {
		n := ""
		if c.Len != nil {
			n = "(" + itoa(*c.Len) + ")"
		}
		t.Logf("%-12s %-24s %s%s", c.Table, c.Column, c.Type, n)
	}

	var idx []struct{ Table, Name, Def string }
	if err := db.Raw(`
		SELECT tablename AS "table", indexname AS "name", indexdef AS "def"
		FROM pg_indexes WHERE schemaname='public'
		  AND tablename IN ('sessions','events','app_states','user_states')
		ORDER BY tablename, indexname`).Scan(&idx).Error; err != nil {
		t.Fatal(err)
	}
	for _, i := range idx {
		t.Logf("INDEX %s", i.Def)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
