package dnskernel

import (
	"github.com/miekg/dns"
)

// stripECS 返回 req 的副本，并移除 EDNS Client Subnet 选项。
//
// dnsproxy 的 Config.EnableEDNSClientSubnet=false 只表示「不主动添加 ECS」，
// 不会移除客户端请求里已有的 ECS。本内核默认剥离 ECS、只透传 DO 位，
// 因此在进入上游组前由本层显式剥离。
//
// 其余 EDNS 选项（如 COOKIE）与 DO 位保持不变。
func stripECS(req *dns.Msg) (stripped *dns.Msg) {
	if req == nil {
		return nil
	}

	stripped = req.Copy()
	opt := stripped.IsEdns0()
	if opt == nil {
		return stripped
	}

	filtered := make([]dns.EDNS0, 0, len(opt.Option))
	for _, o := range opt.Option {
		if o.Option() == dns.EDNS0SUBNET {
			continue
		}

		filtered = append(filtered, o)
	}
	opt.Option = filtered

	return stripped
}
