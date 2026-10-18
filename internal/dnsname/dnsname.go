// Package dnsname 提供配置侧与查询侧的域名归一化与校验。
//
// 配置保存用户原文，本包只在加载、匹配与校验时构造规范域名，不写回配置。
package dnsname

import (
	"errors"
	"fmt"
	"strings"

	"github.com/miekg/dns"
	"golang.org/x/net/idna"
)

// profile 是配置侧的 IDNA2008（非过渡）转换 profile。
//
// Transitional(false) 保留 ß 等字符的 IDNA2008 语义；BidiRule 与
// StrictDomainName 用于拒绝不可注册的标签；长度在 validate 中统一校验。
var profile = idna.New(
	idna.MapForLookup(),
	idna.Transitional(false),
	idna.StrictDomainName(true),
	idna.BidiRule(),
)

// Canon 返回配置侧规范域名：IDNA2008 转 A-label、小写、去一个结尾点。
//
// 用于规则 domain 与本地记录 name；失败返回错误（配置校验时使用）。
func Canon(name string) (string, error) {
	if name == "" {
		return "", errors.New("域名为空")
	}

	// IDNA 映射会新产生结尾点（如「。」「．」→「.」）并保留原结尾点，因此
	// 去尾点必须发生在 IDNA 之后；否则规范域名会带尾点，规则匹配与重复检测
	// 都会失效。
	ascii, err := profile.ToASCII(name)
	if err != nil {
		return "", fmt.Errorf("IDNA 转换失败: %w", err)
	}

	return normalizeASCIIName(ascii)
}

// CanonQuery 返回查询侧规范域名：仅小写与去一个结尾点。
//
// 查询名来自 DNS 报文，已是 ASCII/A-label，不做 IDNA 转换。
func CanonQuery(name string) (string, error) {
	if name == "" {
		return "", errors.New("域名为空")
	}

	return normalizeASCIIName(name)
}

// normalizeASCIIName 把已是 ASCII 的域名归一为去尾点的规范域名并校验。
func normalizeASCIIName(name string) (string, error) {
	canon := strings.TrimSuffix(strings.ToLower(name), ".")

	// 去一个结尾点后仍为空说明输入是根域名；仍以点结尾说明存在空标签。
	if canon == "" {
		return "", errors.New("根域名不能作为域名")
	}
	if strings.HasSuffix(canon, ".") {
		return "", errors.New("域名包含空标签")
	}

	if err := validate(canon); err != nil {
		return "", err
	}

	return canon, nil
}

// IsEqualOrSubdomain 判断 name 是否等于 suffix 或为 suffix 的子域。
//
// 两个参数都必须是规范域名；按完整标签比较，能正确处理转义点。
func IsEqualOrSubdomain(name, suffix string) bool {
	if name == "" || suffix == "" {
		return false
	}

	return dns.IsSubDomain(suffix, name)
}

// LabelCount 返回规范域名的标签数量。
func LabelCount(name string) int {
	if name == "" {
		return 0
	}

	return dns.CountLabel(name)
}

// FQDN 返回小写、带一个结尾点的域名。
//
// 用于事件的 domain 字段等需要带结尾点的场景；匹配与配置比较仍使用去尾点
// 的规范域名。
func FQDN(name string) string {
	return dns.Fqdn(strings.ToLower(name))
}

// Suffixes 返回规范域名的全部标签后缀，从完整域名开始。
//
// 例如 "a.b.example.com" 返回 ["a.b.example.com", "b.example.com",
// "example.com", "com"]；按标签切分，能正确处理转义点。
func Suffixes(name string) []string {
	labels := dns.SplitDomainName(name)
	if len(labels) == 0 {
		return nil
	}

	out := make([]string, 0, len(labels))
	for start := range labels {
		out = append(out, strings.Join(labels[start:], "."))
	}

	return out
}

// maxDomainNameTextOctets 是去尾点规范域名的文本长度上限（对应 DNS 名字
// 255 字节 wire 长度上限；文本表示比 wire 少两个字节）。
const maxDomainNameTextOctets = 253

// validate 校验规范域名的长度与标签。
//
// dns.IsDomainName 负责标签结构与各标签长度；总长单独限制为 253 字节，
// 因为该辅助函数对总长的判定比 DNS 规范宽松（254/255 字节的域名会通过）。
func validate(name string) error {
	if _, ok := dns.IsDomainName(name); !ok {
		return errors.New("非法域名")
	}
	if len(name) > maxDomainNameTextOctets {
		return fmt.Errorf("域名总长 %d 字节超过 %d 字节上限", len(name), maxDomainNameTextOctets)
	}

	return nil
}
