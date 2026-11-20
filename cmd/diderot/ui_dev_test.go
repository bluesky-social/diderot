package main

import "testing"

func TestUIDevCommandDefaults(t *testing.T) {
	cmd := uiDevCmd()
	if cmd.Use != "ui-dev" {
		t.Fatalf("Use = %q", cmd.Use)
	}
	fixture, err := cmd.Flags().GetString("fixture")
	if err != nil || fixture != "internal/adminui/testdata/ui-dev.json" {
		t.Fatalf("fixture = %q, %v", fixture, err)
	}
	listen, err := cmd.Flags().GetString("listen")
	if err != nil || listen != "127.0.0.1:8082" {
		t.Fatalf("listen = %q, %v", listen, err)
	}
}
