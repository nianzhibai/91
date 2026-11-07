package main

import (
	"context"
	"testing"

	"github.com/nianzhibai/91/backend/internal/catalog"
)

func newThemeTestApp(t *testing.T) *App {
	t.Helper()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	return &App{cat: cat, theme: "dark"}
}

func TestLoadThemeMigratesRetiredLightPalettes(t *testing.T) {
	for _, tc := range []struct{ stored, want string }{
		{"", "dark"}, {"dark", "dark"}, {"light", "light"},
		{"pink", "light"}, {"sky", "light"}, {"unknown", "dark"},
	} {
		t.Run(tc.stored, func(t *testing.T) {
			a := newThemeTestApp(t)
			ctx := context.Background()
			if tc.stored != "" {
				if err := a.cat.SetSetting(ctx, "ui.theme", tc.stored); err != nil {
					t.Fatal(err)
				}
			}
			a.loadTheme(ctx)
			if got := a.Theme(); got != tc.want {
				t.Fatalf("theme = %q, want %q", got, tc.want)
			}
			if tc.stored == "pink" || tc.stored == "sky" {
				stored, err := a.cat.GetSetting(ctx, "ui.theme", "")
				if err != nil || stored != "light" {
					t.Fatalf("migration was not persisted: theme=%q err=%v", stored, err)
				}
			}
		})
	}
}

func TestSetThemePersistsAcrossReload(t *testing.T) {
	a := newThemeTestApp(t)
	ctx := context.Background()
	for _, theme := range []string{"light", "dark"} {
		if err := a.SetTheme(ctx, theme); err != nil {
			t.Fatal(err)
		}
		reloaded := &App{cat: a.cat}
		reloaded.loadTheme(ctx)
		if a.Theme() != theme || reloaded.Theme() != theme {
			t.Fatalf("theme %q was not applied and persisted", theme)
		}
	}
}

func TestSetThemeFailureKeepsCurrentPreference(t *testing.T) {
	a := newThemeTestApp(t)
	ctx := context.Background()
	if err := a.SetTheme(ctx, "light"); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"pink", "sky", "orange", "auto", ""} {
		if err := a.SetTheme(ctx, invalid); err == nil {
			t.Fatalf("accepted unsupported theme %q", invalid)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := a.SetTheme(cancelled, "dark"); err == nil {
		t.Fatal("expected cancelled persistence to fail")
	}
	stored, err := a.cat.GetSetting(ctx, "ui.theme", "")
	if err != nil || stored != "light" || a.Theme() != "light" {
		t.Fatalf("failed update changed the theme: stored=%q active=%q err=%v", stored, a.Theme(), err)
	}
}
