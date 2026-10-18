package upstream

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/AdguardTeam/dnsproxy/upstream"
	"github.com/miekg/dns"
)

// exchangeWithBudget 在 budget 内完成一次上游交换。
//
// dnsproxy 的明文上游在遇到网络错误（含超时）时会丢开调用方的 deadline，用一次
// 全新的 Timeout 重试，因此只靠 context 无法保证对外承诺的 timeout_ms。这里用
// goroutine + 定时器在外层强制预算：到点立即返回 DeadlineExceeded，未完成的
// 交换由库自身的超时收尾（耗时被库的尝试次数限界，不会无限增长）。
//
// budget <= 0 表示不限制，直接透传。
func exchangeWithBudget(
	ctx context.Context,
	resolver upstream.Upstream,
	req *dns.Msg,
	budget time.Duration,
) (*dns.Msg, error) {
	if budget <= 0 {
		return resolver.Exchange(ctx, req)
	}

	// 已取消的查询不发起真实交换：dnsproxy 重拨时会丢弃 context 取消信号，
	// 提前返回可以避免为必然失败的请求占用 socket。
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	type outcome struct {
		resp *dns.Msg
		err  error
	}

	// 带缓冲：即使本函数已超时返回，库的交换协程也不会阻塞在发送上。
	done := make(chan outcome, 1)
	go func() {
		resp, err := resolver.Exchange(ctx, req)
		done <- outcome{resp: resp, err: err}
	}()

	select {
	case out := <-done:
		return out.resp, out.err
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, ctx.Err()
		}

		return nil, fmt.Errorf("上游交换超过 %s: %w", budget, ctx.Err())
	}
}
