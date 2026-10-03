package gateway

import (
	"testing"

	"github.com/zlogic-labs/fleet/core/pkg/billing"
)

func TestAMetricLabelCarriesTheProjectNameNotItsId(t *testing.T) {
	cases := []struct {
		name string
		rec  billing.Record
		want string
	}{
		{
			name: "a qualified project loses the tenant",
			rec:  billing.Record{Tenant: "acme", Project: "acme/research"},
			want: "research",
		},
		{
			name: "a bare project is left alone",
			rec:  billing.Record{Tenant: "acme", Project: "research"},
			want: "research",
		},
		{
			// An unauthenticated gateway has no tenant to strip, and a project
			// id without one is the whole value.
			name: "no tenant keeps the whole value",
			rec:  billing.Record{Project: "acme/research"},
			want: "acme/research",
		},
		{
			name: "a tenant sharing the project's name is not stripped",
			rec:  billing.Record{Tenant: "research", Project: "research"},
			want: "research",
		},
		{
			name: "a project named like another tenant is untouched",
			rec:  billing.Record{Tenant: "acme", Project: "globex/chat"},
			want: "globex/chat",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := projectName(tc.rec); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
