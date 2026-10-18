package dnsname

import (
	"strings"
	"testing"
)

func TestCanon(t *testing.T) {
	t.Parallel()

	validMaxName := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." +
		strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)
	tooLongName := validMaxName + "e"

	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "小写", in: "Example.COM", want: "example.com"},
		{name: "尾点", in: "example.com.", want: "example.com"},
		{name: "大小写与尾点", in: "API.Corp.Local.", want: "api.corp.local"},
		{name: "IDN", in: "bücher.example", want: "xn--bcher-kva.example"},
		{name: "IDN 大写", in: "BÜCHER.Example.", want: "xn--bcher-kva.example"},
		{name: "单标签", in: "local", want: "local"},
		{name: "根域名", in: ".", wantErr: true},
		{name: "空域名", in: "", wantErr: true},
		{name: "双尾点", in: "example.com..", wantErr: true},
		{name: "空标签", in: "a..b", wantErr: true},
		{name: "下划线", in: "_dmarc.example.com", wantErr: true},
		{name: "超长标签", in: strings.Repeat("a", 64) + ".com", wantErr: true},
		{name: "超长域名", in: strings.Repeat("a.", 127) + "com", wantErr: true},
		// IDNA 会把全角句号/句点映射为「.」且保留结尾点，归一必须发生在映射之后。
		{name: "全角句号结尾", in: "example.com。", want: "example.com"},
		{name: "全角句点结尾", in: "example.com．", want: "example.com"},
		{name: "全角句号与 IDN", in: "例え.テスト。", want: "xn--r8jz45g.xn--zckzah"},
		{name: "仅全角句号", in: "。", wantErr: true},
		{name: "253 字节", in: validMaxName, want: validMaxName},
		{name: "254 字节", in: tooLongName, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := Canon(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Canon(%q) = %q, 期望错误", tt.in, got)
				}

				return
			}

			if err != nil {
				t.Fatalf("Canon(%q) 返回错误: %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("Canon(%q) = %q, 期望 %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCanonQuery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "小写", in: "API.Corp.Local.", want: "api.corp.local"},
		{name: "无尾点", in: "api.corp.local", want: "api.corp.local"},
		{name: "下划线", in: "_dmarc.example.com.", want: "_dmarc.example.com"},
		{name: "反向名", in: "10.1.168.192.in-addr.arpa.", want: "10.1.168.192.in-addr.arpa"},
		{name: "空域名", in: "", wantErr: true},
		{name: "根域名", in: ".", wantErr: true},
		{name: "空标签", in: "a..b.", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := CanonQuery(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("CanonQuery(%q) = %q, 期望错误", tt.in, got)
				}

				return
			}

			if err != nil {
				t.Fatalf("CanonQuery(%q) 返回错误: %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("CanonQuery(%q) = %q, 期望 %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestIsEqualOrSubdomain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		domain string
		suffix string
		want   bool
	}{
		{name: "相等", domain: "example.com", suffix: "example.com", want: true},
		{name: "子域", domain: "www.example.com", suffix: "example.com", want: true},
		{name: "多级子域", domain: "a.b.example.com", suffix: "example.com", want: true},
		{name: "后缀非边界", domain: "notexample.com", suffix: "example.com", want: false},
		{name: "相似后缀", domain: "example.com.cn", suffix: "example.com", want: false},
		{name: "反向不成立", domain: "example.com", suffix: "www.example.com", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := IsEqualOrSubdomain(tt.domain, tt.suffix); got != tt.want {
				t.Fatalf("IsEqualOrSubdomain(%q, %q) = %v, 期望 %v", tt.domain, tt.suffix, got, tt.want)
			}
		})
	}
}

func TestLabelCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want int
	}{
		{in: "", want: 0},
		{in: "local", want: 1},
		{in: "example.com", want: 2},
		{in: "a.b.example.com", want: 4},
	}

	for _, tt := range tests {
		if got := LabelCount(tt.in); got != tt.want {
			t.Fatalf("LabelCount(%q) = %d, 期望 %d", tt.in, got, tt.want)
		}
	}
}

func TestFQDN(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want string
	}{
		{in: "", want: "."},
		{in: ".", want: "."},
		{in: "example.com", want: "example.com."},
		{in: "Example.COM", want: "example.com."},
		{in: "example.com.", want: "example.com."},
	}

	for _, tt := range tests {
		if got := FQDN(tt.in); got != tt.want {
			t.Fatalf("FQDN(%q) = %q, 期望 %q", tt.in, got, tt.want)
		}
	}
}
