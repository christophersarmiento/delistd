package config

import (
	"path/filepath"
	"testing"
)

func TestWhatsNewMarker(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if WhatsNewSeen(p) {
		t.Fatal("not seen before it's marked")
	}
	MarkWhatsNewSeen(p)
	if !WhatsNewSeen(p) {
		t.Error("marker not remembered")
	}
}

func TestGetenvIgnoresTheLegacyName(t *testing.T) {
	t.Setenv("DELISTD_X", "")
	t.Setenv("ERASER_X", "old")
	if got := Getenv("DELISTD_X"); got != "" {
		t.Errorf("ERASER_* must not be read any more, got %q", got)
	}
	t.Setenv("DELISTD_X", "new")
	if got := Getenv("DELISTD_X"); got != "new" {
		t.Errorf("DELISTD_X = %q", got)
	}
}
