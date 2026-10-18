//go:build darwin && arm64

package main

import (
	"path/filepath"
	"testing"
)

// The recipes export LIMEN_BUILD, the one spelling of the checkout's output
// directory; the builder's scratch directory and its --out default follow it.
func TestBuildDirFollowsLimenBuild(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LIMEN_BUILD", dir)

	if got := buildDir(); got != dir {
		t.Fatalf("buildDir() = %q, want %q", got, dir)
	}

	if got, want := scratchDir(), filepath.Join(dir, "kernel-scratch"); got != want {
		t.Fatalf("scratchDir() = %q, want %q", got, want)
	}
}

// Run by hand from the checkout root, with nothing exported, the builder
// writes under .build/ like the recipes would.
func TestBuildDirByHandIsDotBuild(t *testing.T) {
	t.Setenv("LIMEN_BUILD", "")

	if got := buildDir(); got != ".build" {
		t.Fatalf("buildDir() = %q, want .build", got)
	}
}
