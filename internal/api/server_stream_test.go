package api

import (
	"fmt"
	"testing"
)

type fakeResolver struct{}

func (fakeResolver) Resolve(upstream string, overridePort int) (string, error) {
	if upstream == "  " {
		return "", fmt.Errorf("upstream is required")
	}
	if overridePort < 0 || overridePort > 65535 {
		return "", fmt.Errorf("container_port must be between 1 and 65535")
	}
	if overridePort == 0 {
		return "10.10.0.12:5432", nil
	}
	return fmt.Sprintf("10.10.0.12:%d", overridePort), nil
}

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
			name:          "resolves upstream with existing port",
			upstream:      "postgres-db:5432",
			containerPort: 0,
			want:          "10.10.0.12:5432",
		},
		{
			name:          "resolves upstream with override port",
			upstream:      "postgres-db",
			containerPort: 3306,
			want:          "10.10.0.12:3306",
		},
		{
			name:          "rejects empty upstream",
			upstream:      "  ",
			containerPort: 5432,
			wantErr:       "upstream is required",
		},
		{
			name:          "rejects invalid container port",
			upstream:      "postgres-db",
			containerPort: 70000,
			wantErr:       "container_port must be between 1 and 65535",
		},
	}

	resolver := fakeResolver{}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := normalizeStreamUpstream(resolver, tc.upstream, tc.containerPort)
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
