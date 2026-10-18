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

// stripOPT 移除应答附加段中的 OPT 伪 RR。
//
// 响应不携带 OPT，也不回显 DO；上游应答与库缓存仍可能残留 OPT，且事件的
// additional 会把 OPT 渲染成多行伪段落，因此在内核出口统一剥离，不依赖
// 上游行为。
func stripOPT(msg *dns.Msg) {
	if msg == nil || len(msg.Extra) == 0 {
		return
	}

	filtered := make([]dns.RR, 0, len(msg.Extra))
	for _, rr := range msg.Extra {
		if rr.Header().Rrtype == dns.TypeOPT {
			continue
		}

		filtered = append(filtered, rr)
	}
	msg.Extra = filtered
}
