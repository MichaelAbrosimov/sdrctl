package config

import "testing"

// The shipped example is documentation that runs: it must survive the strict
// loader (unknown fields are rejected), and the template-instance mode names
// in it must resolve the way its own comments promise.
func TestExampleConfigLoads(t *testing.T) {
	cfg, err := Load("../../configs/config.example.yaml")
	if err != nil {
		t.Fatalf("configs/config.example.yaml must load: %v", err)
	}
	dev, _ := cfg.DefaultDevice()
	if dev == nil {
		t.Fatal("example config yields no default device")
	}
	if got, err := dev.ResolveMode("868"); err != nil || got != "rtl-433@868" {
		t.Errorf("ResolveMode(868) = %q, %v; want rtl-433@868", got, err)
	}
	if _, err := dev.ResolveMode("rtl-433"); err == nil {
		t.Error("the family name alone must stay ambiguous while both instances exist")
	}
}
