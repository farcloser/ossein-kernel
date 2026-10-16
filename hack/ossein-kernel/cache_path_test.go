//go:build darwin && arm64

package main

import (
	"path/filepath"
	"testing"
)

func TestCachePath(t *testing.T) {
	t.Parallel()

	const digest = "039aef84f2b0994aeda3f4fcfc3d02ec9d7a9bbb9020ea264c43f446c860f606"

	cases := []struct {
		name string
		base string
		sha  string
		want string
	}{
		{
			"keyed by digest",
			filepath.Join("scratch", "source.tar.xz"),
			digest,
			filepath.Join("scratch", "source-039aef84f2b0.tar.xz"),
		},
		{
			"whole extension chain kept",
			filepath.Join("scratch", "seed-kernel.tar.zst"),
			digest,
			filepath.Join("scratch", "seed-kernel-039aef84f2b0.tar.zst"),
		},
		{"no digest, no key", filepath.Join("scratch", "source.tar.xz"), "", filepath.Join("scratch", "source.tar.xz")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := cachePath(tc.base, tc.sha); got != tc.want {
				t.Fatalf("cachePath(%q, %q) = %q, want %q", tc.base, tc.sha, got, tc.want)
			}
		})
	}
}
