package main

import (
	"context"
	"errors"
	"strings"

	"github.com/nianzhibai/91/backend/internal/auth"
	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/config"
)

func migrateLegacyAdmin(ctx context.Context, cat *catalog.Catalog, manager *config.Manager) (bool, error) {
	return manager.MigrateLegacyAdminCredentials(func(username, password string) error {
		required, err := cat.AdminSetupRequired(ctx)
		if err != nil || !required {
			return err
		}
		username = strings.TrimSpace(username)
		// Empty credentials and the retired public default require first-run setup.
		if username == "" || password == "" || (username == "admin" && password == "admin123") {
			return nil
		}
		hashed, err := auth.HashPassword(password)
		if err != nil {
			return err
		}
		err = cat.InitializeAdmin(ctx, username, hashed)
		if errors.Is(err, catalog.ErrAdminAlreadyInitialized) {
			return nil
		}
		return err
	})
}
