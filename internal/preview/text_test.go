package preview

import "testing"

func TestSyntaxFilename(t *testing.T) {
	tests := []struct{ input, want string }{
		{`{"ok":true}`, "clipboard.json"},
		{"#!/usr/bin/env python3\nprint('hi')", "clipboard.py"},
		{"#!/bin/sh\necho hi", "clipboard.sh"},
		{"package main\nfunc main() {}", "clipboard.go"},
		{"fn main() {}", "clipboard.rs"},
		{"plain text", "clipboard.txt"},
	}
	for _, tt := range tests {
		if got := syntaxFilename(tt.input); got != tt.want {
			t.Errorf("syntaxFilename(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestRenderTextFallback(t *testing.T) {
	got := RenderText("one two three", 5)
	if len(got) == 0 {
		t.Fatal("expected a non-empty preview")
	}
}
