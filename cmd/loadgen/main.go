// Command loadgen 是开发/压测用的 DNS 负载生成器。
//
// 它按固定 QPS 向目标 DNS 服务器发送 A 查询并输出 JSON 统计（发送量、成功率、
// 延迟分位）。-selftest 会在进程内启动 mockdns 上游并压测它，用于验证工具本身
// 能达到 1,000 QPS，不依赖外部环境。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/CedarHuang/Namesplit/internal/mockdns"
)

// benchResult 是一次压测的统计结果。
type benchResult struct {
	Target       string    `json:"target"`
	QPSTarget    int       `json:"qps_target"`
	Concurrency  int       `json:"concurrency"`
	DurationMS   int64     `json:"duration_ms"`
	SendWindowMS int64     `json:"send_window_ms"`
	Sent         int64     `json:"sent"`
	OK           int64     `json:"ok"`
	Failed       int64     `json:"failed"`
	AchievedQPS  float64   `json:"achieved_qps"`
	LatencyMS    latencies `json:"latency_ms"`
}

// latencies 是延迟分位（毫秒）。
type latencies struct {
	P50  float64 `json:"p50"`
	P95  float64 `json:"p95"`
	P99  float64 `json:"p99"`
	Max  float64 `json:"max"`
	Avg  float64 `json:"avg"`
	Note string  `json:"note,omitempty"`
}

func main() {
	addr := flag.String("addr", "", "目标 DNS 地址（host:port）")
	qps := flag.Int("qps", 1000, "目标 QPS")
	duration := flag.Duration("duration", 5*time.Second, "持续时间")
	concurrency := flag.Int("concurrency", 64, "并发工作协程数")
	selftest := flag.Bool("selftest", false, "启动进程内 mock 上游并压测它")
	flag.Parse()

	if *qps <= 0 || *duration <= 0 || *concurrency <= 0 {
		fmt.Fprintln(os.Stderr, "qps、duration、concurrency 必须为正数")
		os.Exit(2)
	}

	ctx := context.Background()
	target := *addr

	if *selftest {
		mock, err := mockdns.StartPlain(mockdns.PlainConfig{A: []byte{192, 0, 2, 1}})
		if err != nil {
			fmt.Fprintf(os.Stderr, "启动自测 mock: %v\n", err)
			os.Exit(1)
		}
		defer mock.Close()

		target = mock.Addr()
	}

	if target == "" {
		fmt.Fprintln(os.Stderr, "必须提供 -addr，或使用 -selftest")
		os.Exit(2)
	}

	res := run(ctx, target, *qps, *duration, *concurrency)
	res.NoteIfSelftest(*selftest)

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		fmt.Fprintf(os.Stderr, "输出统计: %v\n", err)
		os.Exit(1)
	}

	if res.Failed > 0 {
		os.Exit(1)
	}
}

// run 执行压测并汇总结果。
func run(ctx context.Context, target string, qps int, duration time.Duration, workers int) benchResult {
	start := time.Now()

	jobs := make(chan uint64, workers)
	var wg sync.WaitGroup
	var sent, ok, failed int64
	var mu sync.Mutex
	var latenciesMS []float64

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			client := &dns.Client{Net: "udp", Timeout: 2 * time.Second}

			for seq := range jobs {
				req := new(dns.Msg)
				req.SetQuestion(fmt.Sprintf("load-%d.namesplit.test.", seq), dns.TypeA)

				begin := time.Now()
				resp, _, err := client.Exchange(req, target)
				elapsed := time.Since(begin)

				// 只有收到 NOERROR 应答才算成功；传输错误和非 NOERROR 都计入失败。
				if err != nil || resp == nil || resp.Rcode != dns.RcodeSuccess {
					mu.Lock()
					failed++
					mu.Unlock()

					continue
				}

				mu.Lock()
				ok++
				latenciesMS = append(latenciesMS, float64(elapsed)/float64(time.Millisecond))
				mu.Unlock()
			}
		}()
	}

	deadline := time.NewTimer(duration)
	defer deadline.Stop()

	interval := time.Duration(float64(time.Second) / float64(qps))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var seq uint64
dispatch:
	for {
		select {
		case <-ctx.Done():
			break dispatch
		case <-deadline.C:
			break dispatch
		case <-ticker.C:
			select {
			case jobs <- seq:
				mu.Lock()
				sent++
				mu.Unlock()
				seq++
			case <-deadline.C:
				break dispatch
			}
		}
	}
	sendEnd := time.Now()

	close(jobs)
	wg.Wait()

	elapsed := time.Since(start)
	sendWindow := sendEnd.Sub(start)
	mu.Lock()
	defer mu.Unlock()

	sort.Float64s(latenciesMS)

	return benchResult{
		Target:       target,
		QPSTarget:    qps,
		Concurrency:  workers,
		DurationMS:   elapsed.Milliseconds(),
		SendWindowMS: sendWindow.Milliseconds(),
		Sent:         sent,
		OK:           ok,
		Failed:       failed,
		AchievedQPS:  float64(ok) / sendWindow.Seconds(),
		LatencyMS:    percentiles(latenciesMS),
	}
}

// percentiles 计算延迟分位。
func percentiles(values []float64) latencies {
	if len(values) == 0 {
		return latencies{Note: "无成功样本"}
	}

	at := func(p float64) float64 {
		idx := int(p * float64(len(values)-1))
		if idx < 0 {
			return 0
		}
		if idx >= len(values) {
			return values[len(values)-1]
		}

		return values[idx]
	}

	var sum float64
	for _, v := range values {
		sum += v
	}

	return latencies{
		P50: at(0.50),
		P95: at(0.95),
		P99: at(0.99),
		Max: values[len(values)-1],
		Avg: sum / float64(len(values)),
	}
}

// NoteIfSelftest 为自测结果补充说明。
func (r *benchResult) NoteIfSelftest(selftest bool) {
	if selftest {
		r.LatencyMS.Note = "进程内 mock UDP 上游，仅验证压测器能力"
	}
}
