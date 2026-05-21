package labels

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		labels  map[string]string
		want    *ServiceDef
		wantErr string
	}{
		{
			name:    "nil labels disabled",
			labels:  nil,
			wantErr: "tsserve.enable",
		},
		{
			name:    "not enabled",
			labels:  map[string]string{"tsserve.service": "svc:web", "tsserve.port": "80"},
			wantErr: "tsserve.enable",
		},
		{
			name: "minimal valid",
			labels: map[string]string{
				"tsserve.enable":  "true",
				"tsserve.service": "svc:web",
				"tsserve.port":    "80",
			},
			want: &ServiceDef{
				Service: "svc:web",
				Port:    80,
				Network: "bridge",
				Scheme:  "http",
			},
		},
		{
			name: "all options",
			labels: map[string]string{
				"tsserve.enable":  "true",
				"tsserve.service": "svc:api",
				"tsserve.port":    "3000",
				"tsserve.network": "frontend",
				"tsserve.scheme":  "https",
				"tsserve.caps":    "example.com/cap/read, example.com/cap/admin",
			},
			want: &ServiceDef{
				Service: "svc:api",
				Port:    3000,
				Network: "frontend",
				Scheme:  "https",
				Caps:    []string{"example.com/cap/read", "example.com/cap/admin"},
			},
		},
		{
			name:    "missing service",
			labels:  map[string]string{"tsserve.enable": "true", "tsserve.port": "80"},
			wantErr: "tsserve.service",
		},
		{
			name:    "service without svc: prefix",
			labels:  map[string]string{"tsserve.enable": "true", "tsserve.service": "web", "tsserve.port": "80"},
			wantErr: "must start with",
		},
		{
			name:    "missing port",
			labels:  map[string]string{"tsserve.enable": "true", "tsserve.service": "svc:x"},
			wantErr: "tsserve.port",
		},
		{
			name:    "invalid port",
			labels:  map[string]string{"tsserve.enable": "true", "tsserve.service": "svc:x", "tsserve.port": "abc"},
			wantErr: "valid port",
		},
		{
			name:    "zero port",
			labels:  map[string]string{"tsserve.enable": "true", "tsserve.service": "svc:x", "tsserve.port": "0"},
			wantErr: "valid port",
		},
		{
			name: "invalid scheme",
			labels: map[string]string{
				"tsserve.enable": "true", "tsserve.service": "svc:x", "tsserve.port": "8080", "tsserve.scheme": "tcp",
			},
			wantErr: "'http' or 'https'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.labels)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("want error containing %q, got nil; def=%+v", tt.wantErr, got)
				}
				if tt.wantErr == "tsserve.enable" {
					if !errors.Is(err, ErrDisabled) {
						t.Fatalf("want ErrDisabled, got %v", err)
					}
					return
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}
