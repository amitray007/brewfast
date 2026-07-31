package host

import "testing"

func TestIsSlowHost(t *testing.T) {
	tests := []struct {
		name   string
		rawurl string
		want   bool
	}{
		{
			name:   "github release asset",
			rawurl: "https://github.com/owner/repo/releases/download/v1/app.dmg",
			want:   true,
		},
		{
			name:   "release-assets redirect target",
			rawurl: "https://release-assets.githubusercontent.com/github-production-release-asset/123/file.dmg",
			want:   true,
		},
		{
			name:   "release-assets any path",
			rawurl: "https://release-assets.githubusercontent.com/anything",
			want:   true,
		},
		{
			name:   "vendor cdn",
			rawurl: "https://dl.example.com/app.dmg",
			want:   false,
		},
		{
			name:   "github non-release path (archive)",
			rawurl: "https://github.com/owner/repo/archive/refs/tags/v1.tar.gz",
			want:   false,
		},
		{
			name:   "github release path but too short",
			rawurl: "https://github.com/owner/repo/releases/download",
			want:   false,
		},
		{
			name:   "http scheme rejected",
			rawurl: "http://github.com/owner/repo/releases/download/v1/app.dmg",
			want:   false,
		},
		{
			name:   "http scheme rejected on release-assets",
			rawurl: "http://release-assets.githubusercontent.com/x/file.dmg",
			want:   false,
		},
		{
			name:   "empty string",
			rawurl: "",
			want:   false,
		},
		{
			name:   "malformed url",
			rawurl: "://bad",
			want:   false,
		},
		{
			name:   "github root",
			rawurl: "https://github.com/",
			want:   false,
		},
		{
			name:   "github missing owner or repo",
			rawurl: "https://github.com/owner/releases/download/v1/app.dmg",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSlowHost(tt.rawurl); got != tt.want {
				t.Errorf("IsSlowHost(%q) = %v, want %v", tt.rawurl, got, tt.want)
			}
		})
	}
}
