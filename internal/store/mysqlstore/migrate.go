package mysqlstore

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

func Migrate(ctx context.Context, db *sql.DB) error {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		version := entry.Name()
		var exists int
		err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'schema_migration'").Scan(&exists)
		if err != nil {
			return fmt.Errorf("inspect migration table: %w", err)
		}
		if exists > 0 {
			var applied int
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migration WHERE version = ?", version).Scan(&applied); err != nil {
				return fmt.Errorf("check migration %s: %w", version, err)
			}
			if applied > 0 {
				continue
			}
		}
		body, err := migrationFS.ReadFile("migrations/" + version)
		if err != nil {
			return err
		}
		// MySQL DDL commits implicitly. Execute simple version-controlled statements
		// one by one, then record the version only after all statements succeed.
		for _, statement := range strings.Split(string(body), ";") {
			statement = strings.TrimSpace(statement)
			if statement == "" {
				continue
			}
			if _, err := db.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("apply migration %s: %w", version, err)
			}
		}
		if _, err := db.ExecContext(ctx, "INSERT INTO schema_migration(version) VALUES (?)", version); err != nil {
			return fmt.Errorf("record migration %s: %w", version, err)
		}
	}
	return nil
}
