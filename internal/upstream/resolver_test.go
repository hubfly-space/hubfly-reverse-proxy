package upstream

import "testing"

func TestResolve(t *testing.T) {
	t.Parallel()

	r := NewDefaultResolver()
	tests := []struct {
		name         string
		upstream     string
		overridePort int
		want         string
		wantErr      string
	}{
		{
			name:     "keeps ipv4 with port",
			upstream: "127.0.0.1:8080",
			want:     "127.0.0.1:8080",
		},
		{
			name:         "overrides port",
			upstream:     "127.0.0.1:8080",
			overridePort: 9000,
			want:         "127.0.0.1:9000",
		},
		{
			name:     "rejects missing port",
			upstream: "127.0.0.1",
			wantErr:  "upstream must include a valid port",
		},
		{
			name:     "resolves localhost",
			upstream: "localhost:80",
			want:     "127.0.0.1:80",
		},
		{
			name:     "rejects empty upstream",
			upstream: "",
			wantErr:  "upstream is required",
		},
		{
			name:         "rejects invalid override port",
			upstream:     "localhost:80",
			overridePort: 70000,
			wantErr:      "container_port must be between 1 and 65535",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := r.Resolve(tc.upstream, tc.overridePort)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error %q, got nil", tc.wantErr)
				}
				if err.Error() != tc.wantErr {
					t.Fatalf("expected error %q, got %q", tc.wantErr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if got != tc.want {
				t.Fatalf("expected %q, got %q", tc.want, got)
			}
		})
	}
}
