package api

import "testing"

func TestNormalizeStreamUpstream(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		upstream      string
		containerPort int
		want          string
		wantErr       string
	}{
		{
			name:          "keeps host and port",
			upstream:      "postgres-db:5432",
			containerPort: 0,
			want:          "postgres-db:5432",
		},
		{
			name:          "overrides port",
			upstream:      "postgres-db:15432",
			containerPort: 3306,
			want:          "postgres-db:3306",
		},
		{
			name:          "rejects empty upstream",
			upstream:      "  ",
			containerPort: 5432,
			wantErr:       "upstream is required",
		},
		{
			name:          "rejects invalid container port",
			upstream:      "postgres-db:5432",
			containerPort: 70000,
			wantErr:       "container_port must be between 1 and 65535",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := normalizeStreamUpstream(tc.upstream, tc.containerPort)
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
