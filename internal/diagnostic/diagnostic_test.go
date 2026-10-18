package diagnostic

import "testing"

func TestHasError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		diags []Diagnostic
		want  bool
	}{
		{name: "空", diags: nil, want: false},
		{name: "仅警告", diags: []Diagnostic{{Severity: SeverityWarning}}, want: false},
		{name: "含错误", diags: []Diagnostic{{Severity: SeverityWarning}, {Severity: SeverityError}}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := HasError(tt.diags); got != tt.want {
				t.Fatalf("HasError() = %v, 期望 %v", got, tt.want)
			}
		})
	}
}

func TestFilter(t *testing.T) {
	t.Parallel()

	diags := []Diagnostic{
		{Code: CodeInvalidTOML, Severity: SeverityError},
		{Code: CodeShadowedRule, Severity: SeverityWarning},
	}

	if got := Filter(diags, SeverityError); len(got) != 1 || got[0].Code != CodeInvalidTOML {
		t.Fatalf("Filter(error) = %+v", got)
	}
	if got := Filter(diags, ""); len(got) != 2 {
		t.Fatalf("Filter(空) 数量 = %d, 期望 2", len(got))
	}
}
