package store

import "testing"

func TestLegacySecretsMigrateToProfile(t *testing.T) {
	s := Secrets{VPNKey: "vpn://x", RawConfig: "[Interface]", ConfigFor: "из другого приложения"}
	s.Normalize()
	if len(s.Profiles) != 1 || s.VPNKey != "" || s.RawConfig != "" {
		t.Fatalf("not migrated: %+v", s)
	}
	p := s.Active()
	if p == nil || p.Name != "Профиль 1" || p.VPNKey != "vpn://x" || p.RawConfig != "[Interface]" {
		t.Fatalf("active profile wrong: %+v", p)
	}
	s.Normalize() // idempotent
	if len(s.Profiles) != 1 {
		t.Fatal("migrated twice")
	}
}

func TestRemoveActivatesRemaining(t *testing.T) {
	s := Secrets{Profiles: []Profile{{ID: "a", Name: "A"}, {ID: "b", Name: "B"}}, ActiveID: "a"}
	s.Remove("a")
	if s.ActiveID != "b" || len(s.Profiles) != 1 {
		t.Fatalf("%+v", s)
	}
	s.Remove("b")
	if s.Active() != nil || len(s.Profiles) != 0 {
		t.Fatalf("%+v", s)
	}
}
